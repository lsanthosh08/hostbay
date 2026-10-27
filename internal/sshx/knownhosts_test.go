package sshx

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"hostbay/internal/apperr"
)

func seedKey(t *testing.T, seed byte) (ssh.Signer, ssh.PublicKey) {
	t.Helper()
	s, err := ssh.NewSignerFromKey(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	return s, s.PublicKey()
}

// writeKnownHosts writes lines to a fresh known_hosts file and returns its path.
func writeKnownHosts(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func khLine(host string, port int, key ssh.PublicKey) string {
	return knownhosts.Line([]string{knownhosts.Normalize(net.JoinHostPort(host, strconv.Itoa(port)))}, key)
}

// Port of session.rs host_key_trusted_only_when_in_known_hosts: the
// presented key is accepted only when stored, and an unaccepted one carries
// the key type and blob the trust prompt writes into known_hosts.
func TestHostKeyTrustedOnlyWhenInKnownHosts(t *testing.T) {
	_, presented := seedKey(t, 9)
	_, other := seedKey(t, 10)

	cb, _, err := hostKeyCallback(writeKnownHosts(t, khLine("srv", 22, presented)), 0, "srv", 22)
	if err != nil {
		t.Fatal(err)
	}
	if err := cb("", nil, presented); err != nil {
		t.Fatalf("known key refused: %v", err)
	}

	cb, _, err = hostKeyCallback(writeKnownHosts(t, khLine("srv", 22, other)), 0, "srv", 22)
	if err != nil {
		t.Fatal(err)
	}
	var hk *HostKeyError
	if err := cb("", nil, presented); !errors.As(err, &hk) {
		t.Fatalf("other key: err = %v, want HostKeyError", err)
	}
	if hk.KeyType != "ssh-ed25519" || !bytes.Equal(hk.Key, presented.Marshal()) {
		t.Fatalf("captured key = %s %x", hk.KeyType, hk.Key)
	}
}

// Covers AE1.
func TestUnknownKeyYieldsTrustRequestAndAcceptAppendsLine(t *testing.T) {
	_, key := seedKey(t, 11)
	path := filepath.Join(t.TempDir(), ".ssh", "known_hosts") // neither exists yet

	cb, _, err := hostKeyCallback(path, 0, "db.internal", 2222)
	if err != nil {
		t.Fatal(err)
	}
	err = cb("", nil, key)
	var hk *HostKeyError
	if !errors.As(err, &hk) || hk.Changed() {
		t.Fatalf("err = %v, want unknown HostKeyError", err)
	}
	if apperr.CodeOf(err) != apperr.HostKeyUnknown {
		t.Fatalf("code = %q", apperr.CodeOf(err))
	}
	if want := ssh.FingerprintSHA256(key); hk.Fingerprint != want || !strings.HasPrefix(want, "SHA256:") {
		t.Fatalf("fingerprint = %q, want %q", hk.Fingerprint, want)
	}
	if hk.Host != "db.internal" || hk.Port != 2222 || hk.Hop != 0 {
		t.Fatalf("hop = %d %s:%d", hk.Hop, hk.Host, hk.Port)
	}

	appendKnownHost := NewKnownHostAppender(path)
	if err := appendKnownHost(hk.Host, hk.Port, hk.KeyType, hk.Key); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	want := "[db.internal]:2222 ssh-ed25519 " + strings.Fields(string(ssh.MarshalAuthorizedKey(key)))[1] + "\n"
	if string(got) != want {
		t.Fatalf("known_hosts = %q, want %q", got, want)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
			t.Errorf("file mode = %v, want 0600", fi.Mode().Perm())
		}
		if fi, _ := os.Stat(filepath.Dir(path)); fi.Mode().Perm() != 0o700 {
			t.Errorf("dir mode = %v, want 0700", fi.Mode().Perm())
		}
	}
	cb, _, _ = hostKeyCallback(path, 0, "db.internal", 2222)
	if err := cb("", nil, key); err != nil {
		t.Fatalf("accepted key still refused: %v", err)
	}
}

// Covers AE2.
func TestChangedKeyIsRefusedWithBothFingerprints(t *testing.T) {
	_, stored := seedKey(t, 12)
	_, presented := seedKey(t, 13)
	cb, _, err := hostKeyCallback(writeKnownHosts(t, khLine("web", 22, stored)), 1, "web", 22)
	if err != nil {
		t.Fatal(err)
	}
	err = cb("", nil, presented)
	var hk *HostKeyError
	if !errors.As(err, &hk) || !hk.Changed() {
		t.Fatalf("err = %v, want changed HostKeyError", err)
	}
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != apperr.HostKeyChanged {
		t.Fatalf("apperr = %v", ae)
	}
	if ae.Details["fingerprint"] != ssh.FingerprintSHA256(presented) ||
		ae.Details["knownFingerprints"] != ssh.FingerprintSHA256(stored) ||
		ae.Details["hop"] != "1" || ae.Details["host"] != "web" {
		t.Fatalf("details = %v", ae.Details)
	}
	if _, ok := ae.Details["requestId"]; ok {
		t.Fatal("a changed key must not carry an accept path")
	}
}

func TestHashedEntryMatches(t *testing.T) {
	_, key := seedKey(t, 14)
	hashed := knownhosts.Line([]string{knownhosts.HashHostname(knownhosts.Normalize("10.0.0.5:2200"))}, key)
	if !strings.HasPrefix(hashed, "|1|") {
		t.Fatalf("not hashed: %s", hashed)
	}
	cb, _, err := hostKeyCallback(writeKnownHosts(t, hashed), 0, "10.0.0.5", 2200)
	if err != nil {
		t.Fatal(err)
	}
	if err := cb("", nil, key); err != nil {
		t.Fatalf("hashed entry not matched: %v", err)
	}
}

func TestHostKeyAlgorithmsPreferKnownTypes(t *testing.T) {
	_, ed := seedKey(t, 15)
	_, algos, err := hostKeyCallback(writeKnownHosts(t, khLine("h", 22, ed)), 0, "h", 22)
	if err != nil {
		t.Fatal(err)
	}
	if len(algos) == 0 || algos[0] != ssh.KeyAlgoED25519 {
		t.Fatalf("algos = %v, want ssh-ed25519 first", algos)
	}

	rk, _ := rsa.GenerateKey(rand.Reader, 2048)
	rpub, _ := ssh.NewPublicKey(&rk.PublicKey)
	_, algos, _ = hostKeyCallback(writeKnownHosts(t, khLine("h", 22, rpub)), 0, "h", 22)
	if len(algos) < 2 || !strings.HasPrefix(algos[0], "rsa-sha2-") || !strings.HasPrefix(algos[1], "rsa-sha2-") {
		t.Fatalf("algos = %v, want rsa-sha2-* first", algos)
	}
}

// Covers the host-name validation in KTD4.
func TestAppendRejectsUnsafeHostNames(t *testing.T) {
	_, key := seedKey(t, 16)
	path := filepath.Join(t.TempDir(), "known_hosts")
	appendKnownHost := NewKnownHostAppender(path)
	for _, host := range []string{"evil\nhost", "a b", "a\tb", "", "a,b", "x\x00y", "*.corp", "@revoked"} {
		if err := appendKnownHost(host, 22, key.Type(), key.Marshal()); err == nil {
			t.Errorf("host %q accepted", host)
		}
	}
	if err := appendKnownHost("ok", 22, "ssh-rsa", key.Marshal()); err == nil {
		t.Error("mismatched key type accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("known_hosts written after rejected input: %v", err)
	}
}

func TestAppendUsesBareHostOnPort22AndKeepsLinesSeparate(t *testing.T) {
	_, key := seedKey(t, 17)
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, []byte("other ssh-ed25519 AAAA"), 0o600); err != nil { // no trailing newline
		t.Fatal(err)
	}
	if err := NewKnownHostAppender(path)("fe80::1", 22, key.Type(), key.Marshal()); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSuffix(string(got), "\n"), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[1], "fe80::1 ssh-ed25519 ") {
		t.Fatalf("known_hosts = %q", got)
	}
}

