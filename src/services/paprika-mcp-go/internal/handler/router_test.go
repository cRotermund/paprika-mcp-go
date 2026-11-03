package handler

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/auth"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/mcpserver"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/middleware"
)

const (
	currentToken = "current-token-value"
	nextToken    = "next-token-value"
	initBody     = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
	pingBody     = `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ping","arguments":{}}}`
)

type harness struct {
	server *httptest.Server
	logs   *bytes.Buffer
}

func newHarness(t *testing.T, mutate func(*Options)) *harness {
	t.Helper()

	verifier, err := auth.NewVerifier([]string{auth.HashToken(currentToken), auth.HashToken(nextToken)})
	if err != nil {
		t.Fatal(err)
	}

	logs := &bytes.Buffer{}
	opts := Options{
		Logger:         slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		MCPServer:      mcpserver.New(nil, mcpserver.Options{}), // only ping is called here
		Verifier:       verifier.Verify,
		AllowedOrigins: []string{"https://claude.ai"},
	}
	if mutate != nil {
		mutate(&opts)
	}

	srv := httptest.NewServer(NewRouter(opts))
	t.Cleanup(srv.Close)

	return &harness{server: srv, logs: logs}
}

type request struct {
	method  string
	path    string
	body    string
	headers map[string]string
}

func (h *harness) do(t *testing.T, req request) (int, string) {
	t.Helper()

	if req.method == "" {
		req.method = http.MethodPost
	}
	if req.path == "" {
		req.path = "/mcp"
	}

	r, err := http.NewRequest(req.method, h.server.URL+req.path, strings.NewReader(req.body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range req.headers {
		r.Header.Set(k, v)
	}

	resp, err := h.server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	return resp.StatusCode, string(body)
}

func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

func TestAuthentication(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{name: "missing token", want: http.StatusUnauthorized},
		{name: "wrong token", headers: bearer("wrong"), want: http.StatusUnauthorized},
		{name: "current token", headers: bearer(currentToken), want: http.StatusOK},
		{name: "rotated second hash", headers: bearer(nextToken), want: http.StatusOK},
		{name: "malformed scheme", headers: map[string]string{"Authorization": "Basic " + currentToken}, want: http.StatusUnauthorized},
		{name: "no scheme", headers: map[string]string{"Authorization": currentToken}, want: http.StatusUnauthorized},
		{name: "extra fields", headers: map[string]string{"Authorization": "Bearer " + currentToken + " extra"}, want: http.StatusUnauthorized},
		{name: "empty bearer", headers: map[string]string{"Authorization": "Bearer "}, want: http.StatusUnauthorized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, nil)

			got, _ := h.do(t, request{body: initBody, headers: tt.headers})
			if got != tt.want {
				t.Errorf("status = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestNoToolRunsUnauthenticated(t *testing.T) {
	h := newHarness(t, nil)

	for _, headers := range []map[string]string{nil, bearer("wrong"), {"Authorization": "garbage"}} {
		code, body := h.do(t, request{body: pingBody, headers: headers})
		if code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", code)
		}
		if strings.Contains(body, "pong") {
			t.Errorf("tool output reached an unauthenticated client: %s", body)
		}
	}
}

type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func TestAuthenticatedPing(t *testing.T) {
	h := newHarness(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   h.server.URL + "/mcp",
		HTTPClient: &http.Client{Transport: bearerTransport{token: currentToken}},
	}, nil)
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	defer session.Close()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "ping"})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if res.IsError {
		t.Fatalf("ping returned an error: %+v", res.Content)
	}
	if !strings.Contains(h.logs.String(), `"path":"/mcp"`) {
		t.Errorf("expected request log lines, got: %s", h.logs.String())
	}
}

