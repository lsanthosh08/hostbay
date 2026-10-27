//go:build server

package main

import "github.com/zalando/go-keyring"

// Server mode is the browser preview used for UI checks, run with a
// throwaway HOME. The real keychain backend there would either reach the
// user's own Hostbay master key or, with no login keychain under that HOME,
// make macOS show "Keychain Not Found" with a Reset To Defaults button. An
// in-memory keychain keeps every server-mode run away from the OS keychain.
func init() {
	keyring.MockInit()
}
