package paprikaclient

import (
	"errors"
	"fmt"
)

// Sentinel errors for the service layer to map. Detailed errors wrap them.
var (
	// ErrUnauthorized means login is failing or the token is still refused after
	// a fresh login. It is a configuration problem.
	ErrUnauthorized = errors.New("paprika: not authorized")
	// ErrTemporary means Paprika is unavailable, rate blocked, timing out, or
	// being given a rest. Retry later.
	ErrTemporary = errors.New("paprika: temporarily unavailable")
	// ErrNotFound means Paprika answered HTTP 404.
	ErrNotFound = errors.New("paprika: not found")
	// ErrOutcomeUnknown means a write may or may not have been applied. Re-read
	// before trying again.
	ErrOutcomeUnknown = errors.New("paprika: write outcome unknown")
	// ErrResponseTooLarge means a response exceeded the configured size cap.
	ErrResponseTooLarge = errors.New("paprika: response too large")
	// ErrUnexpectedResponse means a response was not shaped as documented.
	ErrUnexpectedResponse = errors.New("paprika: unexpected response")
	// ErrInvalidArgument means the caller passed something the client refuses to send.
	ErrInvalidArgument = errors.New("paprika: invalid argument")
)

// APIError is an error reported by Paprika, through the "error" envelope or an
// unexpected HTTP status.
type APIError struct {
	// Status is the HTTP status of the response.
	Status int
	// Code is the "code" from the error envelope, or zero.
	Code int
	// Message is the upstream message, or empty.
	Message string
}

// Error implements error. The upstream message is included, so do not show it
// to end users.
func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("paprika: HTTP %d (code %d)", e.Status, e.Code)
	}
	return fmt.Sprintf("paprika: HTTP %d (code %d): %s", e.Status, e.Code, e.Message)
}

// Is makes a 404 match [ErrNotFound].
func (e *APIError) Is(target error) bool {
	return target == ErrNotFound && e.Status == 404
}
