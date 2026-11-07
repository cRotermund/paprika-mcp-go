package config

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"

	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/auth"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/indexstore"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/paprikaclient"
)

const (
	EnvPort           = "PORT"
	EnvLogLevel       = "LOG_LEVEL"
	EnvTokenSHA256    = "MCP_TOKEN_SHA256"
	EnvAllowedOrigins = "MCP_ALLOWED_ORIGINS"
	EnvWritesEnabled  = "PAPRIKA_WRITES_ENABLED"
	EnvPaprikaEmail   = "PAPRIKA_EMAIL"
	EnvPaprikaPass    = "PAPRIKA_PASSWORD"
	EnvRequestsPerMin = "PAPRIKA_REQUESTS_PER_MINUTE"
	EnvRequestBurst   = "PAPRIKA_REQUEST_BURST"
)

type Config struct {
	Port     int
	LogLevel slog.Level

	// TokenSHA256 holds one or two lowercase hex SHA-256 digests of accepted
	// MCP bearer tokens. HTTP mode cannot start without it.
	TokenSHA256 []string

	// AllowedOrigins lists the Origin values accepted on /mcp, normalized.
	AllowedOrigins []string

	// WritesEnabled is the write kill switch. It defaults to false.
	WritesEnabled bool

	// PaprikaEmail and PaprikaPassword are the Paprika account credentials.
	// They are secrets: never log them.
	PaprikaEmail    string
	PaprikaPassword string

	// RequestsPerMinute and RequestBurst size the Paprika client's token
	// bucket (PAPRIKA_REQUESTS_PER_MINUTE, PAPRIKA_REQUEST_BURST).
	RequestsPerMinute int
	RequestBurst      int

	// Index selects where the recipe index snapshot is kept (INDEX_* variables).
	// The caller sets Index.FormatVersion.
	Index indexstore.Config
}

// LoadDotenv loads a .env file from the working directory when one exists.
// A missing file is not an error: containers receive real environment
// variables, and real variables always win over .env values.
func LoadDotenv() error {
	if err := godotenv.Load(); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	return nil
}

// Load reads the configuration for HTTP mode. It needs PORT and
// MCP_TOKEN_SHA256 as well as everything LoadStdio needs.
func Load(logger *slog.Logger) (*Config, error) {
	return load(logger, true)
}

// LoadStdio reads the configuration for -stdio mode, which has no listener and
// no inbound authentication, so PORT and MCP_TOKEN_SHA256 are not needed.
func LoadStdio(logger *slog.Logger) (*Config, error) {
	return load(logger, false)
}

func load(logger *slog.Logger, httpMode bool) (*Config, error) {
	cfg := &Config{LogLevel: LogLevelFromEnv()}

	if httpMode {
		port, err := requiredInt(EnvPort, logger)
		if err != nil {
			return nil, err
		}
		cfg.Port = port

		cfg.TokenSHA256, err = tokenHashes(logger)
		if err != nil {
			return nil, err
		}

		cfg.AllowedOrigins, err = ParseOrigins(os.Getenv(EnvAllowedOrigins))
		if err != nil {
			logger.Error("invalid environment variable", "name", EnvAllowedOrigins, "error", err)
			return nil, fmt.Errorf("environment variable %s is invalid: %w", EnvAllowedOrigins, err)
		}
	}

	var err error
	if cfg.PaprikaEmail, err = requiredString(EnvPaprikaEmail, logger); err != nil {
		return nil, err
	}
	if cfg.PaprikaPassword, err = requiredString(EnvPaprikaPass, logger); err != nil {
		return nil, err
	}

	cfg.WritesEnabled, err = optionalBool(EnvWritesEnabled, false, logger)
	if err != nil {
		return nil, err
	}

	cfg.RequestsPerMinute, err = positiveInt(
		EnvRequestsPerMin,
		paprikaclient.DefaultRequestsPerMinute,
		logger,
	)
	if err != nil {
		return nil, err
	}
	cfg.RequestBurst, err = positiveInt(EnvRequestBurst, paprikaclient.DefaultRequestBurst, logger)
	if err != nil {
		return nil, err
	}
	if cfg.RequestBurst <= paprikaclient.BackgroundReserve {
		logger.Error(
			"request burst must exceed the background reserve",
			"name",
			EnvRequestBurst,
			"reserve",
			paprikaclient.BackgroundReserve,
		)
		return nil, fmt.Errorf(
			"environment variable %s must be greater than %d",
			EnvRequestBurst,
			paprikaclient.BackgroundReserve,
		)
	}

	cfg.Index, err = indexstore.ConfigFromEnv(os.Getenv)
	if err != nil {
		logger.Error("invalid index snapshot configuration", "error", err)
		return nil, fmt.Errorf("index snapshot configuration is invalid: %w", err)
	}

	return cfg, nil
}

func LogLevelFromEnv() slog.Level {
	switch strings.ToUpper(os.Getenv(EnvLogLevel)) {
	case "DEBUG":
		return slog.LevelDebug
	case "WARN":
		return slog.LevelWarn
	case "ERROR":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func requiredString(name string, logger *slog.Logger) (string, error) {
	value := os.Getenv(name)
	if value == "" {
		logger.Error("required environment variable is missing", "name", name)
		return "", fmt.Errorf("required environment variable %s is not set", name)
	}

	return value, nil
}

func requiredInt(name string, logger *slog.Logger) (int, error) {
	raw, err := requiredString(name, logger)
	if err != nil {
		return 0, err
	}

	value, err := strconv.Atoi(raw)
	if err != nil {
		logger.Error(
			"could not parse environment variable as an integer",
			"name",
			name,
			"value",
			raw,
		)
		return 0, fmt.Errorf("environment variable %s is not an integer: %w", name, err)
	}

	return value, nil
}

// tokenHashes reads and validates MCP_TOKEN_SHA256. The value is never logged.
func tokenHashes(logger *slog.Logger) ([]string, error) {
	raw, err := requiredString(EnvTokenSHA256, logger)
	if err != nil {
		return nil, err
	}

	hashes, err := auth.ParseHashes(raw)
	if err != nil {
		logger.Error("invalid environment variable", "name", EnvTokenSHA256, "error", err)
		return nil, fmt.Errorf("environment variable %s is invalid: %w", EnvTokenSHA256, err)
	}

	return hashes, nil
}

// ParseOrigins parses a comma-separated list of origins such as
// "https://claude.ai". Each must be a scheme and host with no path. Empty
// input yields no origins.
func ParseOrigins(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	var origins []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		u, err := url.Parse(part)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
			u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return nil, fmt.Errorf("origin %q must look like https://host[:port]", part)
		}
		origins = append(origins, strings.ToLower(u.Scheme+"://"+u.Host))
	}

	return origins, nil
}

// positiveInt reads an optional integer that must be greater than zero.
func positiveInt(name string, fallback int, logger *slog.Logger) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}

	value, err := strconv.Atoi(raw)
	if err != nil {
		logger.Error("could not parse environment variable as an integer", "name", name)
		return 0, fmt.Errorf("environment variable %s is not an integer: %w", name, err)
	}
	if value <= 0 {
		logger.Error("environment variable must be a positive integer", "name", name)
		return 0, fmt.Errorf("environment variable %s must be a positive integer", name)
	}

	return value, nil
}

func optionalBool(name string, fallback bool, logger *slog.Logger) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}

	value, err := strconv.ParseBool(raw)
	if err != nil {
		logger.Error("could not parse environment variable as a boolean", "name", name)
		return false, fmt.Errorf("environment variable %s is not a boolean: %w", name, err)
	}

	return value, nil
}
