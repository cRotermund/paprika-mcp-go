package config

import (
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/auth"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/indexstore"
)

func TestLoad(t *testing.T) {
	hashA := auth.HashToken("a")
	hashB := auth.HashToken("b")

	tests := []struct {
		name        string
		port        string
		tokens      string
		origins     string
		writes      string
		wantPort    int
		wantTokens  []string
		wantOrigins []string
		wantWrites  bool
		wantErr     bool
	}{
		{name: "valid", port: "8080", tokens: hashA, wantPort: 8080, wantTokens: []string{hashA}},
		{
			name:       "two hashes",
			port:       "8080",
			tokens:     hashA + "," + hashB,
			wantPort:   8080,
			wantTokens: []string{hashA, hashB},
		},
		{
			name:        "origins",
			port:        "8080",
			tokens:      hashA,
			origins:     "https://Claude.ai, http://localhost:3000",
			wantPort:    8080,
			wantTokens:  []string{hashA},
			wantOrigins: []string{"https://claude.ai", "http://localhost:3000"},
		},
		{
			name:       "writes on",
			port:       "8080",
			tokens:     hashA,
			writes:     "true",
			wantPort:   8080,
			wantTokens: []string{hashA},
			wantWrites: true,
		},
		{
			name:       "writes off explicitly",
			port:       "8080",
			tokens:     hashA,
			writes:     "false",
			wantPort:   8080,
			wantTokens: []string{hashA},
		},
		{name: "missing port", tokens: hashA, wantErr: true},
		{name: "non-numeric port", port: "abc", tokens: hashA, wantErr: true},
		{name: "missing token hash", port: "8080", wantErr: true},
		{
			name:    "three token hashes",
			port:    "8080",
			tokens:  hashA + "," + hashB + "," + hashA,
			wantErr: true,
		},
		{name: "short token hash", port: "8080", tokens: hashA[:10], wantErr: true},
		{name: "raw token instead of hash", port: "8080", tokens: "plain-token", wantErr: true},
		{name: "bad origin", port: "8080", tokens: hashA, origins: "claude.ai", wantErr: true},
		{
			name:    "origin with path",
			port:    "8080",
			tokens:  hashA,
			origins: "https://claude.ai/app",
			wantErr: true,
		},
		{name: "bad writes flag", port: "8080", tokens: hashA, writes: "maybe", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvPort, tt.port)
			t.Setenv(EnvTokenSHA256, tt.tokens)
			t.Setenv(EnvAllowedOrigins, tt.origins)
			t.Setenv(EnvWritesEnabled, tt.writes)
			setAccount(t)

			cfg, err := Load(slog.Default())
			if (err != nil) != tt.wantErr {
				t.Fatalf("Load() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if cfg.Port != tt.wantPort {
				t.Errorf("Load() port = %d, want %d", cfg.Port, tt.wantPort)
			}
			if !slices.Equal(cfg.TokenSHA256, tt.wantTokens) {
				t.Errorf("Load() token hashes = %v, want %v", cfg.TokenSHA256, tt.wantTokens)
			}
			if !slices.Equal(cfg.AllowedOrigins, tt.wantOrigins) {
				t.Errorf("Load() origins = %v, want %v", cfg.AllowedOrigins, tt.wantOrigins)
			}
			if cfg.WritesEnabled != tt.wantWrites {
				t.Errorf("Load() writes = %v, want %v", cfg.WritesEnabled, tt.wantWrites)
			}
		})
	}
}

func TestLoadDoesNotLogTokenValue(t *testing.T) {
	var out strings.Builder
	logger := slog.New(slog.NewTextHandler(&out, nil))

	t.Setenv(EnvPort, "8080")
	t.Setenv(EnvTokenSHA256, "plain-secret-token")
	t.Setenv(EnvAllowedOrigins, "")
	t.Setenv(EnvWritesEnabled, "")
	setAccount(t)

	if _, err := Load(logger); err == nil {
		t.Fatal("Load() succeeded with a malformed token hash")
	}
	if strings.Contains(out.String(), "plain-secret-token") {
		t.Errorf("log output contains the configured value: %s", out.String())
	}
}

