package service_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/caching"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/paprikaclient/fakepaprika"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/service"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/testsupport"
)

const (
	typeBreakfast = "MT-1"
	typeDinner    = "MT-3"
	listMain      = "LIST-MAIN"
	listParty     = "LIST-PARTY"
)

func seed(f *fakepaprika.Server) {
	f.Seed(
		"categories",
		fakepaprika.SampleCategory("CAT-DINNER", "Dinner"),
		fakepaprika.SampleCategory("CAT-SOUP", "Soups"),
		withParent(fakepaprika.SampleCategory("CAT-STEW", "Stews"), "CAT-DINNER"),
		withParent(fakepaprika.SampleCategory("CAT-CHILI", "Chili"), "CAT-STEW"),
		fakepaprika.SampleCategory("CAT-DUP1", "Twin"),
		fakepaprika.SampleCategory("CAT-DUP2", "twin"),
	)
	f.SeedRecipe(testsupport.Recipe("R1", "Tomato Soup", "tomatoes\ncream", "CAT-SOUP"))
	f.SeedRecipe(testsupport.Recipe("R2", "Beef Chili", "beef\nbeans", "CAT-CHILI"))
	trashed := testsupport.Recipe("R3", "Old Soup", "water", "CAT-SOUP")
	trashed["in_trash"] = true
	f.SeedRecipe(trashed)
	for i := range 7 {
		f.SeedRecipe(testsupport.Recipe(fmt.Sprintf("P%d", i), fmt.Sprintf("Pasta %d", i), "pasta"))
	}

	f.Seed(
		"mealtypes",
		fakepaprika.SampleMealType(typeDinner, "Dinner", 2),
		fakepaprika.SampleMealType(typeBreakfast, "Breakfast", 0),
	)
	linked := fakepaprika.SampleMeal("M-LINK", "Soup night", "2030-03-10 00:00:00", typeDinner)
	linked["recipe_uid"] = "R1"
	f.Seed(
		"meals",
		linked,
		fakepaprika.SampleMeal("M-B", "Pancakes", "2030-03-10 00:00:00", typeBreakfast),
		fakepaprika.SampleMeal("M-EARLY", "Early", "2030-03-01 00:00:00", typeDinner),
		fakepaprika.SampleMeal("M-LATE", "Late", "2030-03-31 00:00:00", typeDinner),
	)

	f.Seed(
		"groceryaisles",
		fakepaprika.SampleAisle("AISLE-1", "Produce"),
		fakepaprika.SampleAisle("AISLE-2", "Dairy"),
	)
	f.Seed(
		"grocerylists",
		fakepaprika.SampleGroceryList(listParty, "Party", false),
		fakepaprika.SampleGroceryList(listMain, "Groceries", true),
	)
	bought := fakepaprika.SampleGrocery("G-BOUGHT", "milk", listMain, "AISLE-2")
	bought["purchased"] = true
	bought["aisle"] = ""
	f.Seed(
		"groceries",
		fakepaprika.SampleGrocery("G-1", "carrots", listMain, "AISLE-1"),
		fakepaprika.SampleGrocery("G-2", "lettuce", listMain, "AISLE-1"),
		bought,
		fakepaprika.SampleGrocery("G-3", "balloons", listParty, "AISLE-1"),
	)
}

func withParent(c map[string]any, parent string) map[string]any {
	c["parent_uid"] = parent
	return c
}

func newService(t *testing.T) (*service.Service, *testsupport.Stack) {
	t.Helper()
	st := testsupport.NewStack(t, nil, seed)
	return st.Service, st
}

func kind(t *testing.T, err error) service.Kind {
	t.Helper()
	var se *service.Error
	if !errors.As(err, &se) {
		t.Fatalf("error %v is not a *service.Error", err)
	}
	return se.Kind
}

