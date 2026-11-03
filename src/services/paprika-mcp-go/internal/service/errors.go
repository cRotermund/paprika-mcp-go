package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/cache"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/paprikaclient"
)

// Kind classifies a service error for the layer above.
type Kind int

// Error kinds. They match the situations in the spec's "Errors" section.
const (
	// KindLoading means the recipe index is still loading. Retry shortly.
	KindLoading Kind = iota + 1
	// KindNotFound means a uid or reference does not exist. Look it up first.
	KindNotFound
	// KindInvalid means the input is wrong. Fix it and call again.
	KindInvalid
	// KindTemporary means Paprika is unavailable or answered badly. Retry later.
	KindTemporary
	// KindConfig means the server cannot sign in to Paprika. The operator must act.
	KindConfig
	// KindInternal means something unexpected happened inside the server.
	KindInternal
	// KindConflict means the object changed since the caller read it. Re-read
	// and apply the change again.
	KindConflict
	// KindOutcomeUnknown means a write may or may not have been applied.
	// Re-read before trying again.
	KindOutcomeUnknown
)

// Error is the only error type the service returns for expected failures. Its
// Message is written for the end client: it states what happened and what to do
// next, and never includes upstream error text. The wrapped cause, which may
// hold upstream details, is for logs only.
type Error struct {
	Kind    Kind
	Message string
	cause   error
}

func (e *Error) Error() string { return e.Message }

// Unwrap returns the underlying cause for logging. Do not show it to clients.
func (e *Error) Unwrap() error { return e.cause }

func invalid(format string, args ...any) *Error {
	return &Error{Kind: KindInvalid, Message: fmt.Sprintf(format, args...)}
}

func notFound(format string, args ...any) *Error {
	return &Error{Kind: KindNotFound, Message: fmt.Sprintf(format, args...)}
}

// mapError turns an error from the cache or client into a service *Error. A
// canceled request is returned as is.
func (s *Service) mapError(op string, err error) error {
	if err == nil {
		return nil
	}
	var svcErr *Error
	if errors.As(err, &svcErr) {
		return err
	}

	var loading *cache.LoadingError
	switch {
	case errors.As(err, &loading):
		msg := "The recipe index is still loading after startup, so search is not available yet. Retry in a minute"
		if loading.Total > 0 {
			msg = fmt.Sprintf("The recipe index is still loading (%d of %d recipes read), so search is not available yet. Retry in a minute",
				loading.Done, loading.Total)
		}
		return &Error{Kind: KindLoading, Message: msg + "; get_recipe works meanwhile.", cause: err}
	case errors.Is(err, paprikaclient.ErrUnauthorized):
		s.log.Error("paprika login is failing", "op", op)
		return &Error{Kind: KindConfig, cause: err,
			Message: "The server cannot sign in to Paprika. This is a server configuration problem that the operator must fix; retrying will not help."}
	case errors.Is(err, context.Canceled):
		return err
	case errors.Is(err, paprikaclient.ErrTemporary), errors.Is(err, context.DeadlineExceeded):
		s.log.Warn("paprika temporarily unavailable", "op", op, "error", err)
		return &Error{Kind: KindTemporary, cause: err,
			Message: "Paprika is temporarily unavailable. Retry later."}
	}

	var api *paprikaclient.APIError
	if errors.As(err, &api) || errors.Is(err, paprikaclient.ErrUnexpectedResponse) || errors.Is(err, paprikaclient.ErrResponseTooLarge) {
		s.log.Warn("paprika answered unexpectedly", "op", op, "error", err)
		return &Error{Kind: KindTemporary, cause: err,
			Message: "Paprika returned an unusable response. Retry later; if this keeps happening the operator should check the server logs."}
	}

	s.log.Error("unexpected error", "op", op, "error", err)
	return &Error{Kind: KindInternal, cause: err, Message: "Internal error. The operator can find details in the server logs."}
}
