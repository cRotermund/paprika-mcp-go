package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/paprikaclient"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/paprikaclient/fakepaprika"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/service"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/testsupport"
)

const (
	apiPrefix   = "/api/v2/sync/"
	notifyPath  = apiPrefix + "notify/"
	mealsPath   = apiPrefix + "meals/"
	itemsPath   = apiPrefix + "groceries/"
	listsPath   = apiPrefix + "grocerylists/"
	secretName  = "SECRET-RECIPE-NAME-77"
	secretBody  = "SECRET-RECIPE-TEXT-88"
	typeLunchNo = 1
)

var (
	uidPattern  = regexp.MustCompile(`^[0-9A-F]{8}-[0-9A-F]{4}-4[0-9A-F]{3}-[89AB][0-9A-F]{3}-[0-9A-F]{12}$`)
	hashPattern = regexp.MustCompile(`^[0-9A-F]{64}$`)
)

func recipePostPath(uid string) string { return apiPrefix + "recipe/" + uid + "/" }

// syncBuffer is a log destination that is safe for concurrent writers.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// writeStack builds a stack over the shared seed, with logs captured.
func writeStack(t *testing.T, extra func(*fakepaprika.Server), tweak func(*paprikaclient.Config)) (*testsupport.Stack, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	st := testsupport.NewStackWith(t, logger, func(f *fakepaprika.Server) {
		seed(f)
		if extra != nil {
			extra(f)
		}
	}, tweak)
	st.Fake.ResetRequests()
	return st, logs
}

// bodyOf decodes the single JSON object of a recorded recipe upload.
func objectBody(t *testing.T, r fakepaprika.Request) map[string]any {
	t.Helper()
	var o map[string]any
	if err := json.Unmarshal(r.Body, &o); err != nil {
		t.Fatalf("body %s: %v", r.Body, err)
	}
	return o
}

// arrayBody decodes the JSON array of a recorded bulk upload.
func arrayBody(t *testing.T, r fakepaprika.Request) []map[string]any {
	t.Helper()
	var a []map[string]any
	if err := json.Unmarshal(r.Body, &a); err != nil {
		t.Fatalf("body %s: %v", r.Body, err)
	}
	return a
}

func onlyPost(t *testing.T, f *fakepaprika.Server, path string) fakepaprika.Request {
	t.Helper()
	posts := f.RequestsTo("POST", path)
	if len(posts) != 1 {
		t.Fatalf("POSTs to %s = %d, want exactly 1", path, len(posts))
	}
	checkUpload(t, posts[0])
	return posts[0]
}

// checkUpload asserts the gzip multipart shape of a write.
func checkUpload(t *testing.T, r fakepaprika.Request) {
	t.Helper()
	if r.BodyError != "" {
		t.Fatalf("upload error: %s", r.BodyError)
	}
	if len(r.Parts) != 1 || r.Parts[0].Name != "data" || !r.Parts[0].Gzipped {
		t.Fatalf("parts = %+v, want one gzipped part named data", r.Parts)
	}
}

func writes(f *fakepaprika.Server) int {
	n := 0
	for _, r := range f.Requests() {
		if r.Method == "POST" {
			n++
		}
	}
	return n
}

// notifiedAfterPost asserts that notify came once, after the last other POST.
func notifiedAfterPost(t *testing.T, f *fakepaprika.Server) {
	t.Helper()
	reqs := f.Requests()
	lastWrite, notifies, notifyAt := -1, 0, -1
	for i, r := range reqs {
		if r.Method != "POST" {
			continue
		}
		if r.Path == notifyPath {
			notifies++
			notifyAt = i
		} else {
			lastWrite = i
		}
	}
	if notifies != 1 || notifyAt < lastWrite || lastWrite < 0 {
		t.Fatalf("notify calls = %d (at %d), last write at %d", notifies, notifyAt, lastWrite)
	}
}

func firstIndex(f *fakepaprika.Server, method, path string) int {
	for i, r := range f.Requests() {
		if r.Method == method && r.Path == path {
			return i
		}
	}
	return -1
}

func str(v string) *string { return &v }

func TestWritesNeedAWriter(t *testing.T) {
	st := testsupport.NewStack(t, nil, seed)
	readOnly := service.New(st.Cache, nil)
	_, err := readOnly.CreateRecipe(context.Background(), service.RecipeFields{Name: str("X")})
	if kind(t, err) != service.KindInternal {
		t.Fatalf("error = %v", err)
	}
}

func TestCreateRecipe(t *testing.T) {
	st, _ := writeStack(t, nil, nil)
	ctx := context.Background()
	cats := []string{"soups", "Dinner", "CAT-SOUP"}
	res, err := st.Service.CreateRecipe(ctx, service.RecipeFields{
		Name: str("  MCP-TEST Lentil Soup "), Ingredients: str("1 cup lentils\r\n2 carrots"),
		Directions: str("Simmer."), Rating: new(4), Favorite: new(true), Categories: &cats,
	})
	if err != nil {
		t.Fatal(err)
	}
	post := onlyPost(t, st.Fake, apiPrefix+"recipe/"+res.Recipe.UID+"/")
	body := objectBody(t, post)

	if !uidPattern.MatchString(res.Recipe.UID) || body["uid"] != res.Recipe.UID {
		t.Errorf("uid = %v, want an uppercase v4 UUID", body["uid"])
	}
	if h, _ := body["hash"].(string); !hashPattern.MatchString(h) || h != res.Recipe.Hash {
		t.Errorf("hash = %v", body["hash"])
	}
	if body["name"] != "MCP-TEST Lentil Soup" || body["ingredients"] != "1 cup lentils\n2 carrots" {
		t.Errorf("text fields = %v / %v", body["name"], body["ingredients"])
	}
	if !reflect.DeepEqual(body["categories"], []any{"CAT-SOUP", "CAT-DINNER"}) {
		t.Errorf("categories = %v, want the two uids once each", body["categories"])
	}
	for _, k := range []string{"photo", "photo_large", "photo_hash", "image_url", "scale"} {
		if v, ok := body[k]; !ok || v != nil {
			t.Errorf("%s = %v (present %v), want null", k, v, ok)
		}
	}
	if body["rating"] != float64(4) || body["on_favorites"] != true || body["in_trash"] != false || body["deleted"] != false {
		t.Errorf("flags = %v %v %v %v", body["rating"], body["on_favorites"], body["in_trash"], body["deleted"])
	}
	if body["description"] != "" || body["notes"] != "" {
		t.Errorf("blank text fields should be empty strings: %v", body)
	}
	notifiedAfterPost(t, st.Fake)

	// The cache knows the recipe without another read of the list.
	page, err := st.Service.SearchRecipes(ctx, service.SearchParams{Query: "lentil"})
	if err != nil || len(page.Items) != 1 || page.Items[0].UID != res.Recipe.UID {
		t.Fatalf("search = %+v, %v", page.Items, err)
	}
	if e, ok := st.Cache.IndexEntry(res.Recipe.UID); !ok || e.Hash != res.Recipe.Hash {
		t.Errorf("index entry = %+v, %v", e, ok)
	}
}

