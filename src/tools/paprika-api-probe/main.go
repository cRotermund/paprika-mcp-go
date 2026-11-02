// Command apiprobe checks docs/paprika-api against the live Paprika API. By default
// it only sends GET requests after logging in, and its report contains the structure
// of each response but none of the recipe, meal, or grocery text. The -write-checks
// and -cleanup modes write only MCP-TEST objects that it created itself.
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

// loadDotenv loads a .env file from the working directory when one exists, or the
// named file, which must exist. A missing default file is not an error, and real
// environment variables take precedence over file values.
func loadDotenv(path string) error {
	if path != "" {
		return godotenv.Load(path)
	}
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
	envFile := flag.String("env-file", "", "credentials file (default .env)")
	writeChecks := flag.Bool("write-checks", false, "back up, then write MCP-TEST objects; see README")
	cleanup := flag.Bool("cleanup", false, "remove the MCP-TEST objects named in the state file")
	inspect := flag.Bool("inspect", false, "report the current state of the objects in the state file, with reads only")
	followup := flag.Int("followup", 0, "run follow-up stage 1 or 2 on the test objects, between checks on a device")
	confirmAccount := flag.String("confirm-account", "", "must repeat the configured email for writes")
	allowRealAccount := flag.Bool("allow-real-account", false, "permit writes to an account with more than 25 recipes")
	backupPath := flag.String("backup", "write-checks-backup.local", "backup file written before write checks")
	statePath := flag.String("state", "write-checks-state.local", "file recording the objects write checks created")
	flag.Parse()

	modes := 0
	for _, on := range []bool{*writeChecks, *cleanup, *inspect, *followup != 0} {
		if on {
			modes++
		}
	}
	if modes > 1 {
		return errors.New("-write-checks, -cleanup, -inspect, and -followup cannot be combined")
	}

	if err := loadDotenv(*envFile); err != nil {
		return fmt.Errorf("load credentials file: %w", err)
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

	if *inspect {
		return runInspect(ctx, newProber(*baseURL, *delay, out), email, password, *statePath)
	}

	if *writeChecks || *cleanup || *followup != 0 {
		opts := writeOptions{
			email:          email,
			confirmAccount: *confirmAccount,
			allowRealAcct:  *allowRealAccount,
			backupPath:     *backupPath,
			statePath:      *statePath,
		}
		prober := newWriteProber(*baseURL, *delay, out)
		if *followup != 0 {
			return runFollowup(ctx, prober, password, opts, *followup)
		}
		if *cleanup {
			return runCleanup(ctx, prober, password, opts)
		}
		return runWriteChecks(ctx, prober, password, opts)
	}

	return newProber(*baseURL, *delay, out).run(ctx, email, password, *samples)
}
