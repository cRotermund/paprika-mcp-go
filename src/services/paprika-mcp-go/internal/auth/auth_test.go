package auth

import (
	"context"
	"errors"
	"strings"
	"testing"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

func TestParseHashes(t *testing.T) {
	a := HashToken("a")
	b := HashToken("b")
	c := HashToken("c")

	tests := []struct {
		name    string
		raw     string
		want    []string
		wantErr bool
	}{
		{name: "one hash", raw: a, want: []string{a}},
		{name: "two hashes with spaces", raw: a + " , " + b, want: []string{a, b}},
		{name: "uppercase is normalized", raw: strings.ToUpper(a), want: []string{a}},
		{name: "empty", raw: "", wantErr: true},
		{name: "blank", raw: "   ", wantErr: true},
		{name: "three hashes", raw: a + "," + b + "," + c, wantErr: true},
		{name: "too short", raw: a[:63], wantErr: true},
		{name: "too long", raw: a + "0", wantErr: true},
		{name: "not hex", raw: strings.Repeat("z", 64), wantErr: true},
		{name: "trailing comma", raw: a + ",", wantErr: true},
		{name: "raw token instead of hash", raw: "my-secret-token", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseHashes(tt.raw)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseHashes() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				if strings.Contains(err.Error(), "my-secret-token") {
					t.Errorf("error leaks the value: %v", err)
				}
				return
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("ParseHashes() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestVerify(t *testing.T) {
	v, err := NewVerifier([]string{HashToken("current"), HashToken("next")})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		token string
		valid bool
	}{
		{name: "first hash", token: "current", valid: true},
		{name: "second hash for rotation", token: "next", valid: true},
		{name: "wrong token", token: "other"},
		{name: "empty token", token: ""},
		{name: "hash itself is not a token", token: HashToken("current")},
		{name: "case differs", token: "Current"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := v.Verify(context.Background(), tt.token, nil)
			if tt.valid {
				if err != nil {
					t.Fatalf("Verify() error = %v", err)
				}
				if info.Expiration.IsZero() {
					t.Error("Verify() returned no expiration, which the SDK middleware rejects")
				}
				return
			}
			if !errors.Is(err, sdkauth.ErrInvalidToken) {
				t.Errorf("Verify() error = %v, want ErrInvalidToken", err)
			}
			if info != nil {
				t.Error("Verify() returned token info for an invalid token")
			}
		})
	}
}

func TestNewVerifierRejectsBadInput(t *testing.T) {
	tests := []struct {
		name   string
		hashes []string
	}{
		{name: "none", hashes: nil},
		{name: "three", hashes: []string{HashToken("a"), HashToken("b"), HashToken("c")}},
		{name: "bad hex", hashes: []string{strings.Repeat("g", 64)}},
		{name: "wrong length", hashes: []string{"abcd"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewVerifier(tt.hashes); err == nil {
				t.Error("NewVerifier() succeeded, want an error")
			}
		})
	}
}
