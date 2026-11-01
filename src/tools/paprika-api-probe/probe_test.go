package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const (
	testEmail    = "person@example.com"
	testPassword = "correct-horse-battery"
	testToken    = "token-value-abc"
	testRecipe   = "11111111-2222-4333-8444-555555555555"
	secretTitle  = "Grandmas Secret Soup"
)

type recordedRequest struct {
	method string
	path   string
	auth   string
}

func newFakeAPI(t *testing.T) (*httptest.Server, func() []recordedRequest) {
	t.Helper()
	var (
		mu       sync.Mutex
		requests []recordedRequest
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, recordedRequest{r.Method, r.URL.Path, r.Header.Get("Authorization")})
		mu.Unlock()

		switch r.URL.Path {
		case loginPath:
			if r.FormValue("password") != testPassword {
				_, _ = w.Write([]byte(`{"error":{"message":"bad login","code":0}}`))
				return
			}
			_, _ = w.Write([]byte(`{"result":{"token":"` + testToken + `"}}`))
		case syncPrefix + "status/":
			_, _ = w.Write([]byte(`{"result":{"recipes":7,"meals":3,"newthing":1,"../evil":2}}`))
		case syncPrefix + "recipes/":
			_, _ = w.Write([]byte(`{"result":[{"uid":"` + testRecipe + `","hash":"ABC"}]}`))
		case syncPrefix + "meals/":
			_, _ = w.Write([]byte(`{"result":[{"uid":"` + testRecipe + `","name":"` + secretTitle + `","date":"2030-01-15 00:00:00","type":2,"deleted":false,"recipe_uid":null}]}`))
		case syncPrefix + "recipe/" + testRecipe + "/":
			_, _ = w.Write([]byte(`{"result":{"uid":"` + testRecipe + `","name":"` + secretTitle + `","categories":["` + testRecipe + `"]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("<html>" + secretTitle + "</html>"))
		}
	}))
	t.Cleanup(server.Close)
	return server, func() []recordedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedRequest(nil), requests...)
	}
}

func TestRunSendsOnlyReadsAfterLogin(t *testing.T) {
	server, requests := newFakeAPI(t)

	var out bytes.Buffer
	if err := newProber(server.URL, 0, &out).run(context.Background(), testEmail, testPassword, 5); err != nil {
		t.Fatalf("run: %v", err)
	}

	sawRecipe := false
	for i, request := range requests() {
		if i == 0 {
			if request.method != http.MethodPost || request.path != loginPath {
				t.Fatalf("first request = %s %s, want login", request.method, request.path)
			}
			continue
		}
		if request.method != http.MethodGet {
			t.Errorf("%s %s is not a read", request.method, request.path)
		}
		if request.auth != "Bearer "+testToken {
			t.Errorf("%s has Authorization %q", request.path, request.auth)
		}
		if strings.Contains(request.path, "evil") {
			t.Errorf("requested unsafe route %s", request.path)
		}
		if request.path == syncPrefix+"recipe/"+testRecipe+"/" {
			sawRecipe = true
		}
	}
	if !sawRecipe {
		t.Error("did not sample a full recipe")
	}
}

func TestReportOmitsPrivateValues(t *testing.T) {
	server, _ := newFakeAPI(t)

	var out bytes.Buffer
	if err := newProber(server.URL, 0, &out).run(context.Background(), testEmail, testPassword, 5); err != nil {
		t.Fatalf("run: %v", err)
	}

	report := out.String()
	for _, private := range []string{secretTitle, testEmail, testPassword, testToken, testRecipe} {
		if strings.Contains(report, private) {
			t.Errorf("report contains %q:\n%s", private, report)
		}
	}
	for _, want := range []string{"`name`", "date 9999-99-99 99:99:99", "`recipe_uid`", "null:1", "`newthing` = 1", "`categories[]`", "uuid-upper"} {
		if !strings.Contains(report, want) {
			t.Errorf("report is missing %q:\n%s", want, report)
		}
	}
}

func TestLoginFailureStopsBeforeReads(t *testing.T) {
	server, requests := newFakeAPI(t)

	var out bytes.Buffer
	err := newProber(server.URL, 0, &out).run(context.Background(), testEmail, "wrong-password", 5)
	if err == nil {
		t.Fatal("run succeeded with a bad password")
	}
	if strings.Contains(err.Error(), "wrong-password") {
		t.Errorf("error exposes the password: %v", err)
	}
	if got := len(requests()); got != 1 {
		t.Errorf("sent %d requests, want only the login", got)
	}
}

func TestTransportBlocksWrites(t *testing.T) {
	server, requests := newFakeAPI(t)
	prober := newProber(server.URL, 0, &bytes.Buffer{})

	tests := []struct {
		method string
		path   string
	}{
		{http.MethodPost, syncPrefix + "meals/"},
		{http.MethodPost, syncPrefix + "notify/"},
		{http.MethodPut, syncPrefix + "recipe/" + testRecipe + "/"},
		{http.MethodDelete, syncPrefix + "recipe/" + testRecipe + "/"},
		{http.MethodPut, loginPath},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			req, err := http.NewRequest(tt.method, server.URL+tt.path, strings.NewReader("{}"))
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			if _, err := prober.client.Do(req); !errors.Is(err, errWriteBlocked) {
				t.Errorf("error = %v, want errWriteBlocked", err)
			}
		})
	}
	if got := len(requests()); got != 0 {
		t.Errorf("%d blocked requests reached the server", got)
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	followed := false
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		followed = true
	}))
	defer other.Close()
	server := httptest.NewServer(http.RedirectHandler(other.URL, http.StatusFound))
	defer server.Close()

	prober := newProber(server.URL, 0, &bytes.Buffer{})
	prober.token = testToken
	res, err := prober.fetch(context.Background(), syncPrefix+"status/")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if res.status != http.StatusFound || followed {
		t.Errorf("status = %d, followed = %v", res.status, followed)
	}
}

func TestStringShape(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{"empty", "", "empty"},
		{"uuid upper", "AAAAAAAA-1111-4222-8333-444444444444", "uuid-upper"},
		{"uuid lower", "aaaaaaaa-1111-4222-8333-444444444444", "uuid-lower"},
		{"hash upper", strings.Repeat("A", 64), "sha256-hex-upper"},
		{"date only", "2030-01-15", "date 9999-99-99"},
		{"date time", "2030-01-15 10:20:30", "date 9999-99-99 99:99:99"},
		{"url", "https://example.com/private", "url"},
		{"multiline", "one\ntwo", "multiline-text"},
		{"text", secretTitle, "text"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stringShape(tt.value); got != tt.want {
				t.Errorf("stringShape(%q) = %q, want %q", tt.value, got, tt.want)
			}
		})
	}
}
