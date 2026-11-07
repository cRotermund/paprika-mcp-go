// Package testsupport holds helpers shared by tests. Production code must not import it.
package testsupport

import (
	"io"
	"log/slog"
	"testing"

	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/paprikaclient"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/paprikaclient/fakepaprika"
)

// Client returns a client for the fake with request pacing turned off.
func Client(tb testing.TB, fake *fakepaprika.Server, logger *slog.Logger) *paprikaclient.Client {
	tb.Helper()
	return ClientWith(tb, fake, logger, nil)
}

// ClientWith is [Client] with a chance to change the configuration, such as a
// short request timeout.
func ClientWith(
	tb testing.TB,
	fake *fakepaprika.Server,
	logger *slog.Logger,
	tweak func(*paprikaclient.Config),
) *paprikaclient.Client {
	tb.Helper()
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	cfg := paprikaclient.Config{
		BaseURL:      fake.URL(),
		Email:        fake.Email(),
		Password:     fake.Password(),
		RequestLimit: -1,
		Logger:       logger,
	}
	if tweak != nil {
		tweak(&cfg)
	}
	c, err := paprikaclient.New(cfg)
	if err != nil {
		tb.Fatal(err)
	}
	return c
}

// Recipe returns a synthetic recipe with the given fields. Its hash is derived
// from the name and ingredients, so changing either changes the hash.
func Recipe(uid, name, ingredients string, categories ...string) map[string]any {
	r := fakepaprika.SampleRecipe(uid, name)
	r["ingredients"] = ingredients
	cats := make([]any, len(categories))
	for i, c := range categories {
		cats[i] = c
	}
	r["categories"] = cats
	r["hash"] = Hash(name + "|" + ingredients)
	return r
}

// Hash returns a 64-character hex-looking opaque string for s.
func Hash(s string) string {
	const hex = "0123456789ABCDEF"
	out := make([]byte, 64)
	h := uint32(2166136261)
	for i := range out {
		if i < len(s) {
			h = (h ^ uint32(s[i])) * 16777619
		} else {
			h = h*16777619 + uint32(i)
		}
		out[i] = hex[h&15]
	}
	return string(out)
}
