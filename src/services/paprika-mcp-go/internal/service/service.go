// Package service holds the read operations behind the MCP tools: input
// validation, reference resolution, pagination, and mapping of cache and
// client errors to a small set of [Error] kinds. It knows nothing about MCP.
package service

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/caching"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/paprikaclient"
)

// Page sizes for list operations.
const (
	DefaultLimit = 25
	MaxLimit     = 100
)

const (
	maxQueryLen  = 200
	maxRefLen    = 200
	maxMealDays  = 366
	dateLayout   = "2006-01-02"
	cursorPrefix = "o1:"
)

// Service implements the read and write operations. It is safe for concurrent
// use.
type Service struct {
	cache *caching.Cache
	log   *slog.Logger

	// w is nil on a read-only service.
	w     Writer
	wlock chan struct{} // the process-wide write lock, held for a whole write
	now   func() time.Time
}

// Option configures [New].
type Option func(*Service)

// WithWriter lets the service write through w, normally a
// *paprikaclient.Client. Without it every write operation fails.
func WithWriter(w Writer) Option {
	return func(s *Service) { s.w = w }
}

// New returns a Service that reads through cache.
func New(cache *caching.Cache, logger *slog.Logger, opts ...Option) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Service{cache: cache, log: logger, wlock: make(chan struct{}, 1), now: time.Now}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Page is one page of a list result.
type Page[T any] struct {
	Items []T
	// Total is the number of items across all pages.
	Total int
	// NextCursor is empty on the last page.
	NextCursor string
}

// paginate returns the page of all selected by limit and cursor. The cursor is
// an opaque offset; results that change between calls can shift pages.
func paginate[T any](all []T, limit int, cursor string) (Page[T], error) {
	if limit <= 0 {
		limit = DefaultLimit
	}
	limit = min(limit, MaxLimit)
	offset := 0
	if cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		n, convErr := strconv.Atoi(strings.TrimPrefix(string(raw), cursorPrefix))
		if err != nil || convErr != nil || !strings.HasPrefix(
			string(raw),
			cursorPrefix,
		) || n < 0 || n > len(all) {
			return Page[T]{}, invalid("The cursor is not valid. Repeat the call without a cursor.")
		}
		offset = n
	}
	end := min(offset+limit, len(all))
	page := Page[T]{Items: append([]T{}, all[offset:end]...), Total: len(all)}
	if end < len(all) {
		page.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(cursorPrefix + strconv.Itoa(end)))
	}
	return page, nil
}

func checkText(field, value string, max int) error {
	if utf8.RuneCountInString(value) > max {
		return invalid("%s is too long; use at most %d characters.", field, max)
	}
	return nil
}

// RecipeSummary is a compact recipe row.
type RecipeSummary struct {
	UID        string
	Name       string
	Categories []string // category names
	Rating     int
	Favorite   bool
	InTrash    bool
	Source     string
	TotalTime  string
}

// SearchParams selects recipes. Category is a category uid or name.
type SearchParams struct {
	Query          string
	Category       string
	Favorite       *bool
	MinRating      int
	IncludeTrashed bool
	Limit          int
	Cursor         string
}

// SearchRecipes searches the recipe index.
func (s *Service) SearchRecipes(ctx context.Context, p SearchParams) (Page[RecipeSummary], error) {
	if err := checkText("query", p.Query, maxQueryLen); err != nil {
		return Page[RecipeSummary]{}, err
	}
	if err := checkText("category", p.Category, maxRefLen); err != nil {
		return Page[RecipeSummary]{}, err
	}
	if p.MinRating < 0 || p.MinRating > 5 {
		return Page[RecipeSummary]{}, invalid("min_rating must be between 0 and 5.")
	}

	q := caching.Query{
		Text:           p.Query,
		Favorite:       p.Favorite,
		MinRating:      p.MinRating,
		IncludeTrashed: p.IncludeTrashed,
	}
	cats, err := s.categoryNames(ctx)
	if p.Category != "" {
		if err != nil {
			return Page[RecipeSummary]{}, s.mapError("search_recipes", err)
		}
		uid, rerr := s.resolveCategory(ctx, p.Category)
		if rerr != nil {
			return Page[RecipeSummary]{}, rerr
		}
		q.CategoryUID = uid
	}

	hits, err := s.cache.Search(ctx, q)
	if err != nil {
		return Page[RecipeSummary]{}, s.mapError("search_recipes", err)
	}
	page, err := paginate(hits, p.Limit, p.Cursor)
	if err != nil {
		return Page[RecipeSummary]{}, err
	}
	out := Page[RecipeSummary]{
		Total:      page.Total,
		NextCursor: page.NextCursor,
		Items:      make([]RecipeSummary, 0, len(page.Items)),
	}
	for _, e := range page.Items {
		out.Items = append(out.Items, RecipeSummary{
			UID:        e.UID,
			Name:       e.Name,
			Categories: namesFor(e.Categories, cats),
			Rating:     e.Rating,
			Favorite:   e.Favorite,
			InTrash:    e.InTrash,
			Source:     e.Source,
			TotalTime:  e.TotalTime,
		})
	}
	return out, nil
}