func TestLogLevelFromEnv(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  slog.Level
	}{
		{name: "debug", value: "debug", want: slog.LevelDebug},
		{name: "warn", value: "WARN", want: slog.LevelWarn},
		{name: "error", value: "Error", want: slog.LevelError},
		{name: "default", value: "", want: slog.LevelInfo},
		{name: "unknown falls back to info", value: "loud", want: slog.LevelInfo},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvLogLevel, tt.value)

			if got := LogLevelFromEnv(); got != tt.want {
				t.Errorf("LogLevelFromEnv() = %v, want %v", got, tt.want)
			}
		})
	}
}

func setAccount(t *testing.T) {
	t.Helper()
	t.Setenv(EnvPaprikaEmail, "cook@example.test")
	t.Setenv(EnvPaprikaPass, "s3cret-password")
	for _, name := range []string{
		"INDEX_STORE",
		"INDEX_FILE_PATH",
		"INDEX_S3_BUCKET",
		"INDEX_S3_KEY",
		"INDEX_S3_ENDPOINT",
	} {
		t.Setenv(name, "")
	}
}

func TestLoadAccountAndIndex(t *testing.T) {
	hash := auth.HashToken("a")
	tests := []struct {
		name     string
		httpMode bool
		env      map[string]string
		wantErr  bool
		check    func(t *testing.T, cfg *Config)
	}{
		{
			name:     "account is read",
			httpMode: true,
			env:      map[string]string{},
			check: func(t *testing.T, cfg *Config) {
				if cfg.PaprikaEmail != "cook@example.test" || cfg.PaprikaPassword != "s3cret-password" {
					t.Errorf("account = %q / %q", cfg.PaprikaEmail, cfg.PaprikaPassword)
				}
				if cfg.Index.Mode != indexstore.ModeNone {
					t.Errorf("index mode = %q, want none", cfg.Index.Mode)
				}
			},
		},
		{
			name:     "missing email",
			httpMode: true,
			env:      map[string]string{EnvPaprikaEmail: ""},
			wantErr:  true,
		},
		{
			name:     "missing password",
			httpMode: true,
			env:      map[string]string{EnvPaprikaPass: ""},
			wantErr:  true,
		},
		{
			name:     "stdio needs the account too",
			httpMode: false,
			env:      map[string]string{EnvPaprikaPass: ""},
			wantErr:  true,
		},
		{
			name:     "stdio needs no port or token",
			httpMode: false,
			env:      map[string]string{EnvPort: "", EnvTokenSHA256: ""},
		},
		{
			name:     "http needs a token",
			httpMode: true,
			env:      map[string]string{EnvTokenSHA256: ""},
			wantErr:  true,
		},
		{
			name:     "filesystem index",
			httpMode: true,
			env: map[string]string{
				"INDEX_STORE":     "filesystem",
				"INDEX_FILE_PATH": "/data/i.gz",
			},
			check: func(t *testing.T, cfg *Config) {
				if cfg.Index.Mode != indexstore.ModeFilesystem || cfg.Index.FilePath != "/data/i.gz" {
					t.Errorf("index = %+v", cfg.Index)
				}
			},
		},
		{
			name:     "unknown index mode stops startup",
			httpMode: true,
			env:      map[string]string{"INDEX_STORE": "redis"},
			wantErr:  true,
		},
		{
			name:     "filesystem without a path stops startup",
			httpMode: true,
			env:      map[string]string{"INDEX_STORE": "filesystem"},
			wantErr:  true,
		},
		{
			name:     "s3 without a bucket stops startup",
			httpMode: true,
			env:      map[string]string{"INDEX_STORE": "s3"},
			wantErr:  true,
		},
		{
			name:     "bad index config stops stdio too",
			httpMode: false,
			env:      map[string]string{"INDEX_STORE": "nope"},
			wantErr:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvPort, "8080")
			t.Setenv(EnvTokenSHA256, hash)
			t.Setenv(EnvAllowedOrigins, "")
			t.Setenv(EnvWritesEnabled, "")
			setAccount(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			var out strings.Builder
			logger := slog.New(slog.NewTextHandler(&out, nil))
			load := LoadStdio
			if tt.httpMode {
				load = Load
			}
			cfg, err := load(logger)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if strings.Contains(out.String(), "s3cret-password") {
				t.Errorf("log output contains the password: %s", out.String())
			}
			if err == nil && tt.check != nil {
				tt.check(t, cfg)
			}
		})
	}
}
