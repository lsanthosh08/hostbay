// Package apperr defines the structured errors every backend call returns,
// so the UI branches on a code instead of parsing message text.
package apperr

import "errors"

// Code names a failure the UI can act on.
type Code string

const (
	HostKeyUnknown Code = "HostKeyUnknown" // a hop presented a key not in known_hosts
	HostKeyChanged Code = "HostKeyChanged" // a hop's key differs from the stored one
	Unreachable    Code = "Unreachable"    // the host did not answer (offers Wake-on-LAN)
	ProxyFailed    Code = "ProxyFailed"    // the host's proxy failed or refused; nothing was dialled directly
	SecretNeeded   Code = "SecretNeeded"   // a saved secret is missing or cannot be decrypted
	VerifyFailed   Code = "VerifyFailed"   // OS user verification was cancelled, failed or unavailable
	HelperMissing  Code = "HelperMissing"  // the bundled RDP helper could not be started
	DatabaseError  Code = "DatabaseError"  // a database refused or failed a request; details.category and details.detail say how
)

// The AI agent's approval gate (KTD32).
const (
	ApprovalDenied  Code = "ApprovalDenied"  // the user denied an agent's request; nothing ran
	ApprovalExpired Code = "ApprovalExpired" // an agent's request went unanswered and was denied; nothing ran
)

// A Claude Code provider's failures (the AI sidebar reads these to tell the
// person what to do; none carries text from the program).
const (
	ClaudeCodeMissing    Code = "ClaudeCodeMissing"    // the program is not installed, or failed the path checks
	ClaudeCodeTooOld     Code = "ClaudeCodeTooOld"     // it does not accept a flag Hostbay needs
	ClaudeCodeSignedOut  Code = "ClaudeCodeSignedOut"  // it is not signed in
	ClaudeCodeLimit      Code = "ClaudeCodeLimit"      // the plan's usage limit was reached
	ClaudeCodeAccount    Code = "ClaudeCodeAccount"    // the account or organization cannot use it
	ClaudeCodeChanged    Code = "ClaudeCodeChanged"    // it behaves differently from the version Hostbay was built for
	ClaudeCodeConfigured Code = "ClaudeCodeConfigured" // admin policy lets it run tools, which Hostbay refuses
	ClaudeCodeStopped    Code = "ClaudeCodeStopped"    // it exited or lost its connection without an answer
)

// Error is a coded error with optional string details for the UI.
type Error struct {
	Code    Code              `json:"code"`
	Message string            `json:"message"`
	Details map[string]string `json:"details,omitempty"`
}

func New(code Code, message string) *Error { return &Error{Code: code, Message: message} }

// With returns e with one more detail set.
func (e *Error) With(key, value string) *Error {
	if e.Details == nil {
		e.Details = map[string]string{}
	}
	e.Details[key] = value
	return e
}

func (e *Error) Error() string { return e.Message }

// CodeOf returns the code of the first *Error in err's chain, or "".
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}