func TestCreateRecipeRejectsBeforeAnyRequest(t *testing.T) {
	tests := []struct {
		name string
		f    service.RecipeFields
		want service.Kind
		text string
	}{
		{"no name", service.RecipeFields{}, service.KindInvalid, "name is required"},
		{"blank name", service.RecipeFields{Name: str("  ")}, service.KindInvalid, "blank"},
		{"bad rating", service.RecipeFields{Name: str("X"), Rating: new(9)}, service.KindInvalid, "rating"},
		{"name too long", service.RecipeFields{Name: str(strings.Repeat("x", 501))}, service.KindInvalid, "too long"},
		{"blank category", service.RecipeFields{Name: str("X"), Categories: &[]string{" "}}, service.KindInvalid, "blank"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, _ := writeStack(t, nil, nil)
			_, err := st.Service.CreateRecipe(context.Background(), tt.f)
			if kind(t, err) != tt.want || !strings.Contains(err.Error(), tt.text) {
				t.Fatalf("error = %v", err)
			}
			if n := len(st.Fake.Requests()); n != 0 {
				t.Errorf("%d requests were made before validation failed", n)
			}
		})
	}
}

func TestCreateRecipeUnknownCategory(t *testing.T) {
	st, _ := writeStack(t, nil, nil)
	_, err := st.Service.CreateRecipe(context.Background(), service.RecipeFields{Name: str("X"), Categories: &[]string{"Soups", "Brunchy"}})
	if kind(t, err) != service.KindNotFound || !strings.Contains(err.Error(), `"Brunchy"`) {
		t.Fatalf("error = %v", err)
	}
	if writes(st.Fake) != 0 {
		t.Error("a recipe was written")
	}
}

