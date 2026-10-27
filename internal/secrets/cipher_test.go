package secrets

import (
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"hostbay/internal/apperr"
	"hostbay/internal/store"
)

var webAAD = []byte("host-1\x00web.example.com\x0022\x00root")

func newTestVault(t *testing.T, ver Verifier) (*Vault, *store.Store) {
	t.Helper()
	keyring.MockInit()
	st, err := store.Open(filepath.Join(t.TempDir(), "Hostbay"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	v := New(st, ver)
	v.requirePassword = false // tests that need the Linux rule turn it on
	return v, st
}

// obfuscate is SSH Manager's XOR+base64 form (src-tauri/src/hosts.rs).
func obfuscate(plain string) string {
	key := []byte("remote-ssh-manager-obfuscation-key-v1")
	b := []byte(plain)
	for i := range b {
		b[i] ^= key[i%len(key)]
	}
	return "obf:" + base64.StdEncoding.EncodeToString(b)
}

func TestSealOpenRoundTrip(t *testing.T) {
	v, _ := newTestVault(t, nil)
	const secret = "hunter2-correct-horse"
	sealed, err := v.Seal([]byte(secret), webAAD)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sealed, "enc:v1:") {
		t.Fatalf("sealed = %q, want enc:v1: prefix", sealed)
	}
	for _, leak := range []string{secret, obfuscate(secret), strings.TrimPrefix(obfuscate(secret), "obf:"), base64.StdEncoding.EncodeToString([]byte(secret))} {
		if strings.Contains(sealed, leak) {
			t.Fatalf("sealed %q contains %q", sealed, leak)
		}
	}
	got, err := v.Open(sealed, webAAD)
	if err != nil || string(got) != secret {
		t.Fatalf("Open = %q, %v; want %q", got, err, secret)
	}
}

func TestOpenTamperedOrMissingNeedsSecret(t *testing.T) {
	v, _ := newTestVault(t, nil)
	sealed, err := v.Seal([]byte("pw"), webAAD)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(sealed, "enc:v1:"))
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0x01
	flipped := "enc:v1:" + base64.StdEncoding.EncodeToString(raw)

	for name, in := range map[string]string{"flipped byte": flipped, "missing": "", "not sealed": "pw"} {
		got, err := v.Open(in, webAAD)
		if apperr.CodeOf(err) != apperr.SecretNeeded || got != nil {
			t.Errorf("%s: Open = %q, %v; want nil, SecretNeeded", name, got, err)
		}
	}
}

func TestOpenAfterHostnameChangeNeedsSecret(t *testing.T) {
	v, _ := newTestVault(t, nil)
	sealed, err := v.Seal([]byte("pw"), webAAD)
	if err != nil {
		t.Fatal(err)
	}
	moved := []byte("host-1\x00evil.example.com\x0022\x00root")
	got, err := v.Open(sealed, moved)
	if apperr.CodeOf(err) != apperr.SecretNeeded || got != nil {
		t.Fatalf("Open with changed hostname = %q, %v; want nil, SecretNeeded", got, err)
	}
}