// categoryNames returns category uid to name. An error means none are known.
func (s *Service) categoryNames(ctx context.Context) (map[string]string, error) {
	cats, err := s.cache.Categories(ctx)
	if err != nil {
		return nil, err
	}
	names := make(map[string]string, len(cats))
	for _, c := range cats {
		names[c.Value.UID] = c.Value.Name
	}
	return names, nil
}

func namesFor(uids []string, names map[string]string) []string {
	out := make([]string, 0, len(uids))
	for _, uid := range uids {
		if n, ok := names[uid]; ok {
			out = append(out, n)
		}
	}
	return out
}

// resolveCategory accepts a category uid or a case-insensitive name.
func (s *Service) resolveCategory(ctx context.Context, ref string) (string, error) {
	cats, err := s.cache.Categories(ctx)
	if err != nil {
		return "", s.mapError("resolve_category", err)
	}
	return resolveCategoryIn(cats, ref)
}

// resolveCategoryIn finds a category by uid, then by case-insensitive name.
func resolveCategoryIn(
	cats []paprikaclient.Entry[paprikaclient.Category],
	ref string,
) (string, error) {
	for _, c := range cats {
		if c.Value.UID == ref {
			return c.Value.UID, nil
		}
	}
	want := strings.ToLower(strings.TrimSpace(ref))
	var matches []string
	for _, c := range cats {
		if strings.ToLower(strings.TrimSpace(c.Value.Name)) == want {
			matches = append(matches, c.Value.UID)
		}
	}
	switch len(matches) {
	case 0:
		return "", notFound(
			"Unknown category %q. Call list_categories to see the valid names and uid values.",
			ref,
		)
	case 1:
		return matches[0], nil
	default:
		return "", invalid(
			"Several categories are named %q. Use the category uid from list_categories instead.",
			ref,
		)
	}
}

// CategoryRef names a category.
type CategoryRef struct {
	UID  string
	Name string
}

// RecipeDetail is a full recipe. The text fields come from the recipe and are
// untrusted: they may contain instructions aimed at a model.
type RecipeDetail struct {
	UID         string
	Hash        string
	Name        string
	Created     string
	Description string
	Ingredients string
	Directions  string
	Notes       string
	Source      string
	SourceURL   string
	Servings    string
	PrepTime    string
	CookTime    string
	TotalTime   string
	Rating      int
	Favorite    bool
	InTrash     bool
	Categories  []CategoryRef
}

// GetRecipe returns one full recipe by uid.
func (s *Service) GetRecipe(ctx context.Context, uid string) (RecipeDetail, error) {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return RecipeDetail{}, invalid("uid is required. Use search_recipes to find a recipe uid.")
	}
	if err := checkText("uid", uid, maxRefLen); err != nil {
		return RecipeDetail{}, err
	}
	entry, err := s.cache.Recipe(ctx, uid)
	if err != nil {
		if isNotFound(err) {
			return RecipeDetail{}, notFound(
				"No recipe has the uid %q. Use search_recipes to find a valid uid.",
				uid,
			)
		}
		return RecipeDetail{}, s.mapError("get_recipe", err)
	}
	return s.recipeDetail(ctx, entry.Value), nil
}

