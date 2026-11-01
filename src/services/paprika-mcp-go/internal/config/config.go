package config

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

const (
	EnvPort     = "PORT"
	EnvLogLevel = "LOG_LEVEL"
)

type Config struct {
	Port     int
	LogLevel slog.Level
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

func Load(logger *slog.Logger) (*Config, error) {
	port, err := requiredInt(EnvPort, logger)
	if err != nil {
		return nil, err
	}

	return &Config{
		Port:     port,
		LogLevel: LogLevelFromEnv(),
	}, nil
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
		logger.Error("could not parse environment variable as an integer", "name", name, "value", raw)
		return 0, fmt.Errorf("environment variable %s is not an integer: %w", name, err)
	}

	return value, nil
}
