//go:build server

package main

import (
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

// The macOS backend refuses a value this large with ErrSetDataTooBig before it
// sends any command to /usr/bin/security, so this tells the two backends apart
// without ever touching the real keychain.
func TestServerBuildNeverUsesTheOSKeychain(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	big := strings.Repeat("k", 5000)
	if err := keyring.Set("Hostbay", "master-key", big); err != nil {
		t.Fatalf("Set = %v, want the in-memory keychain to accept it", err)
	}
	got, err := keyring.Get("Hostbay", "master-key")
	if err != nil || got != big {
		t.Fatalf("Get = %d bytes, %v; want the value just set", len(got), err)
	}
}