// recipeDetail converts a recipe, naming its categories from the cache.
func (s *Service) recipeDetail(ctx context.Context, r paprikaclient.Recipe) RecipeDetail {
	names, nerr := s.categoryNames(ctx)
	if nerr != nil {
		s.log.Warn("could not name recipe categories", "error", nerr)
	}
	cats := make([]CategoryRef, 0, len(r.Categories))
	for _, c := range r.Categories {
		cats = append(cats, CategoryRef{UID: c, Name: names[c]})
	}
	return RecipeDetail{
		UID:         r.UID,
		Hash:        r.Hash,
		Name:        r.Name,
		Created:     r.Created,
		Description: r.Description,
		Ingredients: r.Ingredients,
		Directions:  r.Directions,
		Notes:       r.Notes,
		Source:      r.Source,
		SourceURL:   r.SourceURL,
		Servings:    r.Servings,
		PrepTime:    r.PrepTime,
		CookTime:    r.CookTime,
		TotalTime:   r.TotalTime,
		Rating:      r.Rating,
		Favorite:    r.OnFavorites,
		InTrash:     r.InTrash,
		Categories:  cats,
	}
}

func isNotFound(err error) bool {
	return errors.Is(err, paprikaclient.ErrNotFound)
}

// CategoryNode is one category in the tree, listed depth first.
type CategoryNode struct {
	UID string
	// ParentUID is empty for a top-level category.
	ParentUID string
	Name      string
	// Depth is 0 for a top-level category.
	Depth int
	// Path is the names from the top level down, joined with " > ".
	Path string
}

// ListCategories returns the category tree flattened depth first: each
// category follows its parent, siblings in display order.
func (s *Service) ListCategories(
	ctx context.Context,
	limit int,
	cursor string,
) (Page[CategoryNode], error) {
	cats, err := s.cache.Categories(ctx)
	if err != nil {
		return Page[CategoryNode]{}, s.mapError("list_categories", err)
	}
	byParent := map[string][]paprikaclient.Category{}
	known := make(map[string]bool, len(cats))
	for _, c := range cats {
		known[c.Value.UID] = true
	}
	for _, c := range cats {
		parent := ""
		if c.Value.ParentUID != nil && known[*c.Value.ParentUID] && *c.Value.ParentUID != c.Value.UID {
			parent = *c.Value.ParentUID
		}
		byParent[parent] = append(byParent[parent], c.Value)
	}
	visited := map[string]bool{}
	all := make([]CategoryNode, 0, len(cats))
	var walk func(parent, parentPath string, depth int)
	walk = func(parent, parentPath string, depth int) {
		kids := byParent[parent]
		slices.SortStableFunc(kids, func(a, b paprikaclient.Category) int {
			if a.OrderFlag != b.OrderFlag {
				return a.OrderFlag - b.OrderFlag
			}
			return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
		})
		for _, k := range kids {
			if visited[k.UID] {
				continue
			}
			visited[k.UID] = true
			path := k.Name
			if parentPath != "" {
				path = parentPath + " > " + k.Name
			}
			all = append(
				all,
				CategoryNode{UID: k.UID, ParentUID: parent, Name: k.Name, Depth: depth, Path: path},
			)
			walk(k.UID, path, depth+1)
		}
	}
	walk("", "", 0)
	return paginate(all, limit, cursor)
}

// MealSummary is one planned meal.
type MealSummary struct {
	UID       string
	Date      string // YYYY-MM-DD
	MealType  string
	Name      string
	RecipeUID string
	// RecipeName is the indexed name of the linked recipe, when known.
	RecipeName string
}

