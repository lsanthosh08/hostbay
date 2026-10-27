package secrets

import "hostbay/internal/store"

const notConfiguredMessage = "No Hostbay master password is set, so saved secrets cannot be revealed or exported."

// NewVerifier returns the Linux verifier, which asks for the Hostbay master
// password through ask. hwnd is unused on Linux.
func NewVerifier(st *store.Store, hwnd uintptr, ask PasswordPrompt) Verifier {
	return NewPasswordVerifier(st, ask)
}