func TestSearchRecipes(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()
	yes := true
	tests := []struct {
		name string
		p    service.SearchParams
		want []string
	}{
		{"by name", service.SearchParams{Query: "tomato"}, []string{"R1"}},
		{"category by name", service.SearchParams{Category: "soups"}, []string{"R1"}},
		{"category by uid", service.SearchParams{Category: "CAT-CHILI"}, []string{"R2"}},
		{"trashed hidden", service.SearchParams{Query: "soup"}, []string{"R1"}},
		{
			"trashed shown",
			service.SearchParams{Query: "soup", IncludeTrashed: true},
			[]string{"R3", "R1"},
		},
		{"favorite", service.SearchParams{Favorite: &yes}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			page, err := svc.SearchRecipes(ctx, tt.p)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, r := range page.Items {
				got = append(got, r.UID)
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}

	page, err := svc.SearchRecipes(ctx, service.SearchParams{Query: "tomato"})
	if err != nil {
		t.Fatal(err)
	}
	if got := page.Items[0].Categories; len(got) != 1 || got[0] != "Soups" {
		t.Errorf("category names = %v", got)
	}
}

func TestSearchRecipesValidation(t *testing.T) {
	svc, _ := newService(t)
	tests := []struct {
		name     string
		p        service.SearchParams
		wantKind service.Kind
		wantText string
	}{
		{
			"unknown category names the value",
			service.SearchParams{Category: "Nonexistent"},
			service.KindNotFound,
			`"Nonexistent"`,
		},
		{"ambiguous category", service.SearchParams{Category: "twin"}, service.KindInvalid, "uid"},
		{"rating too high", service.SearchParams{MinRating: 6}, service.KindInvalid, "min_rating"},
		{"rating negative", service.SearchParams{MinRating: -1}, service.KindInvalid, "min_rating"},
		{
			"query too long",
			service.SearchParams{Query: strings.Repeat("a", 201)},
			service.KindInvalid,
			"query",
		},
		{"bad cursor", service.SearchParams{Cursor: "garbage"}, service.KindInvalid, "cursor"},
		{
			"cursor past the end",
			service.SearchParams{Cursor: "bzE6OTk5"},
			service.KindInvalid,
			"cursor",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.SearchRecipes(context.Background(), tt.p)
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := kind(t, err); got != tt.wantKind {
				t.Errorf("kind = %v, want %v (%v)", got, tt.wantKind, err)
			}
			if !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("message %q lacks %q", err.Error(), tt.wantText)
			}
		})
	}
}

