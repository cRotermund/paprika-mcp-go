// Command ssm-secrets writes the production secrets for paprika-mcp-go to AWS
// SSM Parameter Store, where the ExternalSecret in the GitOps repository reads
// them. It never deploys and never prints secret values.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

const usage = `Usage:
  ssm-secrets bootstrap [flags]   create the parameters; fails if one exists
  ssm-secrets rotate    [flags]   overwrite the parameters
  ssm-secrets token               generate a bearer token and its SHA-256 hash

Flags (bootstrap and rotate): -env-file, -region, -profile, -dry-run, -only
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("a command is required")
	}

	switch args[0] {
	case "token":
		token, hash, err := NewToken()
		if err != nil {
			return err
		}
		fmt.Printf("Bearer token (give to the MCP client; it is not stored anywhere): %s\n", token)
		fmt.Printf("MCP_TOKEN_SHA256 (put in secrets.env): %s\n", hash)
		return nil
	case "bootstrap":
		return syncParameters(args[0], args[1:], false)
	case "rotate":
		return syncParameters(args[0], args[1:], true)
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func syncParameters(name string, args []string, overwrite bool) error {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	envFile := flags.String("env-file", "../../../infra/k8s/prod/secrets.env", "gitignored env file with the secret values")
	region := flags.String("region", "us-east-1", "AWS region")
	profile := flags.String("profile", "", "AWS CLI profile")
	dryRun := flags.Bool("dry-run", false, "validate the env file and print parameter names without writing")
	only := flags.String("only", "", "comma-separated variables to sync: MCP_TOKEN_SHA256, PAPRIKA_EMAIL, PAPRIKA_PASSWORD")
	if err := flags.Parse(args); err != nil {
		return err
	}

	var filter []string
	if *only != "" {
		filter = strings.Split(*only, ",")
	}
	params, err := LoadParameters(*envFile, filter)
	if err != nil {
		return err
	}

	fmt.Printf("--- %s from %s ---\n", name, *envFile)
	return PutParameters(params, *region, *profile, overwrite, *dryRun)
}
