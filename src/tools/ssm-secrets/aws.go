package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// runner executes the aws CLI. It is a variable so tests can replace it.
var runner = func(args []string) (string, error) {
	cmd := exec.Command("aws", args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

// PutParameters writes each parameter with the aws CLI. Without overwrite, an
// existing parameter is an error. With dryRun it only prints names.
func PutParameters(params []Parameter, region, profile string, overwrite, dryRun bool) error {
	for _, p := range params {
		if dryRun {
			fmt.Printf("would write %s (from %s)\n", p.Name, p.Variable)
			continue
		}
		if err := putOne(p, region, profile, overwrite); err != nil {
			return err
		}
		fmt.Printf("wrote %s\n", p.Name)
	}
	return nil
}

func putOne(p Parameter, region, profile string, overwrite bool) error {
	input, err := putParameterInput(p, overwrite)
	if err != nil {
		return err
	}

	file, err := os.CreateTemp("", "ssm-put-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(input); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}

	args := []string{
		"ssm",
		"put-parameter",
		"--cli-input-json",
		"file://" + file.Name(),
		"--region",
		region,
	}
	if profile != "" {
		args = append(args, "--profile", profile)
	}

	out, err := runner(args)
	if err != nil {
		// The value is only in the temp file, but redact it anyway in case the
		// CLI echoes its input.
		return fmt.Errorf("put %s failed: %w\n%s", p.Name, err, strings.ReplaceAll(out, p.Value, "[REDACTED]"))
	}
	return nil
}
