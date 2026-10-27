package claudecode

import (
	"fmt"
	"strings"

	"hostbay/internal/apperr"
)

// Every failure reads from a fixed message. Text the program printed never
// reaches the page or the model (KTD8).
var (
	errMissing    = apperr.New(apperr.ClaudeCodeMissing, "Claude Code was not found. Install it, or choose where it is, in Settings.")
	errTooOld     = apperr.New(apperr.ClaudeCodeTooOld, "This Claude Code is too old for Hostbay. Update Claude Code and try again.")
	errSignedOut  = apperr.New(apperr.ClaudeCodeSignedOut, "Claude Code is not signed in. Run `claude auth login` in your own terminal.")
	errLimit      = apperr.New(apperr.ClaudeCodeLimit, "Your Claude plan's usage limit was reached. Try again after it resets.")
	errAccount    = apperr.New(apperr.ClaudeCodeAccount, "Your Claude account or organization cannot use Claude Code right now.")
	errOtherAPI   = apperr.New(apperr.ClaudeCodeAccount, "Claude Code is set up to use another provider, not a Claude plan. Hostbay needs the plan sign-in.")
	errChanged    = apperr.New(apperr.ClaudeCodeChanged, "Claude Code changed in a way this Hostbay does not support; update Hostbay.")
	errConfigured = apperr.New(apperr.ClaudeCodeConfigured, "Claude Code is configured to run tools; Hostbay cannot use it.")
	errNetwork    = apperr.New(apperr.ClaudeCodeStopped, "Claude Code could not reach Anthropic. Check the connection and try again.")
	errIdle       = apperr.New(apperr.ClaudeCodeStopped, "Claude Code stopped answering and was ended.")
)

func stopped(exit int) *apperr.Error {
	if exit <= 0 {
		return apperr.New(apperr.ClaudeCodeStopped, "Claude Code stopped before it answered.")
	}
	return apperr.New(apperr.ClaudeCodeStopped, fmt.Sprintf("Claude Code stopped (exit %d).", exit))
}

// failure maps what a finished call left behind to the error the person
// sees, or nil when it answered.
func (s *streamState) failure(exit int, stderr string) *apperr.Error {
	switch s.errCode {
	case "":
	case "authentication_failed":
		return errSignedOut
	case "rate_limit":
		return errLimit
	case "billing_error", "account_on_hold", "oauth_org_not_allowed":
		return errAccount
	default:
		return stopped(exit)
	}
	if s.result != nil && s.result.IsError {
		switch s.result.APIErrorStatus {
		case 401:
			return errSignedOut
		case 429:
			return errLimit
		case 402, 403:
			return errAccount
		}
		return stopped(exit)
	}
	if s.result == nil || exit != 0 {
		if strings.Contains(stderr, "unknown option") {
			return errTooOld
		}
		return stopped(exit)
	}
	return nil
}
