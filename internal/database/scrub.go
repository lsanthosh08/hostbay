package database

import (
	"context"
	"errors"
	"net"
	"strings"

	"hostbay/internal/apperr"
)

// Error is an engine's error with its category (KTD23): auth, network,
// timeout, cancelled, syntax or permission, or "" for any other.
type Error struct {
	Category string
	Err      error
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

// Category names what kind of failure err is.
func Category(err error) string {
	var e *Error
	var ne net.Error
	switch {
	case errors.As(err, &e) && e.Category != "":
		return e.Category
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case errors.As(err, &ne):
		return "network"
	}
	return ""
}

// scrubbed stands in for a removed secret.
const scrubbed = "[hidden]"

// Scrub removes every exact match of the secrets from s (KTD26).
func Scrub(s string, secrets ...string) string {
	for _, sec := range secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, scrubbed)
		}
	}
	return s
}

// AppError is what crosses the bridge for err: an *apperr.Error already in
// err keeps its code, any other error becomes DatabaseError with its
// category and the engine's message in "detail". Either way every exact
// match of the secrets is removed first, so a driver that echoes the
// password never shows it to the page.
func AppError(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	var ae *apperr.Error
	if errors.As(err, &ae) {
		out := apperr.New(ae.Code, Scrub(ae.Message, secrets...))
		for k, v := range ae.Details {
			out.With(k, Scrub(v, secrets...))
		}
		return out
	}
	msg := Scrub(err.Error(), secrets...)
	return apperr.New(apperr.DatabaseError, msg).With("category", Category(err)).With("detail", msg)
}
