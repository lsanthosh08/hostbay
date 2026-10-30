package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"hostbay/internal/apperr"
	"hostbay/internal/importer/sshmanager"
	"hostbay/internal/secrets"
	"hostbay/internal/store"
	"hostbay/internal/vpn/detect"
)

type fakeVerifier secrets.Result

func (f fakeVerifier) Verify(context.Context, string) secrets.Result { return secrets.Result(f) }

func newHostService(t *testing.T, ver secrets.Verifier) (*HostService, *HostCredentials, string) {
	t.Helper()
	keyring.MockInit()
	dir := filepath.Join(t.TempDir(), "Hostbay")
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	v := secrets.New(st, ver)
	if err := v.SetMasterPassword("test master password"); err != nil { // Linux needs one before sealing
		t.Fatal(err)
	}
	creds := NewHostCredentials(st, v)
	vpns := NewVPNService(st, v)
	vpns.detect = func(context.Context) (detect.Snapshot, error) { return detect.Snapshot{}, nil } // never this computer's VPNs
	LinkVPNs(creds, vpns)
	return NewHostService(st, v, ver), creds, dir
}

func TestSaveHostSealsPasswordAndHidesIt(t *testing.T) {
	s, creds, dir := newHostService(t, fakeVerifier(secrets.Passed))
	pw, auth := "Svc-Pass-7!", "password"
	h, err := s.SaveHost(Host{Alias: "web", Hostname: "10.0.0.9", Port: 22, User: "root", AuthType: &auth, Password: &pw})
	if err != nil {
		t.Fatal(err)
	}
	if !h.HasPassword || h.Password == nil || *h.Password != "" {
		t.Fatalf("SaveHost returned password %v, hasPassword %v", h.Password, h.HasPassword)
	}
	files, _ := os.ReadDir(dir)
	for _, f := range files {
		b, _ := os.ReadFile(filepath.Join(dir, f.Name()))
		if strings.Contains(string(b), pw) {
			t.Fatalf("%s holds the password in plaintext", f.Name())
		}
	}

	// Saving the form again with an empty password keeps the saved one.
	h.Alias, h.Password = "web2", new(string)
	if _, err := s.SaveHost(h); err != nil {
		t.Fatal(err)
	}
	cr, err := creds.Lookup(h.ID)
	if err != nil || cr.Password != pw || cr.Host.Alias != "web2" {
		t.Fatalf("Lookup = %+v, %v", cr, err)
	}
	got, err := s.RevealPassword(context.Background(), h.ID)
	if err != nil || got != pw {
		t.Fatalf("RevealPassword = %q, %v", got, err)
	}

	// Pointing the host elsewhere unbinds the password (KTD6).
	h.Hostname = "evil.example.com"
	if _, err := s.SaveHost(h); err != nil {
		t.Fatal(err)
	}
	if _, err := creds.Lookup(h.ID); apperr.CodeOf(err) != apperr.SecretNeeded {
		t.Fatalf("Lookup after rebinding = %v, want SecretNeeded", err)
	}
}

func TestExportAndRevealNeedVerification(t *testing.T) {
	s, _, _ := newHostService(t, fakeVerifier(secrets.Cancelled))
	pw := "x"
	h, err := s.SaveHost(Host{Alias: "web", Hostname: "h", Port: 22, User: "u", Password: &pw})
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "b.json")
	for name, err := range map[string]error{
		"plain export":     s.ExportStore(context.Background(), out, ""),
		"encrypted export": s.ExportHost(context.Background(), h.ID, out, "pass"),
	} {
		if apperr.CodeOf(err) != apperr.VerifyFailed {
			t.Errorf("%s = %v, want VerifyFailed", name, err)
		}
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("a refused export wrote a file")
	}
	if got, err := s.RevealPassword(context.Background(), h.ID); apperr.CodeOf(err) != apperr.VerifyFailed || got != "" {
		t.Fatalf("RevealPassword = %q, %v", got, err)
	}
}

