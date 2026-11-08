package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var goodHash = strings.Repeat("a", 64)

func writeEnv(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secrets.env")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadParameters(t *testing.T) {
	full := "MCP_TOKEN_SHA256=" + goodHash + "\nPAPRIKA_EMAIL=a@b.c\nPAPRIKA_PASSWORD=pw\n"
	allNames := []string{"mcp-token-sha256", "paprika-email", "paprika-password"}
	tests := []struct {
		name    string
		env     string
		only    []string
		want    []string
		wantErr string
	}{
		{name: "all", env: full, want: allNames},
		{name: "only", env: full, only: []string{"PAPRIKA_EMAIL"}, want: []string{"paprika-email"}},
		{name: "two hashes", env: strings.Replace(full, goodHash, goodHash+","+goodHash, 1), want: allNames},
		{name: "missing", env: "PAPRIKA_EMAIL=a@b.c\n", wantErr: "MCP_TOKEN_SHA256 is empty"},
		{name: "bad hash", env: strings.Replace(full, goodHash, "nothex", 1), wantErr: "MCP_TOKEN_SHA256"},
		{name: "three hashes", env: strings.Replace(full, goodHash, strings.Join([]string{goodHash, goodHash, goodHash}, ","), 1), wantErr: "one or two"},
		{name: "unknown only", env: full, only: []string{"NOPE"}, wantErr: "unknown variable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params, err := LoadParameters(writeEnv(t, tt.env), tt.only)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(params) != len(tt.want) {
				t.Fatalf("got %d parameters, want %d", len(params), len(tt.want))
			}
			for i, p := range params {
				if p.Name != ParameterPrefix+tt.want[i] {
					t.Errorf("parameter %d = %s", i, p.Name)
				}
			}
		})
	}
}

func TestErrorsDoNotLeakValues(t *testing.T) {
	_, err := LoadParameters(writeEnv(t, "MCP_TOKEN_SHA256=secret-not-hex\n"), nil)
	if err == nil || strings.Contains(err.Error(), "secret-not-hex") {
		t.Fatalf("error should name the variable only: %v", err)
	}
}

func TestPutParameterInput(t *testing.T) {
	p := Parameter{Name: "/x", Value: "v"}
	for _, overwrite := range []bool{false, true} {
		raw, err := putParameterInput(p, overwrite)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if got["Type"] != "SecureString" || got["Name"] != "/x" || got["Value"] != "v" {
			t.Errorf("unexpected input %v", got)
		}
		if _, has := got["Overwrite"]; has != overwrite {
			t.Errorf("overwrite=%v but Overwrite present=%v", overwrite, has)
		}
	}
}

func TestNewToken(t *testing.T) {
	token, hash, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(token))
	if hash != hex.EncodeToString(sum[:]) {
		t.Error("hash does not match token")
	}
	if err := validateTokenHashes(hash); err != nil {
		t.Error(err)
	}
}

func TestPutParametersRunsAWS(t *testing.T) {
	var calls [][]string
	runner = func(args []string) (string, error) {
		calls = append(calls, args)
		return "", nil
	}
	params := []Parameter{{Name: "/a", Value: "v1"}, {Name: "/b", Value: "v2"}}
	if err := PutParameters(params, "us-east-1", "prof", true, false); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("got %d calls", len(calls))
	}
	joined := strings.Join(calls[0], " ")
	if !strings.Contains(joined, "--profile prof") || !strings.Contains(joined, "--region us-east-1") {
		t.Errorf("args = %s", joined)
	}
	if strings.Contains(joined, "v1") {
		t.Error("value must not be on the command line")
	}

	calls = nil
	if err := PutParameters(params, "us-east-1", "", false, true); err != nil || len(calls) != 0 {
		t.Errorf("dry run called aws: %v %v", err, calls)
	}
}
