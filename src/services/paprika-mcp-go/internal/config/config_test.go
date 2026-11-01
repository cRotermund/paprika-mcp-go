package config

import (
	"log/slog"
	"testing"
)

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		port    string
		want    int
		wantErr bool
	}{
		{name: "valid port", port: "8080", want: 8080},
		{name: "missing port", port: "", wantErr: true},
		{name: "non-numeric port", port: "abc", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvPort, tt.port)

			cfg, err := Load(slog.Default())
			if (err != nil) != tt.wantErr {
				t.Fatalf("Load() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && cfg.Port != tt.want {
				t.Errorf("Load() port = %d, want %d", cfg.Port, tt.want)
			}
		})
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
