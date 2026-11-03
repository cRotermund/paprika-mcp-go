// Package auth verifies inbound bearer tokens for the MCP endpoint.
package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

const (
	// MaxHashes is the number of token hashes accepted at once: the current
	// token and, during a rotation, the next one.
	MaxHashes = 2

	hashHexLen = sha256.Size * 2

	// The SDK middleware rejects a TokenInfo without an expiration. A static
	// token does not expire, so each verified request gets a short lease that
	// only has to outlive that request.
	tokenLease = time.Hour

	subject = "owner"
)

// HashToken returns the lowercase hex SHA-256 of a token. Use it to produce
// the value for MCP_TOKEN_SHA256.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ParseHashes splits a comma-separated list of hex SHA-256 digests, checks the
// format, and returns the digests in lowercase. The error never includes the
// offending value.
func ParseHashes(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("no token hash given")
	}

	parts := strings.Split(raw, ",")
	if len(parts) > MaxHashes {
		return nil, fmt.Errorf("at most %d token hashes are allowed, got %d", MaxHashes, len(parts))
	}

	hashes := make([]string, 0, len(parts))
	for i, part := range parts {
		part = strings.ToLower(strings.TrimSpace(part))
		if len(part) != hashHexLen {
			return nil, fmt.Errorf("token hash %d must be %d hex characters", i+1, hashHexLen)
		}
		if _, err := hex.DecodeString(part); err != nil {
			return nil, fmt.Errorf("token hash %d is not valid hex", i+1)
		}
		hashes = append(hashes, part)
	}

	return hashes, nil
}

// Verifier checks bearer tokens against stored SHA-256 hashes.
type Verifier struct {
	hashes [][sha256.Size]byte
}

// NewVerifier builds a Verifier from hex SHA-256 digests (see ParseHashes).
func NewVerifier(hexHashes []string) (*Verifier, error) {
	if len(hexHashes) == 0 || len(hexHashes) > MaxHashes {
		return nil, fmt.Errorf("need between 1 and %d token hashes", MaxHashes)
	}

	v := &Verifier{hashes: make([][sha256.Size]byte, 0, len(hexHashes))}
	for i, h := range hexHashes {
		decoded, err := hex.DecodeString(strings.ToLower(h))
		if err != nil || len(decoded) != sha256.Size {
			return nil, fmt.Errorf("token hash %d is not a hex SHA-256 digest", i+1)
		}
		var digest [sha256.Size]byte
		copy(digest[:], decoded)
		v.hashes = append(v.hashes, digest)
	}

	return v, nil
}

// Verify implements the SDK's auth.TokenVerifier. Every stored hash is
// compared, in constant time, whether or not an earlier one matched.
func (v *Verifier) Verify(_ context.Context, token string, _ *http.Request) (*sdkauth.TokenInfo, error) {
	presented := sha256.Sum256([]byte(token))

	match := 0
	for i := range v.hashes {
		match |= subtle.ConstantTimeCompare(presented[:], v.hashes[i][:])
	}
	if match != 1 {
		return nil, sdkauth.ErrInvalidToken
	}

	return &sdkauth.TokenInfo{
		UserID:     subject,
		Expiration: time.Now().Add(tokenLease),
	}, nil
}