// ListMeals returns planned meals from start through end, inclusive. Both are
// YYYY-MM-DD dates.
func (s *Service) ListMeals(
	ctx context.Context,
	start, end string,
	limit int,
	cursor string,
) (Page[MealSummary], error) {
	from, err := parseDate("start_date", start)
	if err != nil {
		return Page[MealSummary]{}, err
	}
	to, err := parseDate("end_date", end)
	if err != nil {
		return Page[MealSummary]{}, err
	}
	if to.Before(from) {
		return Page[MealSummary]{}, invalid("end_date must not be before start_date.")
	}
	if to.Sub(from) > (maxMealDays-1)*24*time.Hour {
		return Page[MealSummary]{}, invalid(
			"The date range is too long; ask for at most %d days at a time.",
			maxMealDays,
		)
	}

	meals, err := s.cache.Meals(ctx)
	if err != nil {
		return Page[MealSummary]{}, s.mapError("list_meals", err)
	}
	types, err := s.cache.MealTypes(ctx)
	if err != nil {
		return Page[MealSummary]{}, s.mapError("list_meals", err)
	}
	mts := newMealTypes(types)

	type row struct {
		sum      MealSummary
		typeSort int
		order    int
	}
	var rows []row
	lo, hi := from.Format(dateLayout), to.Format(dateLayout)
	for _, m := range meals {
		if len(m.Value.Date) < len(dateLayout) {
			continue
		}
		day := m.Value.Date[:len(dateLayout)]
		if day < lo || day > hi {
			continue
		}
		typeName, typeOrder := mts.forMeal(m.Value)
		sum := MealSummary{UID: m.Value.UID, Date: day, Name: m.Value.Name, MealType: typeName}
		if m.Value.RecipeUID != nil && *m.Value.RecipeUID != "" {
			sum.RecipeUID = *m.Value.RecipeUID
			sum.RecipeName = s.cache.RecipeName(sum.RecipeUID)
		}
		rows = append(rows, row{sum: sum, typeSort: typeOrder, order: m.Value.OrderFlag})
	}
	slices.SortStableFunc(rows, func(a, b row) int {
		if c := strings.Compare(a.sum.Date, b.sum.Date); c != 0 {
			return c
		}
		if a.typeSort != b.typeSort {
			return a.typeSort - b.typeSort
		}
		return a.order - b.order
	})
	all := make([]MealSummary, len(rows))
	for i, r := range rows {
		all[i] = r.sum
	}
	return paginate(all, limit, cursor)
}

func parseDate(field, value string) (time.Time, error) {
	t, err := time.Parse(dateLayout, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}, invalid("%s is required and must be a date in YYYY-MM-DD form.", field)
	}
	return t, nil
}

// NamedRef is a uid and a name.
type NamedRef struct {
	UID  string
	Name string
}

// ListMealTypes returns the account's meal types in display order. An account
// with none gets the four built-in types, flagged Builtin and without a uid.
func (s *Service) ListMealTypes(
	ctx context.Context,
	limit int,
	cursor string,
) (Page[MealTypeInfo], error) {
	types, err := s.cache.MealTypes(ctx)
	if err != nil {
		return Page[MealTypeInfo]{}, s.mapError("list_meal_types", err)
	}
	return paginate(newMealTypes(types).infos(), limit, cursor)
}

// GroceryListSummary is a grocery list with its item counts.
type GroceryListSummary struct {
	UID              string
	Name             string
	IsDefault        bool
	ItemCount        int
	UnpurchasedCount int
}

// ListGroceryLists returns the grocery lists with item counts.
func (s *Service) ListGroceryLists(
	ctx context.Context,
	limit int,
	cursor string,
) (Page[GroceryListSummary], error) {
	lists, err := s.cache.GroceryLists(ctx)
	if err != nil {
		return Page[GroceryListSummary]{}, s.mapError("list_grocery_lists", err)
	}
	items, err := s.cache.Groceries(ctx)
	if err != nil {
		return Page[GroceryListSummary]{}, s.mapError("list_grocery_lists", err)
	}
	total, open := map[string]int{}, map[string]int{}
	for _, it := range items {
		total[it.Value.ListUID]++
		if !it.Value.Purchased {
			open[it.Value.ListUID]++
		}
	}
	vals := make([]paprikaclient.GroceryList, len(lists))
	for i, l := range lists {
		vals[i] = l.Value
	}
	slices.SortStableFunc(
		vals,
		func(a, b paprikaclient.GroceryList) int { return a.OrderFlag - b.OrderFlag },
	)
	all := make([]GroceryListSummary, len(vals))
	for i, l := range vals {
		all[i] = GroceryListSummary{
			UID:              l.UID,
			Name:             l.Name,
			IsDefault:        l.IsDefault,
			ItemCount:        total[l.UID],
			UnpurchasedCount: open[l.UID],
		}
	}
	return paginate(all, limit, cursor)
}

// GroceryItemSummary is one grocery item.
type GroceryItemSummary struct {
	UID         string
	Name        string
	Quantity    string
	Instruction string
	Aisle       string
	Purchased   bool
}

// GroceryItemsPage is a page of items from one list.
type GroceryItemsPage struct {
	ListUID  string
	ListName string
	Page[GroceryItemSummary]
}

