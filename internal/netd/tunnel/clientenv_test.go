package tunnel

import (
	"bufio"
	"os"
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

// Every path compiled into the Windows openconnect bundle names a variable
// clientEnv sets; bundle-openconnect.sh fails the build on a path the list
// lacks, so together no compiled-in path is left for openconnect to read.
func TestOpenConnectPathsAreCovered(t *testing.T) {
	f, err := os.Open("../../../build/windows/openconnect-paths.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	env := clientEnv("conf", "prio", "libs")
	n := 0
	s := bufio.NewScanner(f)
	for s.Scan() {
		l := strings.TrimSpace(s.Text())
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		fields := strings.Fields(l)
		if len(fields) != 2 {
			t.Errorf("openconnect-paths.txt: want a path and a variable, got %q", l)
			continue
		}
		n++
		if v, ok := envValue(env, fields[1]); !ok || v == "" {
			t.Errorf("%s is covered by %s, which clientEnv does not set", fields[0], fields[1])
		}
	}
	if n == 0 {
		t.Fatal("openconnect-paths.txt lists no paths")
	}
}
