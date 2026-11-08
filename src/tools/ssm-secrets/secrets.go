package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/joho/godotenv"
)

// ParameterPrefix is the SSM path the ExternalSecret in the GitOps repository
// reads. The External Secrets Operator role can read /kubernetes/appsecrets/*.
const ParameterPrefix = "/kubernetes/appsecrets/chad-applications/paprika-mcp-go/"

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// secretSpec maps one env file variable to one SSM parameter.
type secretSpec struct {
	Variable  string
	Parameter string
	Validate  func(value string) error
}

var secretSpecs = []secretSpec{
	{
		Variable:  "MCP_TOKEN_SHA256",
		Parameter: ParameterPrefix + "mcp-token-sha256",
		Validate:  validateTokenHashes,
	},
	{
		Variable:  "PAPRIKA_EMAIL",
		Parameter: ParameterPrefix + "paprika-email",
		Validate:  validateSingleLine,
	},
	{
		Variable:  "PAPRIKA_PASSWORD",
		Parameter: ParameterPrefix + "paprika-password",
		Validate:  validateSingleLine,
	},
}

// Parameter is one SSM SecureString to write.
type Parameter struct {
	Variable string
	Name     string
	Value    string
}

func validateSingleLine(value string) error {
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("must be a single line")
	}
	return nil
}

// validateTokenHashes accepts one or two comma-separated lowercase SHA-256
// hashes, the same shape the server requires.
func validateTokenHashes(value string) error {
	parts := strings.Split(value, ",")
	if len(parts) > 2 {
		return fmt.Errorf("must hold one or two comma-separated hashes, got %d", len(parts))
	}
	for _, part := range parts {
		if !sha256Pattern.MatchString(strings.TrimSpace(part)) {
			return fmt.Errorf("each hash must be 64 lowercase hex characters")
		}
	}
	return nil
}

// LoadParameters reads the env file and returns the parameters to write,
// restricted to only when it is not empty. Error messages name variables, never
// values.
func LoadParameters(envFile string, only []string) ([]Parameter, error) {
	values, err := godotenv.Read(envFile)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w (copy secrets.env.example to secrets.env first)", envFile, err)
	}

	for _, name := range only {
		known := slices.ContainsFunc(secretSpecs, func(s secretSpec) bool {
			return s.Variable == name
		})
		if !known {
			return nil, fmt.Errorf("unknown variable %q in --only", name)
		}
	}

	var params []Parameter
	for _, spec := range secretSpecs {
		if len(only) > 0 && !slices.Contains(only, spec.Variable) {
			continue
		}
		value := strings.TrimSpace(values[spec.Variable])
		if value == "" {
			return nil, fmt.Errorf("%s is empty in %s", spec.Variable, envFile)
		}
		if err := spec.Validate(value); err != nil {
			return nil, fmt.Errorf("%s: %w", spec.Variable, err)
		}
		params = append(params, Parameter{Variable: spec.Variable, Name: spec.Parameter, Value: value})
	}
	return params, nil
}

// putParameterInput renders the JSON for aws ssm put-parameter --cli-input-json,
// so the value never appears on a command line.
func putParameterInput(p Parameter, overwrite bool) ([]byte, error) {
	input := map[string]any{
		"Name":  p.Name,
		"Value": p.Value,
		"Type":  "SecureString",
	}
	if overwrite {
		input["Overwrite"] = true
	}
	return json.Marshal(input)
}

// NewToken returns a random bearer token and the SHA-256 hash the server
// stores. The hash is of the token exactly as a client sends it.
func NewToken() (token, hash string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	token = hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	return token, hex.EncodeToString(sum[:]), nil
}
