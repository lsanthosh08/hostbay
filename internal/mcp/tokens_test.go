package mcp

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hostbay/internal/store"
)

// The token file holds hashes only; a revoked token stops verifying.
func TestTokensStoredHashed(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	tokens := NewTokens(st)
	tok, secret, err := tokens.Create("codex")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, tokenPrefix) || len(secret) < 40 {
		t.Fatalf("token %q", secret)
	}
	b, err := os.ReadFile(filepath.Join(st.Dir(), tokensFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), secret) || strings.Contains(string(b), secret[len(tokenPrefix):]) || !strings.Contains(string(b), hashOf(secret)) {
		t.Fatalf("token file = %s", b)
	}
	if fi, _ := os.Stat(filepath.Join(st.Dir(), tokensFile)); fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("token file mode %v", fi.Mode())
	}
	if got, ok := tokens.Verify(secret); !ok || got.ID != tok.ID || got.LastUsed == 0 {
		t.Fatalf("verify = %+v %v", got, ok)
	}
	if _, ok := tokens.Verify(secret + "x"); ok {
		t.Fatal("a wrong token verified")
	}
	if _, err := tokens.Revoke(tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := tokens.Verify(secret); ok {
		t.Fatal("a revoked token verified")
	}
}

// A busy client does not rewrite the file on every request.
func TestTokenLastUsedThrottled(t *testing.T) {
	st, _ := store.Open(t.TempDir())
	defer st.Close()
	tokens := NewTokens(st)
	now := time.UnixMilli(1_000_000)
	tokens.Now = func() time.Time { return now }
	_, secret, _ := tokens.Create("a")
	first, _ := tokens.Verify(secret)
	now = now.Add(time.Second)
	if again, _ := tokens.Verify(secret); again.LastUsed != first.LastUsed {
		t.Fatal("last used rewritten within a minute")
	}
	now = now.Add(time.Minute)
	if later, _ := tokens.Verify(secret); later.LastUsed == first.LastUsed {
		t.Fatal("last used not updated after a minute")
	}
}

// Revoking a token over HTTP: new calls get 401.
func TestRevokedTokenGets401(t *testing.T) {
	r := newRig(t)
	if _, err := r.srv.Tokens.Revoke(r.tokID); err != nil {
		t.Fatal(err)
	}
	if got := post(t, r, func(*http.Request) {}); got != http.StatusUnauthorized {
		t.Fatalf("revoked token: %d", got)
	}
}
