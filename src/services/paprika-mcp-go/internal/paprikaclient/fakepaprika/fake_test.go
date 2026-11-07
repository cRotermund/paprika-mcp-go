package fakepaprika

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type rawClient struct {
	t     *testing.T
	s     *Server
	token string
}

func newRawClient(t *testing.T, s *Server) *rawClient {
	t.Helper()
	c := &rawClient{t: t, s: s}
	resp, err := http.PostForm(
		s.URL()+loginPath,
		url.Values{"email": {s.Email()}, "password": {s.Password()}},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Result struct{ Token string }
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.Result.Token == "" {
		t.Fatalf("login: %v %+v", err, body)
	}
	c.token = body.Result.Token
	return c
}

func (c *rawClient) do(
	method, path string,
	body io.Reader,
	contentType string,
) (int, json.RawMessage) {
	c.t.Helper()
	req, err := http.NewRequest(method, c.s.URL()+path, body)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var env struct {
		Result json.RawMessage `json:"result"`
	}
	data, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(data, &env)
	return resp.StatusCode, env.Result
}

func (c *rawClient) post(path string, payload any) (int, json.RawMessage) {
	c.t.Helper()
	raw, _ := json.Marshal(payload)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(raw)
	_ = zw.Close()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, _ := mw.CreateFormFile("data", "file")
	_, _ = part.Write(gz.Bytes())
	_ = mw.Close()
	return c.do(http.MethodPost, path, &body, mw.FormDataContentType())
}

func TestWriteBehavior(t *testing.T) {
	s := New(t)
	s.SeedDefault()
	c := newRawClient(t, s)
	base := s.Counters()

	t.Run("bulk counters rise per object and merge keeps omitted fields", func(t *testing.T) {
		status, _ := c.post("/api/v2/sync/groceries/", []map[string]any{
			{
				"uid":       "ITEM0000-0000-4000-8000-000000000001",
				"purchased": true,
				"list_uid":  "UNKNOWN-LIST",
			},
			{"uid": "NEW-ITEM", "name": "flour", "list_uid": "UNKNOWN-LIST"},
		})
		if status != 200 {
			t.Fatalf("status = %d", status)
		}
		if got := s.Counters()["groceries"] - base["groceries"]; got != 2 {
			t.Fatalf("counter rose by %d, want 2", got)
		}
		items := s.Objects("groceries")
		if len(items) != 2 {
			t.Fatalf("items = %d, want 2", len(items))
		}
		if items[0]["name"] != "carrots" || items[0]["purchased"] != true || items[0]["list_uid"] != "UNKNOWN-LIST" {
			t.Fatalf("merged item = %v", items[0])
		}
	})

	t.Run("deleted objects leave the list and no object lists deleted", func(t *testing.T) {
		c.post("/api/v2/sync/groceries/", []map[string]any{{"uid": "NEW-ITEM", "deleted": true}})
		for _, o := range s.Objects("groceries") {
			if o["uid"] == "NEW-ITEM" {
				t.Fatal("deleted item still listed")
			}
			if _, has := o["deleted"]; has {
				t.Fatalf("object lists deleted: %v", o)
			}
		}
	})

	t.Run("stale recipe write is accepted and trashed recipes are listed", func(t *testing.T) {
		stale := map[string]any{
			"uid":  RecipeSoupUID,
			"hash": strings.Repeat("0", 64),
			"name": "Stale",
		}
		if status, _ := c.post("/api/v2/sync/recipe/"+RecipeSoupUID+"/", stale); status != 200 {
			t.Fatalf("status = %d", status)
		}
		stored, _ := s.StoredRecipe(RecipeSoupUID)
		if stored["name"] != "Stale" || stored["directions"] == nil {
			t.Fatalf("stored = %v", stored)
		}
		_, list := c.do(http.MethodGet, "/api/v2/sync/recipes/", nil, "")
		var refs []map[string]string
		if err := json.Unmarshal(list, &refs); err != nil || len(refs) != 2 {
			t.Fatalf("recipe list = %s, %v", list, err)
		}
		if got := s.Counters()["recipes"] - base["recipes"]; got != 1 {
			t.Fatalf("recipes counter rose by %d, want 1", got)
		}
	})

	t.Run("unknown recipe is a 200 error envelope", func(t *testing.T) {
		status, result := c.do(http.MethodGet, "/api/v2/sync/recipe/NOPE/", nil, "")
		if status != 200 || len(result) != 0 {
			t.Fatalf("status = %d, result = %s", status, result)
		}
	})
}

func TestAuthAndFaults(t *testing.T) {
	s := New(t)
	c := newRawClient(t, s)

	if status, _ := c.do(http.MethodGet, "/api/v2/sync/status/", nil, ""); status != 200 {
		t.Fatalf("status = %d", status)
	}
	s.RevokeTokens()
	if status, _ := c.do(http.MethodGet, "/api/v2/sync/status/", nil, ""); status != 401 {
		t.Fatalf("revoked token status = %d, want 401", status)
	}

	c = newRawClient(t, s)
	s.Inject(Fault{Path: "/api/v2/sync/status/", Kind: FaultUnavailable})
	if status, _ := c.do(http.MethodGet, "/api/v2/sync/status/", nil, ""); status != 503 {
		t.Fatalf("faulted status = %d, want 503", status)
	}
	if status, _ := c.do(http.MethodGet, "/api/v2/sync/status/", nil, ""); status != 200 {
		t.Fatalf("fault should be used up, got %d", status)
	}

	for _, r := range s.Requests() {
		if r.Header.Get("Authorization") != "" && r.Header.Get("Authorization") != "[redacted]" {
			t.Fatalf("authorization value recorded: %q", r.Header.Get("Authorization"))
		}
	}
}