func TestRemoveKnownHostNeedsTypedNameAndVerification(t *testing.T) {
	_, key := seedKey(t, 18)
	_, keep := seedKey(t, 19)
	plain := khLine("old.example", 2222, key)
	hashed := knownhosts.Line([]string{knownhosts.HashHostname("[old.example]:2222")}, key)
	kept := khLine("old.example", 22, keep) // same name, other port
	original := strings.Join([]string{"# comment", plain, kept, hashed}, "\n") + "\n"

	dir := t.TempDir()
	target := filepath.Join(dir, "real_known_hosts")
	if err := os.WriteFile(target, []byte(original), 0o640); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "known_hosts")
	if err := os.Symlink(target, path); err != nil {
		t.Skip("symlinks unavailable:", err)
	}

	pass := func(context.Context, string) error { return nil }
	refuse := func(context.Context, string) error { return apperr.New(apperr.VerifyFailed, "no") }
	ctx := context.Background()

	if err := RemoveKnownHost(ctx, path, "old.example", 2222, "old.exampl", pass); err == nil {
		t.Fatal("mistyped host name accepted")
	}
	if err := RemoveKnownHost(ctx, path, "old.example", 2222, "old.example", refuse); apperr.CodeOf(err) != apperr.VerifyFailed {
		t.Fatalf("refused gate: err = %v", err)
	}
	if err := RemoveKnownHost(ctx, path, "old.example", 2222, "old.example", nil); err == nil {
		t.Fatal("nil gate accepted")
	}
	if got, _ := os.ReadFile(target); string(got) != original {
		t.Fatalf("file changed without verification:\n%s", got)
	}

	if err := RemoveKnownHost(ctx, path, "old.example", 2222, "old.example", pass); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(target)
	if want := "# comment\n" + kept + "\n"; string(got) != want {
		t.Fatalf("after removal:\n%q\nwant\n%q", got, want)
	}
	if fi, err := os.Lstat(path); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced by a file")
	}
	if fi, _ := os.Stat(target); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %v, want 0640", fi.Mode().Perm())
	}
}