func TestKeyStoreKeyIsSealedAndShownByName(t *testing.T) {
	s, creds, dir := newHostService(t, fakeVerifier(secrets.Passed))
	src := filepath.Join(t.TempDir(), "id_test")
	key := "-----BEGIN OPENSSH PRIVATE KEY-----\nsvc-test-key\n-----END OPENSSH PRIVATE KEY-----\n"
	if err := os.WriteFile(src, []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
	name, err := s.SaveIdentityKey(src)
	if err != nil || name != "id_test" {
		t.Fatalf("SaveIdentityKey = %q, %v", name, err)
	}
	if again, _ := s.SaveIdentityKey(src); again != "id_test-1" {
		t.Fatalf("second copy named %q, want id_test-1", again)
	}
	h, err := s.SaveHost(Host{Alias: "k", Hostname: "h", Port: 22, User: "u", IdentityFile: name})
	if err != nil || h.IdentityFile != name {
		t.Fatalf("SaveHost identityFile = %q, %v", h.IdentityFile, err)
	}
	cr, err := creds.Lookup(h.ID)
	if err != nil || string(cr.Key) != key || cr.Host.IdentityFile != store.KeyRefPrefix+name {
		t.Fatalf("Lookup = %+v, %v", cr, err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "keys.json"))
	if strings.Contains(string(b), "svc-test-key") {
		t.Fatal("keys.json holds the key in plaintext")
	}
}

func TestPasswordHostWithoutPasswordNeedsSecret(t *testing.T) {
	s, creds, _ := newHostService(t, fakeVerifier(secrets.Passed))
	auth := "password"
	h, err := s.SaveHost(Host{Alias: "p", Hostname: "h", Port: 22, User: "u", AuthType: &auth})
	if err != nil {
		t.Fatal(err)
	}
	_, err = creds.Lookup(h.ID)
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != apperr.SecretNeeded || ae.Details["hostId"] != h.ID {
		t.Fatalf("Lookup = %#v, want SecretNeeded naming host %s", err, h.ID)
	}
}

// Clear recent empties every host's lastConnected without counting as an
// edit to sync.
func TestClearRecentForgetsLastConnected(t *testing.T) {
	s, creds, _ := newHostService(t, fakeVerifier(secrets.Passed))
	h, err := s.SaveHost(Host{Alias: "web", Hostname: "10.0.0.1", Port: 22, User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	if err := creds.st.Edit(func(d *store.Data) error { return stampConnected(d, h.ID) }); err != nil {
		t.Fatal(err)
	}
	d, _ := creds.st.Data()
	rev := d.Host(h.ID).Revision
	if err := s.ClearRecent(); err != nil {
		t.Fatal(err)
	}
	d, _ = creds.st.Data()
	if got := d.Host(h.ID); got.LastConnected != "" || got.Revision != rev {
		t.Fatalf("lastConnected %q, revision %d; want it cleared at revision %d", got.LastConnected, got.Revision, rev)
	}
}

// sshManagerSource points the user config dir at a temp dir and copies SSH
// Manager's fixture into it, returning where ImportFromSshManager reads.
func sshManagerSource(t *testing.T) string {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("HOME", cfg)            // macOS: $HOME/Library/Application Support
	t.Setenv("XDG_CONFIG_HOME", cfg) // Linux
	t.Setenv("AppData", cfg)         // Windows
	src, err := sshmanager.DefaultSourceDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(src, os.DirFS("../../testdata/sshmanager/remote-ssh-manager")); err != nil {
		t.Fatal(err)
	}
	return src
}

// Running the import again from settings picks up a file the first launch
// skipped and adds nothing it already brought across (R31).
func TestImportFromSshManagerRetriesSkippedFile(t *testing.T) {
	s, creds, _ := newHostService(t, fakeVerifier(secrets.Passed))
	src := sshManagerSource(t)
	p := filepath.Join(src, "folders.json")
	good, _ := os.ReadFile(p)
	if err := os.WriteFile(p, good[:len(good)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	if rep, err := sshmanager.ImportOnce(creds.st, creds.vault, src); err != nil || len(rep.Skipped) != 1 {
		t.Fatalf("first launch = %+v, %v", rep, err)
	}
	if err := os.WriteFile(p, good, 0o600); err != nil {
		t.Fatal(err)
	}

	rep, err := s.ImportFromSshManager()
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Ran || rep.Hosts != 0 || rep.Folders != 2 || len(rep.Skipped) != 0 {
		t.Fatalf("rerun report = %+v", rep)
	}
	rep, err = s.ImportFromSshManager()
	if err != nil || rep.Hosts+rep.Folders+rep.Scripts+rep.Keys != 0 {
		t.Fatalf("third run = %+v, %v", rep, err)
	}
	d, _ := creds.st.Data()
	if len(d.Hosts.Items) != 8 || len(d.Folders.Items) != 2 {
		t.Fatalf("store has %d hosts, %d folders", len(d.Hosts.Items), len(d.Folders.Items))
	}
}

// With no SSH Manager folder, running the import says so instead of
// reporting an empty success.
func TestImportFromSshManagerNeedsSourceFolder(t *testing.T) {
	s, _, _ := newHostService(t, fakeVerifier(secrets.Passed))
	src := sshManagerSource(t)
	if err := os.RemoveAll(src); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportFromSshManager(); err == nil || !strings.Contains(err.Error(), src) {
		t.Fatalf("err = %v, want one naming %s", err, src)
	}
}

// After a restart in master-password mode the vault is locked. Cloning must
// fail so the UI unlocks and retries, not commit a copy without its password.
func TestCloneHostWithALockedVaultFailsAndAddsNothing(t *testing.T) {
	s, _, _ := newHostService(t, fakeVerifier(secrets.Passed))
	pw, auth := "Svc-Pass-7!", "password"
	h, err := s.SaveHost(Host{Alias: "web", Hostname: "10.0.0.9", Port: 22, User: "root", AuthType: &auth, Password: &pw})
	if err != nil {
		t.Fatal(err)
	}
	// What SetMasterPassword records on Linux when no Secret Service answers.
	mp, err := store.Load[map[string]any](s.st, "masterpassword.json")
	if err != nil {
		t.Fatal(err)
	}
	mp["unlocksKey"] = true
	if err := store.Save(s.st, "masterpassword.json", mp); err != nil {
		t.Fatal(err)
	}
	locked := NewHostService(s.st, secrets.New(s.st, nil), nil)
	if _, err := locked.CloneHost(h.ID); apperr.CodeOf(err) != apperr.SecretNeeded {
		t.Fatalf("CloneHost with a locked vault = %v; want SecretNeeded", err)
	}
	if hosts, _ := locked.ListHosts(); len(hosts) != 1 {
		t.Fatalf("%d hosts after a failed clone; want 1", len(hosts))
	}
}

// A VNC host signs in with a password, so a missing one is asked for
// rather than tried empty.
func TestVNCHostNeedsItsPassword(t *testing.T) {
	s, creds, _ := newHostService(t, fakeVerifier(secrets.Passed))
	h, err := s.SaveHost(Host{Alias: "mac", Hostname: "mac.lan", Kind: new("vnc")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := creds.Lookup(h.ID); apperr.CodeOf(err) != apperr.SecretNeeded {
		t.Fatalf("Lookup = %v, want SecretNeeded", err)
	}
	if _, err := s.SaveHost(Host{Alias: "x", Hostname: "x", Kind: new("vnc"), TLS: new("tls-port")}); err == nil {
		t.Fatal("an unknown VNC security mode was saved")
	}
}

// ImportFrom routes each tool to its parser, seals what it imports, and
// adds nothing on a second run (R54).
func TestImportFromToolAddsOnceAndSealsPasswords(t *testing.T) {
	s, creds, dir := newHostService(t, fakeVerifier(secrets.Passed))
	if _, err := s.ImportFrom("nosuchtool", "x", ""); err == nil {
		t.Fatal("unknown tool imported")
	}
	src := "../../testdata/importers/mremoteng/confCons.xml"
	rep, err := s.ImportFrom("mremoteng", src, "mr-master")
	if err != nil || rep.Hosts != 5 || len(rep.Skipped) != 3 {
		t.Fatalf("report = %+v, %v", rep, err)
	}
	if rep, err = s.ImportFrom("mremoteng", src, "mr-master"); err != nil || rep.Hosts+rep.Folders != 0 {
		t.Fatalf("second import = %+v, %v", rep, err)
	}
	d, _ := creds.st.Data()
	for _, h := range d.Hosts.Items {
		if h.Alias == "win-1" {
			if c, err := creds.Lookup(h.ID); err != nil || c.Password != "Win-Pass-2!" {
				t.Fatalf("win-1 credentials = %q, %v", c.Password, err)
			}
		}
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "hosts.json"))
	if strings.Contains(string(raw), "Win-Pass-2!") {
		t.Fatal("hosts.json holds an imported password in clear")
	}
}