func TestOriginRejection(t *testing.T) {
	tests := []struct {
		name   string
		origin string
		want   int
	}{
		{name: "no origin", want: http.StatusOK},
		{name: "allowed origin", origin: "https://claude.ai", want: http.StatusOK},
		{name: "unknown origin", origin: "https://evil.example", want: http.StatusForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, nil)
			headers := bearer(currentToken)
			if tt.origin != "" {
				headers["Origin"] = tt.origin
			}

			got, _ := h.do(t, request{body: initBody, headers: headers})
			if got != tt.want {
				t.Errorf("status = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestBodyLimit(t *testing.T) {
	h := newHarness(t, nil)
	padding := strings.Repeat("a", DefaultMaxBodyBytes)
	oversized := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"x":"` + padding + `"}}`

	got, _ := h.do(t, request{body: oversized, headers: bearer(currentToken)})
	if got != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d", got, http.StatusRequestEntityTooLarge)
	}

	got, _ = h.do(t, request{body: initBody, headers: bearer(currentToken)})
	if got != http.StatusOK {
		t.Errorf("normal request: status = %d, want 200", got)
	}
}

func TestFailedAuthRateLimit(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.FailedAuthLimiter = middleware.NewFailureLimiter(3, time.Minute)
	})
	guesser := map[string]string{"Authorization": "Bearer guess", "X-Forwarded-For": "198.51.100.9"}

	for i := range 3 {
		if got, _ := h.do(t, request{body: initBody, headers: guesser}); got != http.StatusUnauthorized {
			t.Fatalf("guess %d: status = %d, want 401", i+1, got)
		}
	}

	if got, _ := h.do(t, request{body: initBody, headers: guesser}); got != http.StatusTooManyRequests {
		t.Errorf("after limit: status = %d, want 429", got)
	}

	correct := bearer(currentToken)
	correct["X-Forwarded-For"] = "198.51.100.9"
	if got, _ := h.do(t, request{body: initBody, headers: correct}); got != http.StatusTooManyRequests {
		t.Errorf("blocked IP with a correct token: status = %d, want 429", got)
	}

	correct["X-Forwarded-For"] = "198.51.100.10"
	if got, _ := h.do(t, request{body: initBody, headers: correct}); got != http.StatusOK {
		t.Errorf("other IP: status = %d, want 200", got)
	}

	logged := h.logs.String()
	if !strings.Contains(logged, `"client_ip":"198.51.100.9"`) || !strings.Contains(logged, "authentication failed") {
		t.Errorf("failed auth was not logged with the client IP: %s", logged)
	}
}

func TestGlobalRateLimit(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.GlobalLimiter = middleware.NewTokenBucket(0.001, 2)
	})

	for i := range 2 {
		if got, _ := h.do(t, request{body: initBody, headers: bearer(currentToken)}); got != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i+1, got)
		}
	}
	if got, _ := h.do(t, request{body: initBody, headers: bearer(currentToken)}); got != http.StatusTooManyRequests {
		t.Errorf("over burst: status = %d, want 429", got)
	}
}

func TestUnauthenticatedRequestsDoNotUseGlobalLimit(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.GlobalLimiter = middleware.NewTokenBucket(0.001, 1)
		o.FailedAuthLimiter = middleware.NewFailureLimiter(1000, time.Minute)
	})

	for range 5 {
		h.do(t, request{body: initBody})
	}

	if got, _ := h.do(t, request{body: initBody, headers: bearer(currentToken)}); got != http.StatusOK {
		t.Errorf("authenticated request: status = %d, want 200", got)
	}
}

func TestRoutes(t *testing.T) {
	h := newHarness(t, nil)

	tests := []struct {
		name string
		req  request
		want int
	}{
		{name: "health needs no auth", req: request{method: http.MethodGet, path: "/health"}, want: http.StatusOK},
		{name: "root", req: request{method: http.MethodGet, path: "/"}, want: http.StatusNotFound},
		{name: "unknown path", req: request{method: http.MethodGet, path: "/admin"}, want: http.StatusNotFound},
		{name: "unknown path with token", req: request{path: "/other", headers: bearer(currentToken)}, want: http.StatusNotFound},
		{name: "mcp subpath", req: request{path: "/mcp/extra", headers: bearer(currentToken)}, want: http.StatusNotFound},
		{name: "metrics", req: request{method: http.MethodGet, path: "/metrics"}, want: http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, body := h.do(t, tt.req)
			if got != tt.want {
				t.Errorf("status = %d, want %d", got, tt.want)
			}
			if tt.req.path == "/health" && strings.TrimSpace(body) != `{"status":"healthy"}` {
				t.Errorf("health body = %q", body)
			}
		})
	}
}

func TestTokensNeverAppearInLogs(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.FailedAuthLimiter = middleware.NewFailureLimiter(2, time.Minute)
	})
	secrets := []string{"guessed-token-xyz", currentToken, nextToken}

	h.do(t, request{body: initBody, headers: bearer(secrets[0])})
	h.do(t, request{body: initBody, headers: map[string]string{"Authorization": "Basic " + secrets[0]}})
	h.do(t, request{body: initBody, headers: bearer(secrets[0])})
	h.do(t, request{body: initBody, headers: bearer(currentToken)})
	h.do(t, request{body: initBody, headers: bearer(nextToken)})
	h.do(t, request{body: initBody, headers: map[string]string{"Authorization": "Bearer " + secrets[0], "Origin": "https://evil.example"}})

	logged := h.logs.String()
	if logged == "" {
		t.Fatal("expected log output")
	}
	for _, secret := range secrets {
		if strings.Contains(logged, secret) {
			t.Errorf("log output contains %q: %s", secret, logged)
		}
	}
	for _, header := range []string{"Bearer ", "Authorization", "authorization"} {
		if strings.Contains(logged, header) {
			t.Errorf("log output contains %q: %s", header, logged)
		}
	}
}