func TestUpdateRecipeRereadsAndKeepsEverything(t *testing.T) {
	st, _ := writeStack(t, nil, nil)
	ctx := context.Background()
	// Another device changed the recipe after the cache read it.
	live := testsupport.Recipe("R1", "Tomato Soup", "tomatoes\ncream", "CAT-SOUP")
	live["notes"] = "added on the phone"
	live["photo"] = "PHOTO-BYTES"
	live["photo_hash"] = "PHOTOHASH"
	live["photo_large"] = "LARGE"
	live["image_url"] = "https://example.test/x.jpg"
	live["other_future"] = []any{1, "two"}
	live["hash"] = testsupport.Hash("phone edit")
	st.Fake.SeedRecipe(live)
	before, _ := st.Fake.StoredRecipe("R1")

	res, err := st.Service.UpdateRecipe(ctx, "R1", service.RecipeFields{Rating: new(4), Categories: &[]string{"Soups", "Dinner"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Changed, []string{"rating", "categories"}) {
		t.Errorf("changed = %v", res.Changed)
	}
	if first, post := firstIndex(st.Fake, "GET", recipePostPath("R1")), firstIndex(st.Fake, "POST", recipePostPath("R1")); first < 0 || first > post {
		t.Errorf("the recipe was not read (%d) before it was written (%d)", first, post)
	}
	body := objectBody(t, onlyPost(t, st.Fake, recipePostPath("R1")))

	for k, v := range before {
		if k == "hash" || k == "rating" || k == "categories" {
			continue
		}
		if got, ok := body[k]; !ok || !reflect.DeepEqual(fmt.Sprint(got), fmt.Sprint(v)) {
			t.Errorf("field %q = %v (present %v), want %v kept as read", k, got, ok, v)
		}
	}
	if body["notes"] != "added on the phone" {
		t.Error("the change made on another device was lost")
	}
	if body["future_field"] == nil || body["other_future"] == nil {
		t.Error("an unknown field was dropped")
	}
	if h, _ := body["hash"].(string); !hashPattern.MatchString(h) || h == before["hash"] {
		t.Errorf("hash = %v, want a new hash", body["hash"])
	}
	if !reflect.DeepEqual(body["categories"], []any{"CAT-SOUP", "CAT-DINNER"}) {
		t.Errorf("categories = %v", body["categories"])
	}
	notifiedAfterPost(t, st.Fake)

	if e, _ := st.Cache.IndexEntry("R1"); e == nil || e.Hash != res.Recipe.Hash || e.Rating != 4 {
		t.Errorf("index entry = %+v", e)
	}
	got, err := st.Service.GetRecipe(ctx, "R1")
	if err != nil || got.Hash != res.Recipe.Hash || got.Notes != "added on the phone" {
		t.Errorf("GetRecipe = %+v, %v", got, err)
	}
}

func TestUpdateRecipeOnlyGivenFieldsAndNoChange(t *testing.T) {
	st, _ := writeStack(t, nil, nil)
	ctx := context.Background()

	if _, err := st.Service.UpdateRecipe(ctx, "R1", service.RecipeFields{}, ""); kind(t, err) != service.KindInvalid {
		t.Errorf("an empty update: %v", err)
	}
	if _, err := st.Service.UpdateRecipe(ctx, "NOPE", service.RecipeFields{Rating: new(1)}, ""); kind(t, err) != service.KindNotFound || !strings.Contains(err.Error(), `"NOPE"`) {
		t.Errorf("unknown recipe: %v", err)
	}
	st.Fake.ResetRequests()

	res, err := st.Service.UpdateRecipe(ctx, "R1", service.RecipeFields{Name: str("Tomato Soup"), Rating: new(0)}, "")
	if err != nil || !res.NoChange || len(res.Changed) != 0 {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if writes(st.Fake) != 0 {
		t.Error("an update that changes nothing was sent")
	}

	res, err = st.Service.UpdateRecipe(ctx, "R1", service.RecipeFields{Directions: str("Stir.\r\nServe.")}, "")
	if err != nil || !reflect.DeepEqual(res.Changed, []string{"directions"}) {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if got := objectBody(t, onlyPost(t, st.Fake, recipePostPath("R1")))["directions"]; got != "Stir.\nServe." {
		t.Errorf("directions = %q", got)
	}
}

func TestUpdateRecipeExpectedHash(t *testing.T) {
	st, _ := writeStack(t, nil, nil)
	ctx := context.Background()
	current, _ := st.Fake.StoredRecipe("R1")
	hash := current["hash"].(string)

	_, err := st.Service.UpdateRecipe(ctx, "R1", service.RecipeFields{Rating: new(5)}, "STALE")
	if kind(t, err) != service.KindConflict || !strings.Contains(err.Error(), "get_recipe") {
		t.Fatalf("error = %v, want a conflict", err)
	}
	if writes(st.Fake) != 0 {
		t.Error("a conflicting update was written")
	}
	if _, err := st.Service.UpdateRecipe(ctx, "R1", service.RecipeFields{Rating: new(5)}, hash); err != nil {
		t.Fatalf("matching hash: %v", err)
	}
	// The hash changed, so the same expected_hash now conflicts.
	if _, err := st.Service.UpdateRecipe(ctx, "R1", service.RecipeFields{Rating: new(2)}, hash); kind(t, err) != service.KindConflict {
		t.Fatalf("second update: %v", err)
	}
}

func TestUpdateRecipeUnknownCategory(t *testing.T) {
	st, _ := writeStack(t, nil, nil)
	_, err := st.Service.UpdateRecipe(context.Background(), "R1", service.RecipeFields{Categories: &[]string{"Nonexistent"}}, "")
	if kind(t, err) != service.KindNotFound || !strings.Contains(err.Error(), `"Nonexistent"`) || writes(st.Fake) != 0 {
		t.Fatalf("error = %v, writes = %d", err, writes(st.Fake))
	}
}

func TestTrashAndRestoreRecipe(t *testing.T) {
	st, _ := writeStack(t, nil, nil)
	ctx := context.Background()
	before, _ := st.Fake.StoredRecipe("R1")

	res, err := st.Service.TrashRecipe(ctx, "R1")
	if err != nil || !res.Recipe.InTrash {
		t.Fatalf("trash = %+v, %v", res, err)
	}
	body := objectBody(t, onlyPost(t, st.Fake, recipePostPath("R1")))
	if body["in_trash"] != true || body["hash"] == before["hash"] || body["deleted"] != false {
		t.Errorf("body = trash %v hash %v deleted %v", body["in_trash"], body["hash"], body["deleted"])
	}
	page, _ := st.Service.SearchRecipes(ctx, service.SearchParams{Query: "tomato"})
	if len(page.Items) != 0 {
		t.Error("a trashed recipe is still in the default search")
	}
	page, _ = st.Service.SearchRecipes(ctx, service.SearchParams{Query: "tomato", IncludeTrashed: true})
	if len(page.Items) != 1 || !page.Items[0].InTrash {
		t.Errorf("trashed search = %+v", page.Items)
	}

	// Trashing again changes nothing.
	st.Fake.ResetRequests()
	if res, err := st.Service.TrashRecipe(ctx, "R1"); err != nil || !res.NoChange || writes(st.Fake) != 0 {
		t.Fatalf("second trash = %+v, %v, writes %d", res, err, writes(st.Fake))
	}

	res, err = st.Service.RestoreRecipe(ctx, "R1")
	if err != nil || res.Recipe.InTrash {
		t.Fatalf("restore = %+v, %v", res, err)
	}
	if stored, _ := st.Fake.StoredRecipe("R1"); stored["in_trash"] != false {
		t.Errorf("stored in_trash = %v", stored["in_trash"])
	}
	if _, err := st.Service.TrashRecipe(ctx, "NOPE"); kind(t, err) != service.KindNotFound {
		t.Errorf("unknown uid: %v", err)
	}
}

func TestWriteLockSerializesWrites(t *testing.T) {
	st, _ := writeStack(t, nil, nil)
	// Widen the window between each write's read and its POST.
	st.Fake.Inject(fakepaprika.Fault{Method: "GET", Path: recipePostPath("R1"), Times: -1, Kind: fakepaprika.FaultDelay, Delay: 15 * time.Millisecond})

	updates := []service.RecipeFields{
		{Description: str("d")}, {Notes: str("n")}, {Servings: str("s")}, {PrepTime: str("p")},
		{CookTime: str("c")}, {TotalTime: str("t")}, {Difficulty: str("f")}, {Source: str("o")},
	}
	var wg sync.WaitGroup
	errs := make([]error, len(updates))
	for i, u := range updates {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = st.Service.UpdateRecipe(context.Background(), "R1", u, "")
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	stored, _ := st.Fake.StoredRecipe("R1")
	for k, want := range map[string]string{"description": "d", "notes": "n", "servings": "s", "prep_time": "p",
		"cook_time": "c", "total_time": "t", "difficulty": "f", "source": "o"} {
		if stored[k] != want {
			t.Errorf("%s = %v, want %q: a concurrent write overwrote another", k, stored[k], want)
		}
	}
}

func TestPostTimeoutIsUnknownOutcomeWithoutRetry(t *testing.T) {
	st, logs := writeStack(t, nil, func(c *paprikaclient.Config) { c.RequestTimeout = 150 * time.Millisecond })
	st.Fake.Inject(fakepaprika.Fault{Method: "POST", Path: recipePostPath("R1"), Times: -1, Kind: fakepaprika.FaultDelay, Delay: 2 * time.Second})

	_, err := st.Service.UpdateRecipe(context.Background(), "R1", service.RecipeFields{Rating: new(5)}, "")
	if kind(t, err) != service.KindOutcomeUnknown || !strings.Contains(err.Error(), "may or may not") {
		t.Fatalf("error = %v", err)
	}
	if n := len(st.Fake.RequestsTo("POST", recipePostPath("R1"))); n != 1 {
		t.Fatalf("recipe POSTs = %d, want exactly 1 (never retried)", n)
	}
	if n := len(st.Fake.RequestsTo("POST", notifyPath)); n != 0 {
		t.Errorf("notify calls = %d after an unknown outcome", n)
	}
	if !strings.Contains(logs.String(), "outcome=outcome_unknown") {
		t.Errorf("audit log lacks the unknown outcome:\n%s", logs)
	}
}

func TestCallerDeadlineDuringPostIsUnknownOutcome(t *testing.T) {
	st, _ := writeStack(t, nil, nil)
	st.Fake.Inject(fakepaprika.Fault{Method: "POST", Path: recipePostPath("R1"), Times: -1, Kind: fakepaprika.FaultDelay, Delay: 2 * time.Second})

	// The caller's deadline, not the client's own timeout, ends the POST.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := st.Service.UpdateRecipe(ctx, "R1", service.RecipeFields{Rating: new(5)}, "")
	if kind(t, err) != service.KindOutcomeUnknown {
		t.Fatalf("error = %v, want an unknown outcome", err)
	}
	if n := len(st.Fake.RequestsTo("POST", notifyPath)); n != 0 {
		t.Errorf("notify calls = %d after an unknown outcome", n)
	}
}

func TestCallerDeadlineBeforePostIsTemporaryNotUnknown(t *testing.T) {
	st, _ := writeStack(t, nil, nil)
	st.Fake.Inject(fakepaprika.Fault{Method: "GET", Path: recipePostPath("R1"), Times: -1, Kind: fakepaprika.FaultDelay, Delay: 2 * time.Second})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := st.Service.UpdateRecipe(ctx, "R1", service.RecipeFields{Rating: new(5)}, "")
	if kind(t, err) != service.KindTemporary {
		t.Fatalf("error = %v, want temporarily unavailable", err)
	}
	if n := len(st.Fake.RequestsTo("POST", recipePostPath("R1"))); n != 0 {
		t.Errorf("recipe POSTs = %d, want none: the re-read never finished", n)
	}
}

func TestBatchTimeoutIsUnknownOutcomeForTheWholeBatch(t *testing.T) {
	st, _ := writeStack(t, nil, func(c *paprikaclient.Config) { c.RequestTimeout = 150 * time.Millisecond })
	st.Fake.Inject(fakepaprika.Fault{Method: "POST", Path: mealsPath, Times: -1, Kind: fakepaprika.FaultDelay, Delay: 2 * time.Second})

	_, err := st.Service.AddMeals(context.Background(), []service.MealInput{
		{Date: "2030-04-01", MealType: "Dinner", Name: "A"}, {Date: "2030-04-02", MealType: "Dinner", Name: "B"},
	})
	if kind(t, err) != service.KindOutcomeUnknown {
		t.Fatalf("error = %v", err)
	}
	if n := len(st.Fake.RequestsTo("POST", mealsPath)); n != 1 {
		t.Fatalf("meal POSTs = %d, want exactly 1", n)
	}
}

func TestNotifyFailureIsSuccessWithWarning(t *testing.T) {
	st, logs := writeStack(t, nil, nil)
	st.Fake.Inject(fakepaprika.Fault{Method: "POST", Path: notifyPath, Kind: fakepaprika.FaultEnvelopeError, Code: 5, Message: "UPSTREAM-DETAIL"})

	res, err := st.Service.UpdateRecipe(context.Background(), "R1", service.RecipeFields{Rating: new(3)}, "")
	if err != nil {
		t.Fatalf("a notify failure failed the write: %v", err)
	}
	if res.Warning == "" || strings.Contains(res.Warning, "UPSTREAM-DETAIL") {
		t.Errorf("warning = %q", res.Warning)
	}
	if stored, _ := st.Fake.StoredRecipe("R1"); stored["rating"] != json.Number("3") {
		t.Errorf("stored rating = %v", stored["rating"])
	}
	if e, _ := st.Cache.IndexEntry("R1"); e == nil || e.Rating != 3 {
		t.Error("the cache was not updated")
	}
	if !strings.Contains(logs.String(), "outcome=ok_notify_failed") {
		t.Errorf("audit log:\n%s", logs)
	}
}

func TestAuditLogHasFieldNamesNeverValues(t *testing.T) {
	st, logs := writeStack(t, nil, nil)
	ctx := context.Background()
	res, err := st.Service.CreateRecipe(ctx, service.RecipeFields{Name: str(secretName), Ingredients: str(secretBody), Notes: str(secretBody)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Service.UpdateRecipe(ctx, res.Recipe.UID, service.RecipeFields{Directions: str(secretBody)}, "WRONG-HASH"); err == nil {
		t.Fatal("expected a conflict")
	}
	if _, err := st.Service.UpdateRecipe(ctx, res.Recipe.UID, service.RecipeFields{Directions: str(secretBody)}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Service.AddGroceryItems(ctx, "", []service.GroceryItemInput{{Text: secretBody}}); err != nil {
		t.Fatal(err)
	}

	out := logs.String()
	for _, secret := range []string{secretName, secretBody, st.Fake.Password(), st.Fake.LastToken(), st.Fake.Email()} {
		if strings.Contains(out, secret) {
			t.Errorf("log contains %q:\n%s", secret, out)
		}
	}
	var audits []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "audit=true") {
			audits = append(audits, line)
		}
	}
	joined := strings.Join(audits, "\n")
	for _, want := range []string{"tool=create_recipe", "resource=recipe", "uid=" + res.Recipe.UID, "fields=name,ingredients,notes",
		"fields=directions", "outcome=ok", "outcome=conflict", "tool=add_grocery_items", "resource=grocery_item"} {
		if !strings.Contains(joined, want) {
			t.Errorf("audit log lacks %q:\n%s", want, joined)
		}
	}
}

// ---- meals ----

func mealByUID(t *testing.T, f *fakepaprika.Server, uid string) map[string]any {
	t.Helper()
	for _, m := range f.Objects("meals") {
		if m["uid"] == uid {
			return m
		}
	}
	return nil
}

func TestAddMealsWithAccountMealTypes(t *testing.T) {
	st, _ := writeStack(t, nil, nil)
	ctx := context.Background()
	res, err := st.Service.AddMeals(ctx, []service.MealInput{
		{Date: "2030-04-01", MealType: "dinner", RecipeUID: "R2"},
		{Date: "2030-04-01", MealType: "Breakfast", Name: "Oatmeal"},
		{Date: "2030-04-02", MealType: "Dinner", RecipeUID: "R1", Name: "Soup again"},
	})
	if err != nil || res.Added != 3 {
		t.Fatalf("result = %+v, %v", res, err)
	}
	post := onlyPost(t, st.Fake, mealsPath)
	meals := arrayBody(t, post)
	if len(meals) != 3 {
		t.Fatalf("one POST should hold all 3 meals, got %d", len(meals))
	}
	m := meals[0]
	if !uidPattern.MatchString(m["uid"].(string)) || m["date"] != "2030-04-01 00:00:00" || m["type_uid"] != typeDinner ||
		m["type"] != float64(2) || m["recipe_uid"] != "R2" || m["name"] != "Beef Chili" ||
		m["deleted"] != false || m["is_ingredient"] != false {
		t.Errorf("recipe meal = %v", m)
	}
	if v, ok := m["scale"]; !ok || v != nil {
		t.Errorf("scale = %v (present %v), want null", v, ok)
	}
	if meals[1]["type_uid"] != typeBreakfast || meals[1]["type"] != float64(0) || meals[1]["recipe_uid"] != nil || meals[1]["name"] != "Oatmeal" {
		t.Errorf("named meal = %v", meals[1])
	}
	if meals[2]["name"] != "Soup again" || meals[2]["recipe_uid"] != "R1" {
		t.Errorf("renamed link = %v", meals[2])
	}
	notifiedAfterPost(t, st.Fake)

	list, err := st.Service.ListMeals(ctx, "2030-04-01", "2030-04-02", 0, "")
	if err != nil || len(list.Items) != 3 {
		t.Fatalf("ListMeals = %+v, %v", list.Items, err)
	}
	if list.Items[0].MealType != "Breakfast" || list.Items[1].MealType != "Dinner" || list.Items[1].RecipeName != "Beef Chili" {
		t.Errorf("listed = %+v", list.Items)
	}
}

func TestAddMealsReportsItemFailuresAndWritesTheRest(t *testing.T) {
	st, _ := writeStack(t, nil, nil)
	res, err := st.Service.AddMeals(context.Background(), []service.MealInput{
		{Date: "2030-04-01", MealType: "Brunch", Name: "A"},
		{Date: "2030-04-01", MealType: "Dinner", Name: "B"},
		{Date: "2030-04-01", MealType: "Dinner", RecipeUID: "NOPE"},
	})
	if err != nil || res.Added != 1 {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if !strings.Contains(res.Items[0].Failure, `"Brunch"`) || !strings.Contains(res.Items[0].Failure, "Breakfast, Dinner") {
		t.Errorf("unknown type failure = %q", res.Items[0].Failure)
	}
	if res.Items[1].Failure != "" || res.Items[1].Meal.UID == "" {
		t.Errorf("item 1 = %+v", res.Items[1])
	}
	if !strings.Contains(res.Items[2].Failure, `"NOPE"`) {
		t.Errorf("unknown recipe failure = %q", res.Items[2].Failure)
	}
	if got := arrayBody(t, onlyPost(t, st.Fake, mealsPath)); len(got) != 1 || got[0]["name"] != "B" {
		t.Errorf("posted = %v", got)
	}

	st.Fake.ResetRequests()
	_, err = st.Service.AddMeals(context.Background(), []service.MealInput{{Date: "2030-04-01", MealType: "Brunch", Name: "A"}})
	if kind(t, err) != service.KindInvalid || !strings.Contains(err.Error(), `"Brunch"`) || writes(st.Fake) != 0 {
		t.Fatalf("all-failing batch: %v, writes %d", err, writes(st.Fake))
	}
}

func TestAddMealsValidatesInputFirst(t *testing.T) {
	many := make([]service.MealInput, 51)
	for i := range many {
		many[i] = service.MealInput{Date: "2030-04-01", MealType: "Dinner", Name: "x"}
	}
	tests := []struct {
		name   string
		meals  []service.MealInput
		errHas string
	}{
		{"none", nil, "at least one"},
		{"51 is over the limit", many, "at most 50"},
		{"bad date", []service.MealInput{{Date: "2030-13-40", MealType: "Dinner", Name: "x"}}, "YYYY-MM-DD"},
		{"missing date", []service.MealInput{{MealType: "Dinner", Name: "x"}}, "YYYY-MM-DD"},
		{"missing type", []service.MealInput{{Date: "2030-04-01", Name: "x"}}, "meal_type"},
		{"no recipe or name", []service.MealInput{{Date: "2030-04-01", MealType: "Dinner"}}, "recipe_uid, a name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, _ := writeStack(t, nil, nil)
			_, err := st.Service.AddMeals(context.Background(), tt.meals)
			if kind(t, err) != service.KindInvalid || !strings.Contains(err.Error(), tt.errHas) {
				t.Fatalf("error = %v", err)
			}
			if len(st.Fake.Requests()) != 0 {
				t.Error("requests were made before validation failed")
			}
		})
	}

	t.Run("exactly 50 is accepted in one POST", func(t *testing.T) {
		st, _ := writeStack(t, nil, nil)
		res, err := st.Service.AddMeals(context.Background(), many[:50])
		if err != nil || res.Added != 50 {
			t.Fatalf("result = %+v, %v", res, err)
		}
		if got := arrayBody(t, onlyPost(t, st.Fake, mealsPath)); len(got) != 50 {
			t.Errorf("posted %d meals", len(got))
		}
	})
}

func TestAddMealsWhenTheAccountHasNoMealTypes(t *testing.T) {
	st := testsupport.NewStack(t, nil, func(f *fakepaprika.Server) { f.SeedNoMealTypes() })
	st.Fake.ResetRequests()
	ctx := context.Background()

	res, err := st.Service.AddMeals(ctx, []service.MealInput{
		{Date: "2030-05-02", MealType: "lunch", Name: "Sandwich"},
		{Date: "2030-05-02", MealType: "Snacks", Name: "Nuts"},
		{Date: "2030-05-02", MealType: "Brunch", Name: "No"},
	})
	if err != nil || res.Added != 2 {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if f := res.Items[2].Failure; !strings.Contains(f, "Breakfast, Lunch, Dinner, Snacks") {
		t.Errorf("failure should list the valid names: %q", f)
	}
	meals := arrayBody(t, onlyPost(t, st.Fake, mealsPath))
	if meals[0]["type"] != float64(typeLunchNo) || meals[0]["type_uid"] != nil || meals[1]["type"] != float64(3) {
		t.Errorf("meals = %v", meals)
	}
	if v, ok := meals[0]["type_uid"]; !ok || v != nil {
		t.Error("type_uid should be sent as null")
	}
	if len(st.Fake.RequestsTo("GET", apiPrefix+"mealtypes/")) != 1 {
		t.Error("meal types should be read once for validation")
	}

	list, err := st.Service.ListMeals(ctx, "2030-05-01", "2030-05-02", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range list.Items {
		got = append(got, m.Date+" "+m.MealType+" "+m.Name)
	}
	want := []string{"2030-05-01 Lunch Leftovers", "2030-05-01 Dinner Pizza", "2030-05-02 Lunch Sandwich", "2030-05-02 Snacks Nuts"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("meals = %v, want %v", got, want)
	}
}

func TestMealTypeNamesFallBack(t *testing.T) {
	ctx := context.Background()
	t.Run("account without types lists the four defaults", func(t *testing.T) {
		st := testsupport.NewStack(t, nil, func(f *fakepaprika.Server) { f.SeedNoMealTypes() })
		page, err := st.Service.ListMealTypes(ctx, 0, "")
		if err != nil || len(page.Items) != 4 {
			t.Fatalf("types = %+v, %v", page.Items, err)
		}
		for i, name := range []string{"Breakfast", "Lunch", "Dinner", "Snacks"} {
			it := page.Items[i]
			if it.Name != name || it.OriginalType != i || !it.Builtin || it.UID != "" {
				t.Errorf("type %d = %+v", i, it)
			}
		}
	})
	t.Run("account types are not flagged", func(t *testing.T) {
		svc, _ := newService(t)
		page, _ := svc.ListMealTypes(ctx, 0, "")
		if page.Items[0].Builtin || page.Items[0].UID == "" {
			t.Errorf("type = %+v", page.Items[0])
		}
	})
	t.Run("type_uid, then original_type, then defaults, then unknown", func(t *testing.T) {
		st := testsupport.NewStack(t, nil, func(f *fakepaprika.Server) {
			f.Seed("mealtypes", fakepaprika.SampleMealType("MT-X", "Brunch", 1))
			byUID := fakepaprika.SampleMeal("A", "by uid", "2030-06-01 00:00:00", "MT-X")
			byUID["type"] = 3
			byOriginal := fakepaprika.SampleLegacyMeal("B", "by original", "2030-06-01 00:00:00", 1)
			byDefault := fakepaprika.SampleLegacyMeal("C", "by default", "2030-06-01 00:00:00", 3)
			unknown := fakepaprika.SampleLegacyMeal("D", "unknown", "2030-06-01 00:00:00", 9)
			dangling := fakepaprika.SampleMeal("E", "dangling uid", "2030-06-01 00:00:00", "GONE")
			dangling["type"] = 0
			f.Seed("meals", byUID, byOriginal, byDefault, unknown, dangling)
		})
		list, err := st.Service.ListMeals(ctx, "2030-06-01", "2030-06-01", 0, "")
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, m := range list.Items {
			got[m.Name] = m.MealType
		}
		want := map[string]string{"by uid": "Brunch", "by original": "Brunch", "by default": "Snacks", "unknown": "Unknown (type 9)", "dangling uid": "Breakfast"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("types = %v, want %v", got, want)
		}
	})
}

func TestUpdateMeal(t *testing.T) {
	st, _ := writeStack(t, func(f *fakepaprika.Server) {
		m := fakepaprika.SampleMeal("M-X", "Old name", "2030-03-20 00:00:00", typeDinner)
		m["future_meal_field"] = "keep me"
		f.Seed("meals", m)
	}, nil)
	ctx := context.Background()

	res, err := st.Service.UpdateMeal(ctx, "M-X", service.MealChanges{Date: str("2030-03-21"), MealType: str("breakfast"), Name: str("New name"), RecipeUID: str("R1")})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Changed, []string{"date", "type", "type_uid", "name", "recipe_uid"}) {
		t.Errorf("changed = %v", res.Changed)
	}
	m := arrayBody(t, onlyPost(t, st.Fake, mealsPath))
	if len(m) != 1 || m[0]["uid"] != "M-X" || m[0]["date"] != "2030-03-21 00:00:00" || m[0]["type"] != float64(0) ||
		m[0]["type_uid"] != typeBreakfast || m[0]["name"] != "New name" || m[0]["recipe_uid"] != "R1" ||
		m[0]["future_meal_field"] != "keep me" || m[0]["deleted"] != false {
		t.Errorf("posted meal = %v", m)
	}
	if v, ok := m[0]["scale"]; !ok || v != nil {
		t.Error("scale was not round-tripped as null")
	}
	notifiedAfterPost(t, st.Fake)
	list, _ := st.Service.ListMeals(ctx, "2030-03-21", "2030-03-21", 0, "")
	if len(list.Items) != 1 || list.Items[0].Name != "New name" || list.Items[0].MealType != "Breakfast" {
		t.Errorf("cache view = %+v", list.Items)
	}

	// Unlink, then repeat for no change.
	if _, err := st.Service.UpdateMeal(ctx, "M-X", service.MealChanges{RecipeUID: str("")}); err != nil {
		t.Fatal(err)
	}
	st.Fake.ResetRequests()
	again, err := st.Service.UpdateMeal(ctx, "M-X", service.MealChanges{RecipeUID: str(""), Date: str("2030-03-21")})
	if err != nil || !again.NoChange || writes(st.Fake) != 0 {
		t.Fatalf("repeat = %+v, %v, writes %d", again, err, writes(st.Fake))
	}
}

func TestUpdateMealRefusals(t *testing.T) {
	tests := []struct {
		name string
		uid  string
		c    service.MealChanges
		want service.Kind
		text string
	}{
		{"no fields", "M-LINK", service.MealChanges{}, service.KindInvalid, "at least one"},
		{"unknown meal", "NOPE", service.MealChanges{Name: str("x")}, service.KindNotFound, `"NOPE"`},
		{"unknown type", "M-LINK", service.MealChanges{MealType: str("Brunch")}, service.KindNotFound, `"Brunch"`},
		{"unknown recipe", "M-LINK", service.MealChanges{RecipeUID: str("NOPE")}, service.KindNotFound, `"NOPE"`},
		{"bad date", "M-LINK", service.MealChanges{Date: str("tomorrow")}, service.KindInvalid, "YYYY-MM-DD"},
		{"blank name", "M-LINK", service.MealChanges{Name: str(" ")}, service.KindInvalid, "name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, _ := writeStack(t, nil, nil)
			_, err := st.Service.UpdateMeal(context.Background(), tt.uid, tt.c)
			if kind(t, err) != tt.want || !strings.Contains(err.Error(), tt.text) {
				t.Fatalf("error = %v", err)
			}
			if writes(st.Fake) != 0 {
				t.Error("a refused update was written")
			}
		})
	}
}

func TestUpdateMealOnAccountWithoutMealTypes(t *testing.T) {
	st := testsupport.NewStack(t, nil, func(f *fakepaprika.Server) { f.SeedNoMealTypes() })
	st.Fake.ResetRequests()
	ctx := context.Background()

	res, err := st.Service.UpdateMeal(ctx, "LEGACY-MEAL-1", service.MealChanges{Date: str("2030-05-03")})
	if err != nil || !reflect.DeepEqual(res.Changed, []string{"date"}) {
		t.Fatalf("result = %+v, %v", res, err)
	}
	m := arrayBody(t, onlyPost(t, st.Fake, mealsPath))[0]
	for _, k := range []string{"type_uid", "recipe_uid", "scale"} {
		if v, ok := m[k]; !ok || v != nil {
			t.Errorf("%s = %v (present %v), want the null kept", k, v, ok)
		}
	}
	if m["is_ingredient"] != false || m["type"] != float64(1) {
		t.Errorf("meal = %v", m)
	}
	if res.Meal.MealType != "Lunch" {
		t.Errorf("meal type = %q", res.Meal.MealType)
	}

	res, err = st.Service.UpdateMeal(ctx, "LEGACY-MEAL-1", service.MealChanges{MealType: str("DINNER")})
	if err != nil || !reflect.DeepEqual(res.Changed, []string{"type"}) {
		t.Fatalf("retype = %+v, %v", res, err)
	}
	last := arrayBody(t, st.Fake.RequestsTo("POST", mealsPath)[1])[0]
	if last["type"] != float64(2) || last["type_uid"] != nil {
		t.Errorf("retyped meal = %v", last)
	}
	if _, err := st.Service.UpdateMeal(ctx, "LEGACY-MEAL-1", service.MealChanges{MealType: str("Brunch")}); kind(t, err) != service.KindNotFound ||
		!strings.Contains(err.Error(), "Breakfast, Lunch, Dinner, Snacks") {
		t.Errorf("unknown name: %v", err)
	}
}

func TestDeleteMeal(t *testing.T) {
	st, _ := writeStack(t, nil, nil)
	ctx := context.Background()
	res, err := st.Service.DeleteMeal(ctx, "M-LINK")
	if err != nil {
		t.Fatal(err)
	}
	if res.Before["name"] != "Soup night" || res.Before["recipe_uid"] != "R1" || res.Before["deleted"] != nil {
		t.Errorf("before = %v", res.Before)
	}
	posted := arrayBody(t, onlyPost(t, st.Fake, mealsPath))
	if len(posted) != 1 || posted[0]["deleted"] != true || posted[0]["name"] != "Soup night" {
		t.Errorf("posted = %v", posted)
	}
	notifiedAfterPost(t, st.Fake)
	if mealByUID(t, st.Fake, "M-LINK") != nil {
		t.Error("the fake still lists the meal")
	}
	list, _ := st.Service.ListMeals(ctx, "2030-03-10", "2030-03-10", 0, "")
	for _, m := range list.Items {
		if m.UID == "M-LINK" {
			t.Error("the cache still lists the deleted meal")
		}
	}
	if _, err := st.Service.DeleteMeal(ctx, "M-LINK"); kind(t, err) != service.KindNotFound {
		t.Errorf("second delete: %v", err)
	}
}

// ---- groceries ----

func groceryByUID(f *fakepaprika.Server, uid string) map[string]any {
	for _, g := range f.Objects("groceries") {
		if g["uid"] == uid {
			return g
		}
	}
	return nil
}

func TestAddGroceryItems(t *testing.T) {
	st, _ := writeStack(t, nil, nil)
	ctx := context.Background()
	res, err := st.Service.AddGroceryItems(ctx, "", []service.GroceryItemInput{
		{Text: "2 carrots, diced", Aisle: "dairy"},
		{Text: "Butter"},
		{Text: "bananas", Quantity: "3", Instruction: "ripe", Ingredient: "banana", Aisle: "Produce"},
	})
	if err != nil || res.Added != 3 || res.ListUID != listMain {
		t.Fatalf("result = %+v, %v", res, err)
	}
	items := arrayBody(t, onlyPost(t, st.Fake, itemsPath))
	if len(items) != 3 {
		t.Fatalf("one POST should hold 3 items, got %d", len(items))
	}
	a := items[0]
	if !uidPattern.MatchString(a["uid"].(string)) || a["name"] != "2 carrots, diced" || a["ingredient"] != "2 carrots, diced" ||
		a["quantity"] != "" || a["instruction"] != "" || a["list_uid"] != listMain || a["aisle_uid"] != "AISLE-2" ||
		a["aisle"] != "Dairy" || a["purchased"] != false || a["deleted"] != false || a["separate"] != false {
		t.Errorf("default item = %v", a)
	}
	if b := items[2]; b["name"] != "bananas" || b["ingredient"] != "banana" || b["quantity"] != "3" || b["instruction"] != "ripe" {
		t.Errorf("explicit item = %v", b)
	}
	notifiedAfterPost(t, st.Fake)

	page, err := st.Service.ListGroceryItems(ctx, "", false, 0, "")
	if err != nil || page.Total != 5 {
		t.Fatalf("items = %+v, %v", page.Page, err)
	}
	lists, _ := st.Service.ListGroceryLists(ctx, 0, "")
	for _, l := range lists.Items {
		if l.UID == listMain && l.ItemCount != 6 {
			t.Errorf("item count = %d", l.ItemCount)
		}
	}
}

func TestGroceryAisleResolutionOrder(t *testing.T) {
	remember := func(f *fakepaprika.Server) {
		f.Seed("groceryingredients",
			map[string]any{"uid": "ING-1", "name": "Butter", "aisle_uid": "AISLE-2"},
			map[string]any{"uid": "ING-2", "name": "ghost", "aisle_uid": "AISLE-GONE"},
			map[string]any{"uid": "ING-3", "name": "nothing", "aisle_uid": nil},
		)
	}
	tests := []struct {
		name      string
		extra     func(*fakepaprika.Server)
		in        service.GroceryItemInput
		wantAisle string
		wantFail  string
	}{
		{"given name wins over the remembered aisle", remember, service.GroceryItemInput{Text: "butter", Aisle: "Produce"}, "AISLE-1", ""},
		{"remembered ingredient", remember, service.GroceryItemInput{Text: "BUTTER"}, "AISLE-2", ""},
		{"remembered by the ingredient field", remember, service.GroceryItemInput{Text: "a block of it", Ingredient: "butter"}, "AISLE-2", ""},
		{"remembered aisle that no longer exists falls through", remember, service.GroceryItemInput{Text: "ghost"}, "AISLE-1", ""},
		{"remembered without an aisle falls through", remember, service.GroceryItemInput{Text: "nothing"}, "AISLE-1", ""},
		{"nothing matches: first aisle", nil, service.GroceryItemInput{Text: "mystery"}, "AISLE-1", ""},
		{"nothing matches: a Miscellaneous aisle is preferred", func(f *fakepaprika.Server) {
			f.Seed("groceryaisles", fakepaprika.SampleAisle("AISLE-MISC", "Miscellaneous"))
		}, service.GroceryItemInput{Text: "mystery"}, "AISLE-MISC", ""},
		{"an unknown given aisle is refused", nil, service.GroceryItemInput{Text: "mystery", Aisle: "Frozen"}, "", `"Frozen"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, _ := writeStack(t, tt.extra, nil)
			res, err := st.Service.AddGroceryItems(context.Background(), "", []service.GroceryItemInput{tt.in, {Text: "filler", Aisle: "Dairy"}})
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantFail != "" {
				if !strings.Contains(res.Items[0].Failure, tt.wantFail) || res.Added != 1 {
					t.Fatalf("results = %+v", res.Items)
				}
				return
			}
			posted := arrayBody(t, onlyPost(t, st.Fake, itemsPath))[0]
			if posted["aisle_uid"] != tt.wantAisle || posted["aisle_uid"] == "" || posted["aisle"] == "" {
				t.Errorf("aisle = %v / %v, want %s", posted["aisle_uid"], posted["aisle"], tt.wantAisle)
			}
		})
	}

	t.Run("an account without aisles cannot place an item", func(t *testing.T) {
		st := testsupport.NewStack(t, nil, func(f *fakepaprika.Server) {
			f.Seed("grocerylists", fakepaprika.SampleGroceryList("L", "Groceries", true))
		})
		st.Fake.ResetRequests()
		_, err := st.Service.AddGroceryItems(context.Background(), "", []service.GroceryItemInput{{Text: "x"}})
		if kind(t, err) != service.KindInvalid || !strings.Contains(err.Error(), "no grocery aisles") || writes(st.Fake) != 0 {
			t.Fatalf("error = %v, writes %d", err, writes(st.Fake))
		}
	})
}

func TestAddGroceryItemsListChoice(t *testing.T) {
	st, _ := writeStack(t, nil, nil)
	ctx := context.Background()
	res, err := st.Service.AddGroceryItems(ctx, "party", []service.GroceryItemInput{{Text: "cake", Aisle: "Dairy"}})
	if err != nil || res.ListUID != listParty {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if got := arrayBody(t, onlyPost(t, st.Fake, itemsPath))[0]["list_uid"]; got != listParty {
		t.Errorf("list_uid = %v", got)
	}

	st.Fake.ResetRequests()
	_, err = st.Service.AddGroceryItems(ctx, "Nope", []service.GroceryItemInput{{Text: "x"}})
	if kind(t, err) != service.KindNotFound || !strings.Contains(err.Error(), `"Nope"`) || writes(st.Fake) != 0 {
		t.Fatalf("unknown list: %v, writes %d", err, writes(st.Fake))
	}
}

func TestAddGroceryItemsBatchLimits(t *testing.T) {
	items := make([]service.GroceryItemInput, 51)
	for i := range items {
		items[i] = service.GroceryItemInput{Text: fmt.Sprintf("item %d", i), Aisle: "Dairy"}
	}
	st, _ := writeStack(t, nil, nil)
	if _, err := st.Service.AddGroceryItems(context.Background(), "", items); kind(t, err) != service.KindInvalid || len(st.Fake.Requests()) != 0 {
		t.Fatalf("51 items: %v, requests %d", err, len(st.Fake.Requests()))
	}
	if _, err := st.Service.AddGroceryItems(context.Background(), "", []service.GroceryItemInput{{Text: " "}}); kind(t, err) != service.KindInvalid || len(st.Fake.Requests()) != 0 {
		t.Fatalf("blank text: %v", err)
	}
	if res, err := st.Service.AddGroceryItems(context.Background(), "", items[:50]); err != nil || res.Added != 50 {
		t.Fatalf("50 items: %+v, %v", res, err)
	}
	if got := arrayBody(t, onlyPost(t, st.Fake, itemsPath)); len(got) != 50 {
		t.Errorf("posted %d items", len(got))
	}
}

func TestUpdateGroceryItem(t *testing.T) {
	st, _ := writeStack(t, func(f *fakepaprika.Server) {
		g := fakepaprika.SampleGrocery("G-1", "carrots", listMain, "AISLE-1")
		g["future_item_field"] = []any{"keep"}
		g["quantity"] = "2"
		f.Seed("groceries", g)
	}, nil)
	ctx := context.Background()

	res, err := st.Service.UpdateGroceryItem(ctx, "G-1", service.GroceryChanges{Name: str("baby carrots"), Aisle: str("dairy"), Purchased: new(true)})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Changed, []string{"name", "ingredient", "aisle_uid", "aisle", "purchased"}) {
		t.Errorf("changed = %v", res.Changed)
	}
	g := arrayBody(t, onlyPost(t, st.Fake, itemsPath))[0]
	if g["name"] != "baby carrots" || g["ingredient"] != "baby carrots" || g["aisle_uid"] != "AISLE-2" || g["aisle"] != "Dairy" ||
		g["purchased"] != true || g["quantity"] != "2" || g["future_item_field"] == nil || g["list_uid"] != listMain {
		t.Errorf("posted item = %v", g)
	}
	notifiedAfterPost(t, st.Fake)

	page, _ := st.Service.ListGroceryItems(ctx, "", false, 0, "")
	for _, it := range page.Items {
		if it.UID == "G-1" {
			t.Error("a checked-off item is still listed as open")
		}
	}
	page, _ = st.Service.ListGroceryItems(ctx, "", true, 0, "")
	found := false
	for _, it := range page.Items {
		if it.UID == "G-1" && it.Purchased && it.Name == "baby carrots" {
			found = true
		}
	}
	if !found {
		t.Errorf("updated item missing from %+v", page.Items)
	}

	st.Fake.ResetRequests()
	again, err := st.Service.UpdateGroceryItem(ctx, "G-1", service.GroceryChanges{Purchased: new(true)})
	if err != nil || !again.NoChange || writes(st.Fake) != 0 {
		t.Fatalf("repeat = %+v, %v", again, err)
	}
	if _, err := st.Service.UpdateGroceryItem(ctx, "G-1", service.GroceryChanges{Aisle: str("Frozen")}); kind(t, err) != service.KindNotFound || writes(st.Fake) != 0 {
		t.Errorf("unknown aisle: %v", err)
	}
	if _, err := st.Service.UpdateGroceryItem(ctx, "NOPE", service.GroceryChanges{Purchased: new(true)}); kind(t, err) != service.KindNotFound {
		t.Errorf("unknown item: %v", err)
	}
	if _, err := st.Service.UpdateGroceryItem(ctx, "G-1", service.GroceryChanges{}); kind(t, err) != service.KindInvalid {
		t.Errorf("no fields: %v", err)
	}
}

func TestDeleteGroceryItem(t *testing.T) {
	st, _ := writeStack(t, nil, nil)
	ctx := context.Background()
	res, err := st.Service.DeleteGroceryItem(ctx, "G-2")
	if err != nil {
		t.Fatal(err)
	}
	if res.Before["name"] != "lettuce" || res.Before["list_uid"] != listMain {
		t.Errorf("before = %v", res.Before)
	}
	if got := arrayBody(t, onlyPost(t, st.Fake, itemsPath))[0]; got["deleted"] != true || got["name"] != "lettuce" {
		t.Errorf("posted = %v", got)
	}
	if groceryByUID(st.Fake, "G-2") != nil {
		t.Error("the fake still lists the item")
	}
	page, _ := st.Service.ListGroceryItems(ctx, "", false, 0, "")
	for _, it := range page.Items {
		if it.UID == "G-2" {
			t.Error("the cache still lists the deleted item")
		}
	}
	if _, err := st.Service.DeleteGroceryItem(ctx, "G-2"); kind(t, err) != service.KindNotFound {
		t.Errorf("second delete: %v", err)
	}
}

func TestCreateAndDeleteGroceryList(t *testing.T) {
	st, _ := writeStack(t, nil, nil)
	ctx := context.Background()

	created, err := st.Service.CreateGroceryList(ctx, "  MCP-TEST Camping ")
	if err != nil {
		t.Fatal(err)
	}
	l := arrayBody(t, onlyPost(t, st.Fake, listsPath))[0]
	if !uidPattern.MatchString(l["uid"].(string)) || l["uid"] != created.List.UID || l["name"] != "MCP-TEST Camping" ||
		l["is_default"] != false || l["reminders_list"] != "Paprika" || l["deleted"] != false {
		t.Errorf("posted list = %v", l)
	}
	notifiedAfterPost(t, st.Fake)
	lists, _ := st.Service.ListGroceryLists(ctx, 0, "")
	if lists.Total != 3 {
		t.Errorf("lists = %+v", lists.Items)
	}
	if _, err := st.Service.CreateGroceryList(ctx, "mcp-test camping"); kind(t, err) != service.KindInvalid || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("duplicate: %v", err)
	}
	if _, err := st.Service.CreateGroceryList(ctx, ""); kind(t, err) != service.KindInvalid {
		t.Errorf("blank: %v", err)
	}

	refusals := []struct{ name, list, text string }{
		{"the default list", "Groceries", "default"},
		{"the default list by uid", listMain, "default"},
		{"a list with items", "Party", "still has 1 item"},
		{"an unknown list", "Nope", `"Nope"`},
	}
	for _, r := range refusals {
		st.Fake.ResetRequests()
		if _, err := st.Service.DeleteGroceryList(ctx, r.list); err == nil || !strings.Contains(err.Error(), r.text) || writes(st.Fake) != 0 {
			t.Errorf("%s: error = %v, writes %d", r.name, err, writes(st.Fake))
		}
	}
	if _, err := st.Service.DeleteGroceryList(ctx, ""); kind(t, err) != service.KindInvalid {
		t.Errorf("an empty list reference must not mean the default list: %v", err)
	}

	st.Fake.ResetRequests()
	res, err := st.Service.DeleteGroceryList(ctx, created.List.UID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Before["name"] != "MCP-TEST Camping" {
		t.Errorf("before = %v", res.Before)
	}
	if got := arrayBody(t, onlyPost(t, st.Fake, listsPath))[0]; got["deleted"] != true || got["uid"] != created.List.UID {
		t.Errorf("posted = %v", got)
	}
	lists, _ = st.Service.ListGroceryLists(ctx, 0, "")
	if lists.Total != 2 {
		t.Errorf("lists after delete = %+v", lists.Items)
	}
}

var _ = paprikaclient.Object{}
