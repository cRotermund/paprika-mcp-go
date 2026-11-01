// Command apiprobe checks docs/paprika-api against the live Paprika API. It only
// sends GET requests after logging in, and its report contains the structure of
// each response but none of the recipe, meal, or grocery text.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"time"

	"github.com/joho/godotenv"
)

const (
	envEmail    = "PAPRIKA_EMAIL"
	envPassword = "PAPRIKA_PASSWORD"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "apiprobe:", err)
		os.Exit(1)
	}
}

// loadDotenv loads a .env file from the working directory when one exists.
// A missing file is not an error, and real environment variables take
// precedence over .env values.
func loadDotenv() error {
	if err := godotenv.Load(); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	return nil
}

func run() error {
	baseURL := flag.String("base-url", defaultBaseURL, "API origin")
	delay := flag.Duration("delay", time.Second, "pause before each read")
	samples := flag.Int("samples", 5, "number of full recipes to read")
	outPath := flag.String("out", "", "report file (default stdout)")
	flag.Parse()

	if err := loadDotenv(); err != nil {
		return fmt.Errorf("load .env: %w", err)
	}
	email, password := os.Getenv(envEmail), os.Getenv(envPassword)
	if email == "" || password == "" {
		return fmt.Errorf("%s and %s must be set", envEmail, envPassword)
	}

	var out io.Writer = os.Stdout
	if *outPath != "" {
		file, err := os.Create(*outPath)
		if err != nil {
			return fmt.Errorf("create report: %w", err)
		}
		defer file.Close()
		out = file
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	return newProber(*baseURL, *delay, out).run(ctx, email, password, *samples)
}
