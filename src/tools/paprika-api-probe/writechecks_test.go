package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const realMealName = "Private Family Dinner"

// fakeWriteAPI stores uploads in memory and hides deleted objects, as the API does.
type fakeWriteAPI struct {
	mu          sync.Mutex
	lists       map[string]map[string]object
	recipes     map[string]object
	posts       []string
	recipeCount int
	failPost    string
}

func newFakeWriteAPI(t *testing.T) (*httptest.Server, *fakeWriteAPI) {
	t.Helper()
	api := &fakeWriteAPI{
		recipes: map[string]object{},
		lists: map[string]map[string]object{
			"categories":         {},
			"groceries":          {},
			"groceryingredients": {},
			"grocerylists":       {},
			"groceryaisles":      {"A1": {"uid": "A1", "name": "Produce"}},
			"meals":              {"M1": {"uid": "M1", "name": realMealName}},
			"mealtypes":          {"T1": {"uid": "T1", "name": "Dinner", "original_type": 2}},
		},
	}
	server := httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(server.Close)
	return server, api
}

func (a *fakeWriteAPI) serve(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()

	path := strings.TrimPrefix(r.URL.Path, syncPrefix)
	switch {
	case r.URL.Path == loginPath:
		_, _ = w.Write([]byte(`{"result":{"token":"` + testToken + `"}}`))
	case r.Header.Get("Authorization") != "Bearer "+testToken:
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid token.","code":0}}`))
	case r.Method == http.MethodPost:
		a.posts = append(a.posts, path)
		if a.failPost == path {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		a.store(path, r)
		_, _ = w.Write([]byte(`{"result":true}`))
	case path == "status/":
		_, _ = w.Write([]byte(`{"result":{"groceries":1,"grocerylists":1,"meals":1,"recipes":1}}`))
	case path == "recipes/":
		items := make([]object, 0)
		for range a.recipeCount {
			items = append(items, object{"uid": "R", "hash": "H"})
		}
		writeResult(w, items)
	case strings.HasPrefix(path, "recipe/"):
		recipe, ok := a.recipes[strings.TrimSuffix(strings.TrimPrefix(path, "recipe/"), "/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeResult(w, recipe)
	default:
		list, ok := a.lists[strings.TrimSuffix(path, "/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		items := make([]object, 0, len(list))
		for _, obj := range list {
			items = append(items, obj)
		}
		writeResult(w, items)
	}
}

func writeResult(w http.ResponseWriter, result any) {
	_ = json.NewEncoder(w).Encode(object{"result": result})
}

func (a *fakeWriteAPI) store(path string, r *http.Request) {
	if path == "notify/" {
		return
	}
	file, _, err := r.FormFile("data")
	if err != nil {
		return
	}
	defer file.Close()
	zipped, err := gzip.NewReader(file)
	if err != nil {
		return
	}
	raw, err := io.ReadAll(zipped)
	if err != nil {
		return
	}
	if strings.HasPrefix(path, "recipe/") {
		var recipe object
		if json.Unmarshal(raw, &recipe) == nil {
			delete(recipe, "deleted")
			a.recipes[recipe["uid"].(string)] = recipe
		}
		return
	}
	var objects []object
	if json.Unmarshal(raw, &objects) != nil {
		return
	}
	list := a.lists[strings.TrimSuffix(path, "/")]
	for _, obj := range objects {
		uid := obj["uid"].(string)
		if obj["deleted"] == true {
			delete(list, uid)
			continue
		}
		delete(obj, "deleted")
		list[uid] = obj
	}
}

func testOptions(t *testing.T) writeOptions {
	t.Helper()
	dir := t.TempDir()
	return writeOptions{
		email:          testEmail,
		confirmAccount: testEmail,
		backupPath:     filepath.Join(dir, "backup.local"),
		statePath:      filepath.Join(dir, "state.local"),
	}
}

func TestWriteChecksRefuseBeforeWriting(t *testing.T) {
	tests := []struct {
		name    string
		recipes int
		change  func(*writeOptions)
	}{
		{"missing confirmation", 0, func(o *writeOptions) { o.confirmAccount = "" }},
		{"wrong confirmation", 0, func(o *writeOptions) { o.confirmAccount = "other@example.com" }},
		{"large library", maxUnconfirmedLib + 1, func(*writeOptions) {}},
		{"backup could be committed", 0, func(o *writeOptions) { o.backupPath = "backup.json" }},
		{"state could be committed", 0, func(o *writeOptions) { o.statePath = "state.json" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, api := newFakeWriteAPI(t)
			api.recipeCount = tt.recipes
			opts := testOptions(t)
			tt.change(&opts)

			err := runWriteChecks(context.Background(), newWriteProber(server.URL, 0, &bytes.Buffer{}), testPassword, opts)
			if !errors.Is(err, errGuard) {
				t.Errorf("error = %v, want errGuard", err)
			}
			if len(api.posts) != 0 {
				t.Errorf("sent writes: %v", api.posts)
			}
		})
	}
}

func TestWriteChecksRunAndCleanUp(t *testing.T) {
	server, api := newFakeWriteAPI(t)
	api.recipeCount = maxUnconfirmedLib + 1
	opts := testOptions(t)
	opts.allowRealAcct = true

	var report bytes.Buffer
	if err := runWriteChecks(context.Background(), newWriteProber(server.URL, 0, &report), testPassword, opts); err != nil {
		t.Fatalf("runWriteChecks: %v\n%s", err, report.String())
	}

	text := report.String()
	for _, want := range []string{"W1", "W2", "W3", "W4", "W5", "W6", "W7", "W8", "W9", "Hash case", "Bad token", "original_type=2 name=Dinner"} {
		if !strings.Contains(text, want) {
			t.Errorf("report is missing %q:\n%s", want, text)
		}
	}
	for _, private := range []string{realMealName, testEmail, testPassword, testToken} {
		if strings.Contains(text, private) {
			t.Errorf("report contains %q", private)
		}
	}

	backup, err := os.ReadFile(opts.backupPath)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if !strings.Contains(string(backup), realMealName) {
		t.Error("backup does not contain existing meals")
	}

	if err := runCleanup(context.Background(), newWriteProber(server.URL, 0, &report), testPassword, opts); err != nil {
		t.Fatalf("runCleanup: %v", err)
	}
	if got := len(api.lists["groceries"]); got != 0 {
		t.Errorf("%d grocery items remain", got)
	}
	if got := len(api.lists["grocerylists"]); got != 0 {
		t.Errorf("%d grocery lists remain", got)
	}
	if _, ok := api.lists["meals"]["M1"]; !ok {
		t.Error("cleanup removed an existing meal")
	}
	if got := len(api.lists["meals"]); got != 1 {
		t.Errorf("%d meals remain, want only the existing one", got)
	}
	if _, err := os.Stat(opts.statePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("state file remains: %v", err)
	}
}

func TestInspectOnlyReads(t *testing.T) {
	server, api := newFakeWriteAPI(t)
	opts := testOptions(t)
	if err := runWriteChecks(context.Background(), newWriteProber(server.URL, 0, &bytes.Buffer{}), testPassword, opts); err != nil {
		t.Fatalf("runWriteChecks: %v", err)
	}
	writes := len(api.posts)

	var report bytes.Buffer
	if err := runInspect(context.Background(), newProber(server.URL, 0, &report), testEmail, testPassword, opts.statePath); err != nil {
		t.Fatalf("runInspect: %v", err)
	}
	if len(api.posts) != writes {
		t.Errorf("inspect sent %d writes", len(api.posts)-writes)
	}
	text := report.String()
	if !strings.Contains(text, "W5 unknown aisle") || !strings.Contains(text, "aisle_uid known=false") {
		t.Errorf("report does not describe items:\n%s", text)
	}
	if strings.Contains(text, realMealName) {
		t.Error("report contains an existing meal")
	}
}

func TestFollowupStages(t *testing.T) {
	server, api := newFakeWriteAPI(t)
	opts := testOptions(t)
	if err := runWriteChecks(context.Background(), newWriteProber(server.URL, 0, &bytes.Buffer{}), testPassword, opts); err != nil {
		t.Fatalf("runWriteChecks: %v", err)
	}
	countRestoreItems := func() int {
		count := 0
		for _, obj := range api.lists["groceries"] {
			if obj["name"] == testPrefix+" W6 restore" {
				count++
			}
		}
		return count
	}

	if err := runFollowup(context.Background(), newWriteProber(server.URL, 0, &bytes.Buffer{}), testPassword, opts, 1); err != nil {
		t.Fatalf("stage 1: %v", err)
	}
	if got := countRestoreItems(); got != 0 {
		t.Errorf("stage 1 left %d restore items", got)
	}
	if _, ok := api.lists["meals"]["M1"]; !ok || api.lists["meals"]["M1"]["name"] != realMealName {
		t.Error("stage 1 changed an existing meal")
	}

	if err := runFollowup(context.Background(), newWriteProber(server.URL, 0, &bytes.Buffer{}), testPassword, opts, 2); err != nil {
		t.Fatalf("stage 2: %v", err)
	}
	if got := countRestoreItems(); got != 1 {
		t.Errorf("stage 2 left %d restore items, want 1", got)
	}
	if err := runFollowup(context.Background(), newWriteProber(server.URL, 0, &bytes.Buffer{}), testPassword, opts, 3); !errors.Is(err, errGuard) {
		t.Errorf("stage 3 error = %v, want errGuard", err)
	}
}

func TestWriteChecksStopOnServerError(t *testing.T) {
	server, api := newFakeWriteAPI(t)
	api.failPost = "groceries/"

	err := runWriteChecks(context.Background(), newWriteProber(server.URL, 0, &bytes.Buffer{}), testPassword, testOptions(t))
	if !errors.Is(err, errUnexpected) {
		t.Fatalf("error = %v, want errUnexpected", err)
	}
	if got := api.posts[len(api.posts)-1]; got != "groceries/" {
		t.Errorf("last write = %s, want the failed one", got)
	}
	failures := 0
	for _, post := range api.posts {
		if post == "groceries/" {
			failures++
		}
	}
	if failures != 1 {
		t.Errorf("sent %d grocery writes, want no retry", failures)
	}
}

func TestAssertOwnedRefusesOtherObjects(t *testing.T) {
	c := &checker{objects: []stateObject{{"meals", "OWNED"}}}
	tests := []struct {
		name string
		obj  object
	}{
		{"unknown uid", object{"uid": "M1", "name": testPrefix + " meal"}},
		{"real name", object{"uid": "OWNED", "name": realMealName}},
		{"no name", object{"uid": "OWNED"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := c.assertOwned("meals", []object{tt.obj}); !errors.Is(err, errNotOwned) {
				t.Errorf("error = %v, want errNotOwned", err)
			}
		})
	}
	if err := c.assertOwned("meals", []object{{"uid": "OWNED", "name": testPrefix + " meal"}}); err != nil {
		t.Errorf("owned object refused: %v", err)
	}
}

func TestWriteTransportAllowsOnlyListedRoutes(t *testing.T) {
	server, _ := newFakeWriteAPI(t)
	prober := newWriteProber(server.URL, 0, &bytes.Buffer{})
	tests := []struct {
		method, path string
		allowed      bool
	}{
		{http.MethodPost, syncPrefix + "meals/", true},
		{http.MethodPost, syncPrefix + "recipe/ABC/", true},
		{http.MethodPost, syncPrefix + "categories/", false},
		{http.MethodPost, syncPrefix + "recipes/", false},
		{http.MethodPost, syncPrefix + "groceryaisles/", false},
		{http.MethodDelete, syncPrefix + "meals/", false},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			req, err := http.NewRequest(tt.method, server.URL+tt.path, nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			_, err = prober.client.Do(req)
			if blocked := errors.Is(err, errWriteBlocked); blocked == tt.allowed {
				t.Errorf("blocked = %v, want allowed = %v", blocked, tt.allowed)
			}
		})
	}
}