func TestPagination(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()
	var seen []string
	cursor := ""
	pages := 0
	for {
		page, err := svc.SearchRecipes(
			ctx,
			service.SearchParams{Query: "pasta", Limit: 3, Cursor: cursor},
		)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if page.Total != 7 {
			t.Errorf("total = %d, want 7", page.Total)
		}
		for _, r := range page.Items {
			seen = append(seen, r.UID)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if pages != 3 || len(seen) != 7 {
		t.Errorf("pages = %d, items = %d; want 3 pages and 7 items", pages, len(seen))
	}
	uniq := map[string]bool{}
	for _, s := range seen {
		uniq[s] = true
	}
	if len(uniq) != 7 {
		t.Errorf("pages overlapped: %v", seen)
	}

	page, err := svc.SearchRecipes(ctx, service.SearchParams{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) > service.MaxLimit {
		t.Errorf("limit not clamped: %d items", len(page.Items))
	}
}

func TestGetRecipe(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()
	r, err := svc.GetRecipe(ctx, "R1")
	if err != nil {
		t.Fatal(err)
	}
	if r.Name != "Tomato Soup" || r.Hash == "" || r.Ingredients != "tomatoes\ncream" {
		t.Errorf("recipe = %+v", r)
	}
	if len(r.Categories) != 1 || r.Categories[0].Name != "Soups" {
		t.Errorf("categories = %+v", r.Categories)
	}

	_, err = svc.GetRecipe(ctx, "NOPE")
	if kind(t, err) != service.KindNotFound || !strings.Contains(err.Error(), `"NOPE"`) {
		t.Errorf("unknown uid error = %v", err)
	}
	if _, err = svc.GetRecipe(ctx, "  "); kind(t, err) != service.KindInvalid {
		t.Errorf("blank uid error = %v", err)
	}
}

func TestListCategoriesTree(t *testing.T) {
	svc, _ := newService(t)
	page, err := svc.ListCategories(context.Background(), 0, "")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range page.Items {
		got = append(got, fmt.Sprintf("%d:%s", c.Depth, c.Path))
	}
	want := []string{
		"0:Dinner",
		"1:Dinner > Stews",
		"2:Dinner > Stews > Chili",
		"0:Soups",
		"0:Twin",
		"0:twin",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("tree = %v, want %v", got, want)
	}
}

func TestListMeals(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()
	page, err := svc.ListMeals(ctx, "2030-03-05", "2030-03-31", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range page.Items {
		got = append(got, m.UID+":"+m.Date+":"+m.MealType)
	}
	want := []string{
		"M-B:2030-03-10:Breakfast",
		"M-LINK:2030-03-10:Dinner",
		"M-LATE:2030-03-31:Dinner",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("meals = %v, want %v", got, want)
	}
	if page.Items[1].RecipeUID != "R1" || page.Items[1].RecipeName != "Tomato Soup" {
		t.Errorf("linked meal = %+v", page.Items[1])
	}

	tests := []struct {
		name       string
		start, end string
		wantText   string
	}{
		{"start required", "", "2030-03-01", "start_date"},
		{"end required", "2030-03-01", "", "end_date"},
		{"wrong format", "03/01/2030", "2030-03-02", "YYYY-MM-DD"},
		{"impossible date", "2030-02-30", "2030-03-02", "start_date"},
		{"reversed", "2030-03-05", "2030-03-01", "before"},
		{"too long", "2030-01-01", "2031-06-01", "too long"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.ListMeals(ctx, tt.start, tt.end, 0, "")
			if err == nil || kind(
				t,
				err,
			) != service.KindInvalid || !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("error = %v, want invalid mentioning %q", err, tt.wantText)
			}
		})
	}
}

func TestListMealTypes(t *testing.T) {
	svc, _ := newService(t)
	page, err := svc.ListMealTypes(context.Background(), 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Items[0].Name != "Breakfast" || page.Items[1].Name != "Dinner" {
		t.Errorf("meal types = %+v", page.Items)
	}
}

func TestGroceries(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()

	lists, err := svc.ListGroceryLists(ctx, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]service.GroceryListSummary{}
	for _, l := range lists.Items {
		got[l.UID] = l
	}
	if l := got[listMain]; l.ItemCount != 3 || l.UnpurchasedCount != 2 || !l.IsDefault {
		t.Errorf("main list = %+v", l)
	}
	if l := got[listParty]; l.ItemCount != 1 || l.UnpurchasedCount != 1 {
		t.Errorf("party list = %+v", l)
	}

	tests := []struct {
		name     string
		list     string
		purchase bool
		want     []string
		wantList string
	}{
		{"default list hides purchased", "", false, []string{"carrots", "lettuce"}, "Groceries"},
		{"include purchased", "", true, []string{"milk", "carrots", "lettuce"}, "Groceries"},
		{"by name", "party", false, []string{"balloons"}, "Party"},
		{"by uid", listParty, false, []string{"balloons"}, "Party"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := svc.ListGroceryItems(ctx, tt.list, tt.purchase, 0, "")
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, it := range res.Items {
				names = append(names, it.Name)
			}
			if strings.Join(
				names,
				",",
			) != strings.Join(tt.want, ",") || res.ListName != tt.wantList {
				t.Errorf(
					"items = %v in %q, want %v in %q",
					names,
					res.ListName,
					tt.want,
					tt.wantList,
				)
			}
		})
	}
	// An item with no aisle text shows the aisle named by its aisle_uid.
	res, _ := svc.ListGroceryItems(ctx, "", true, 0, "")
	if res.Items[0].Name != "milk" || res.Items[0].Aisle != "Dairy" {
		t.Errorf("aisle fallback: %+v", res.Items[0])
	}

	if _, err := svc.ListGroceryItems(
		ctx,
		"Nope",
		false,
		0,
		"",
	); kind(t, err) != service.KindNotFound || !strings.Contains(err.Error(), `"Nope"`) {
		t.Errorf("unknown list error = %v", err)
	}

	aisles, err := svc.ListGroceryAisles(ctx, 0, "")
	if err != nil || len(aisles.Items) != 2 {
		t.Fatalf("aisles = %+v, %v", aisles, err)
	}
}

func TestErrorMapping(t *testing.T) {
	tests := []struct {
		name     string
		prepare  func(st *testsupport.Stack)
		wantKind service.Kind
		wantText string
		banned   []string
	}{
		{
			name: "paprika unavailable",
			prepare: func(st *testsupport.Stack) {
				st.Fake.Inject(fakepaprika.Fault{
					Method: "GET",
					Path:   "/api/v2/sync/recipe/R1/",
					Times:  -1,
					Kind:   fakepaprika.FaultUnavailable,
				})
			},
			wantKind: service.KindTemporary,
			wantText: "temporarily unavailable",
		},
		{
			name: "login failing is a configuration problem without detail",
			prepare: func(st *testsupport.Stack) {
				st.Fake.SetPassword("changed-elsewhere")
				st.Fake.RevokeTokens()
			},
			wantKind: service.KindConfig,
			wantText: "server configuration problem",
			banned:   []string{"password", "401", "changed-elsewhere", "cook@example.test"},
		},
		{
			name: "upstream error text never reaches the client",
			prepare: func(st *testsupport.Stack) {
				st.Fake.Inject(fakepaprika.Fault{
					Method:  "GET",
					Path:    "/api/v2/sync/recipe/R1/",
					Times:   -1,
					Kind:    fakepaprika.FaultEnvelopeError,
					Code:    77,
					Message: "SECRET-UPSTREAM-DETAIL",
				})
			},
			wantKind: service.KindTemporary,
			wantText: "unusable response",
			banned:   []string{"SECRET-UPSTREAM-DETAIL", "77"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := testsupport.NewStack(t, nil, seed)
			tt.prepare(st)
			// R1 is indexed but its body is not cached, so this goes upstream.
			_, err := st.Service.GetRecipe(context.Background(), "R1")
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := kind(t, err); got != tt.wantKind {
				t.Errorf("kind = %v, want %v (%v)", got, tt.wantKind, err)
			}
			if !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("message %q lacks %q", err.Error(), tt.wantText)
			}
			for _, b := range tt.banned {
				if strings.Contains(strings.ToLower(err.Error()), strings.ToLower(b)) {
					t.Errorf("message %q leaks %q", err.Error(), b)
				}
			}
		})
	}
}

func TestStillLoadingError(t *testing.T) {
	fake := fakepaprika.New(t)
	seed(fake)
	cache := caching.New(testsupport.Client(t, fake, nil), caching.Options{})
	svc := service.New(cache, nil)
	_, err := svc.SearchRecipes(context.Background(), service.SearchParams{Query: "soup"})
	if err == nil || kind(
		t,
		err,
	) != service.KindLoading || !strings.Contains(err.Error(), "still loading") {
		t.Fatalf("error = %v, want a loading error", err)
	}
	// get_recipe is served from the API meanwhile.
	if _, err := svc.GetRecipe(context.Background(), "R1"); err != nil {
		t.Errorf("GetRecipe while loading: %v", err)
	}
}

func TestStillLoadingMessageShowsCumulativeProgress(t *testing.T) {
	ctx := context.Background()
	fake := fakepaprika.New(t)
	seed(fake)
	fake.Inject(fakepaprika.Fault{
		Method:  "GET",
		Path:    "/api/v2/sync/recipe/P3/",
		Kind:    fakepaprika.FaultStatus,
		Status:  500,
		Message: "boom",
	})
	cache := caching.New(testsupport.Client(t, fake, nil), caching.Options{})
	svc := service.New(cache, nil)
	if err := cache.Sync(ctx); err == nil {
		t.Fatal("expected the interrupted pass to fail")
	}
	p := cache.Progress()
	if p.Ready || p.Indexed == 0 || p.Total != 10 {
		t.Fatalf("progress = %+v, want a partial load of 10", p)
	}

	_, err := svc.SearchRecipes(ctx, service.SearchParams{Query: "soup"})
	want := fmt.Sprintf(
		"The recipe index is still loading: %d of 10 recipes read. Retry shortly.",
		p.Indexed,
	)
	if err == nil || kind(t, err) != service.KindLoading || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want it to contain %q", err, want)
	}
}