// Settings lists every named entry with the port RemoveKnownHost takes; a
// hashed entry shows as hashed, and marker lines and comments are skipped.
func TestListKnownHosts(t *testing.T) {
	_, a := seedKey(t, 20)
	_, b := seedKey(t, 21)
	hashed := knownhosts.Line([]string{knownhosts.HashHostname("secret.example")}, b)
	path := writeKnownHosts(t,
		"# comment",
		khLine("web.example", 22, a),
		khLine("db.example", 2222, b),
		knownhosts.Line([]string{"alias.example", "10.0.0.7"}, a),
		hashed,
		"@revoked * "+strings.TrimSpace(string(ssh.MarshalAuthorizedKey(b))),
	)
	got, err := ListKnownHosts(path)
	if err != nil {
		t.Fatal(err)
	}
	fa, fb := ssh.FingerprintSHA256(a), ssh.FingerprintSHA256(b)
	want := []KnownHost{
		{Host: "web.example", Port: 22, KeyType: a.Type(), Fingerprint: fa},
		{Host: "db.example", Port: 2222, KeyType: b.Type(), Fingerprint: fb},
		{Host: "alias.example", Port: 22, KeyType: a.Type(), Fingerprint: fa},
		{Host: "10.0.0.7", Port: 22, KeyType: a.Type(), Fingerprint: fa},
		{Hashed: true, KeyType: b.Type(), Fingerprint: fb},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries %+v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	if got, err := ListKnownHosts(filepath.Join(t.TempDir(), "none")); err != nil || len(got) != 0 {
		t.Fatalf("missing file: %v, %v; want no entries", got, err)
	}
}

func TestListKnownHostsSkipsUnreadableLines(t *testing.T) {
	_, a := seedKey(t, 22)
	got, err := ListKnownHosts(writeKnownHosts(t, "broken line with too many fields here x", khLine("after.example", 22, a)))
	if err != nil || len(got) != 1 || got[0].Host != "after.example" {
		t.Fatalf("got %+v, %v; want the line after the broken one", got, err)
	}
}
