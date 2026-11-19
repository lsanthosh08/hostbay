package tunnel

import (
	"strings"
	"testing"
)

// envValue returns the value env gives name, and whether it gives one.
func envValue(env []string, name string) (string, bool) {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			return v, true
		}
	}
	return "", false
}

// The clients get the variables that keep their libraries away from
// compiled-in paths, on every system, and nothing that would undo them.
func TestClientEnvConfinesLibraries(t *testing.T) {
	env := clientEnv("/run/openssl.cnf", "/run/gnutls.config", "/libs")
	for name, want := range map[string]string{
		"GNUTLS_SYSTEM_PRIORITY_FILE": "/run/gnutls.config",
		"LC_ALL":                      "C",
		"XML_CATALOG_FILES":           " ",
		"OPENSSL_CONF":                "/run/openssl.cnf",
	} {
		if got, ok := envValue(env, name); !ok || got != want {
			t.Errorf("%s = %q (set %v), want %q", name, got, ok, want)
		}
	}
	for _, name := range []string{"LANGUAGE", "LANG", "LC_MESSAGES", "GNUTLS_SYSTEM_PRIORITY_FAIL_ON_INVALID"} {
		if _, ok := envValue(env, name); ok {
			t.Errorf("clientEnv sets %s", name)
		}
	}
}
