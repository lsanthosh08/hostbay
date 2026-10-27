package secrets

import (
	"context"

	"hostbay/internal/apperr"
)

// Result is the outcome of OS user verification. The zero value is not a
// pass, so an unset result refuses.
type Result int

const (
	Passed Result = iota + 1
	Cancelled
	Failed
	NotConfigured // no verification method is set up or available
)

// Verifier asks the OS to verify the user at the keyboard. reason is shown
// in the prompt.
type Verifier interface {
	Verify(ctx context.Context, reason string) Result
}

// PasswordPrompt asks the user for the Hostbay master password. ok is false
// when the user cancels.
type PasswordPrompt func(ctx context.Context, reason string) (password string, ok bool)

// Gate runs verification and returns nil only when it passes. Every other
// result, including a nil verifier, is a VerifyFailed error.
func Gate(ctx context.Context, ver Verifier, reason string) error {
	r := NotConfigured
	if ver != nil {
		r = ver.Verify(ctx, reason)
	}
	switch r {
	case Passed:
		return nil
	case Cancelled:
		return apperr.New(apperr.VerifyFailed, "Verification was cancelled.").With("result", "cancelled")
	case NotConfigured:
		return apperr.New(apperr.VerifyFailed, notConfiguredMessage).With("result", "notConfigured")
	default:
		return apperr.New(apperr.VerifyFailed, "Verification failed.").With("result", "failed")
	}
}