// ListGroceryItems returns the items of one list. List is a list uid or name;
// empty selects the default list.
func (s *Service) ListGroceryItems(
	ctx context.Context,
	list string,
	includePurchased bool,
	limit int,
	cursor string,
) (GroceryItemsPage, error) {
	if err := checkText("list", list, maxRefLen); err != nil {
		return GroceryItemsPage{}, err
	}
	lists, err := s.cache.GroceryLists(ctx)
	if err != nil {
		return GroceryItemsPage{}, s.mapError("list_grocery_items", err)
	}
	target, err := pickList(lists, list)
	if err != nil {
		return GroceryItemsPage{}, err
	}
	items, err := s.cache.Groceries(ctx)
	if err != nil {
		return GroceryItemsPage{}, s.mapError("list_grocery_items", err)
	}
	aisles, err := s.cache.GroceryAisles(ctx)
	if err != nil {
		return GroceryItemsPage{}, s.mapError("list_grocery_items", err)
	}
	aisleName := map[string]string{}
	for _, a := range aisles {
		aisleName[a.Value.UID] = a.Value.Name
	}

	var vals []paprikaclient.Grocery
	for _, it := range items {
		if it.Value.ListUID != target.UID || (it.Value.Purchased && !includePurchased) {
			continue
		}
		vals = append(vals, it.Value)
	}
	aisleOf := func(g paprikaclient.Grocery) string {
		if g.Aisle != "" {
			return g.Aisle
		}
		return aisleName[g.AisleUID]
	}
	slices.SortStableFunc(vals, func(a, b paprikaclient.Grocery) int {
		if c := strings.Compare(strings.ToLower(aisleOf(a)), strings.ToLower(aisleOf(b))); c != 0 {
			return c
		}
		if a.OrderFlag != b.OrderFlag {
			return a.OrderFlag - b.OrderFlag
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	all := make([]GroceryItemSummary, len(vals))
	for i, g := range vals {
		all[i] = GroceryItemSummary{
			UID:         g.UID,
			Name:        g.Name,
			Quantity:    g.Quantity,
			Instruction: g.Instruction,
			Aisle:       aisleOf(g),
			Purchased:   g.Purchased,
		}
	}
	page, err := paginate(all, limit, cursor)
	if err != nil {
		return GroceryItemsPage{}, err
	}
	return GroceryItemsPage{ListUID: target.UID, ListName: target.Name, Page: page}, nil
}

// pickList finds a list by uid, then by case-insensitive name. An empty ref
// selects the default list.
func pickList(
	lists []paprikaclient.Entry[paprikaclient.GroceryList],
	ref string,
) (paprikaclient.GroceryList, error) {
	ref = strings.TrimSpace(ref)
	if len(lists) == 0 {
		return paprikaclient.GroceryList{}, notFound("There are no grocery lists.")
	}
	if ref == "" {
		for _, l := range lists {
			if l.Value.IsDefault {
				return l.Value, nil
			}
		}
		return lists[0].Value, nil
	}
	for _, l := range lists {
		if l.Value.UID == ref {
			return l.Value, nil
		}
	}
	var found []paprikaclient.GroceryList
	for _, l := range lists {
		if strings.EqualFold(strings.TrimSpace(l.Value.Name), ref) {
			found = append(found, l.Value)
		}
	}
	switch len(found) {
	case 0:
		return paprikaclient.GroceryList{}, notFound(
			"Unknown grocery list %q. Call list_grocery_lists to see the valid names and uid values.",
			ref,
		)
	case 1:
		return found[0], nil
	default:
		return paprikaclient.GroceryList{}, invalid(
			"Several grocery lists are named %q. Use the list uid from list_grocery_lists instead.",
			ref,
		)
	}
}

// ListGroceryAisles returns the store aisles in display order.
func (s *Service) ListGroceryAisles(
	ctx context.Context,
	limit int,
	cursor string,
) (Page[NamedRef], error) {
	aisles, err := s.cache.GroceryAisles(ctx)
	if err != nil {
		return Page[NamedRef]{}, s.mapError("list_grocery_aisles", err)
	}
	vals := make([]paprikaclient.GroceryAisle, len(aisles))
	for i, a := range aisles {
		vals[i] = a.Value
	}
	slices.SortStableFunc(
		vals,
		func(a, b paprikaclient.GroceryAisle) int { return a.OrderFlag - b.OrderFlag },
	)
	all := make([]NamedRef, len(vals))
	for i, v := range vals {
		all[i] = NamedRef{UID: v.UID, Name: v.Name}
	}
	return paginate(all, limit, cursor)
}
