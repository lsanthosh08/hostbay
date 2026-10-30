package importer_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"hostbay/internal/backup"
	"hostbay/internal/importer"
	"hostbay/internal/importer/importertest"
	"hostbay/internal/secrets"
	"hostbay/internal/store"
)

func TestReadFileRefusesOversizeFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "huge.xml")
	if err := os.WriteFile(p, make([]byte, importer.MaxFileSize+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := importer.ReadFile(p); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("err = %v, want too large", err)
	}
}

func TestKeyPathRefusesTraversalAndForeignPaths(t *testing.T) {
	home, _ := os.UserHomeDir()
	for in, want := range map[string]string{
		"$(HomeDir)/.ssh/id_ed25519":   filepath.Join(home, ".ssh", "id_ed25519"),
		"~/.ssh/id_rsa":                filepath.Join(home, ".ssh", "id_rsa"),
		"$(HomeDir)/../../etc/shadow":  "",
		"/home/me/.ssh/../../../etc/x": "",
		"keys/id_rsa":                  "",
		"keystore:deploy":              "",
		"":                             "",
	} {
		if got := importer.KeyPath(in); got != want {
			t.Errorf("KeyPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuilderSkipsUnsafeFieldsWithReasons(t *testing.T) {
	b := importer.NewBuilder("test")
	b.Add(importer.Entry{Name: "nl", Host: store.Host{Alias: "nl", Hostname: "192.0.2.1\nProxyCommand x", Port: 22}})
	b.Add(importer.Entry{Name: "dash", Host: store.Host{Alias: "dash", Hostname: "-oProxyCommand=x", Port: 22}})
	b.Add(importer.Entry{Name: "user", Host: store.Host{Alias: "user", Hostname: "192.0.2.2", Port: 22, User: "root\r\nx"}})
	b.Add(importer.Entry{Name: "none", Host: store.Host{Alias: "none", Port: 22}})
	b.Add(importer.Entry{Name: "ok", Host: store.Host{Alias: "ok", Hostname: "192.0.2.3", Port: 22}})
	res := b.Result()
	if len(res.Bundle.Hosts) != 1 || res.Bundle.Hosts[0].Alias != "ok" {
		t.Fatalf("hosts = %+v", res.Bundle.Hosts)
	}
	for _, s := range res.Skipped {
		if s.Reason == "" {
			t.Errorf("%s skipped without a reason", s.File)
		}
	}
	if len(res.Skipped) != 4 {
		t.Fatalf("skipped = %+v", res.Skipped)
	}
}

// A SQLite file and a URL-addressed host have no host name and still
// import; a PostgreSQL host set to TLS "none" is reported as unencrypted.
func TestBuilderAddsPathAndURLHostsAndNotesUnencryptedDatabases(t *testing.T) {
	str := func(v string) *string { return &v }
	b := importer.NewBuilder("test")
	b.Add(importer.Entry{Name: "lite", Host: store.Host{Alias: "lite", Kind: str("sqlite"), File: str("/data/app.db")}})
	b.Add(importer.Entry{Name: "dav", Host: store.Host{Alias: "dav", Kind: str("webdav"), URL: str("https://dav.example/x")}})
	b.Add(importer.Entry{Name: "pg", Host: store.Host{Alias: "pg", Hostname: "db.example", Port: 5432, Kind: str("postgres"), TLS: str("none")}})
	res := b.Result()
	if len(res.Bundle.Hosts) != 3 || len(res.Skipped) != 0 {
		t.Fatalf("hosts = %+v, skipped = %+v", res.Bundle.Hosts, res.Skipped)
	}
	if len(res.Notes) != 1 || res.Notes[0] != "pg: connects unencrypted" {
		t.Fatalf("notes = %q", res.Notes)
	}
}

// A keychain that refuses to seal still imports the hosts, with no
// password and no key-store key in any form (AE7).
func TestImportWithoutKeychainKeepsNoSecrets(t *testing.T) {
	st, _, dir := importertest.Store(t)
	keyring.MockInitWithError(errors.New("user denied access"))
	v := secrets.New(st, nil)
	b := importer.NewBuilder("test")
	h := store.Host{Alias: "k", Hostname: "192.0.2.4", Port: 22}
	h.IdentityFile = b.AddKey("k", []byte("synthetic-key-material"))
	b.Add(importer.Entry{Name: "k", Host: h, Password: "Synthetic-Pass-9!"})
	rep := importertest.Import(t, st, v, b.Result())
	if rep.Hosts != 1 || rep.Keys != 0 || rep.PasswordsNeeded != 1 {
		t.Fatalf("report = %+v", rep)
	}
	got := importertest.Host(t, st, "k")
	if got.Password != "" || got.IdentityFile != "" {
		t.Fatalf("host kept %q / %q", got.Password, got.IdentityFile)
	}
	importertest.NoSecrets(t, dir, "Synthetic-Pass-9!", "synthetic-key-material")
}

func TestJumpLinkFollowsTheExportAndMarksUnknownOnes(t *testing.T) {
	b := importer.NewBuilder("test")
	b.Add(importer.Entry{Name: "jump", Host: store.Host{Alias: "jump", Hostname: "192.0.2.5", Port: 22}, SourceID: "a"})
	b.Add(importer.Entry{Name: "inner", Host: store.Host{Alias: "inner", Hostname: "10.0.0.1", Port: 22}, JumpID: "a"})
	b.Add(importer.Entry{Name: "lost", Host: store.Host{Alias: "lost", Hostname: "10.0.0.2", Port: 22}, JumpID: "zz"})
	res := b.Result()
	jump, inner, lost := res.Bundle.Hosts[0], res.Bundle.Hosts[1], res.Bundle.Hosts[2]
	if inner.JumpHostID == nil || *inner.JumpHostID != jump.ID {
		t.Fatalf("inner jump = %v, want %s", inner.JumpHostID, jump.ID)
	}
	if lost.Jumped() || !lost.Proxied() || !strings.HasPrefix(*lost.ProxyID, "import-missing-") || len(res.Notes) != 1 {
		t.Fatalf("lost = jump %v, proxy %v, notes %v", lost.JumpHostID, lost.ProxyID, res.Notes)
	}
}

// A host of any kind whose jump host is not in the export is never imported
// to connect directly: it gets the missing-proxy marker, so it fails to
// connect until its jump host or proxy is chosen (KTD24).
func TestHostWithAnUnknownJumpHostNeverConnectsDirectly(t *testing.T) {
	for _, k := range []string{"mysql", "kubernetes", "smb", "rdp"} {
		b := importer.NewBuilder("test")
		b.Add(importer.Entry{Name: k, Host: store.Host{Alias: k, Hostname: "10.0.0.3", Port: 3306, Kind: new(k)}, JumpID: "zz"})
		h := b.Result().Bundle.Hosts[0]
		if h.Jumped() || !h.Proxied() || !strings.HasPrefix(*h.ProxyID, "import-missing-") {
			t.Fatalf("%s = jump %v, proxy %v", k, h.JumpHostID, h.ProxyID)
		}
	}
}

// Elasticsearch and rqlite connect as their URL says and have no TLS
// setting, so a plain export is told to change the URL, never to set TLS.
func TestURLDatabaseNotesSayToChangeTheURL(t *testing.T) {
	for _, opts := range []map[string]string{{"esUseSsl": "false"}, nil} {
		for _, k := range []string{"elasticsearch", "rqlite"} {
			b := importer.NewBuilder("test")
			b.AddDatabase(importer.Database{Name: k, Alias: k, Kind: k, Host: "10.0.0.4", Options: opts})
			notes := strings.Join(b.Result().Notes, "\n")
			if strings.Contains(notes, "TLS to none") || strings.Contains(notes, "TLS set to none") || !strings.Contains(notes, "http://") {
				t.Fatalf("%s %v notes:\n%s", k, opts, notes)
			}
		}
	}
}

// The fallback restored when secrets cannot be sealed carries no VPN
// secret, and copying it leaves the parsed bundle as it was.
func TestWithoutSecretsBlanksVPNSecrets(t *testing.T) {
	b := backup.Bundle{VPNs: []store.VPN{{Name: "office", Secret: `{"privateKey":"test-key"}`}}}
	out := importer.WithoutSecrets(b)
	if len(out.VPNs) != 1 || out.VPNs[0].Secret != "" || out.VPNs[0].Name != "office" {
		t.Fatalf("fallback VPNs = %+v", out.VPNs)
	}
	if b.VPNs[0].Secret == "" {
		t.Fatal("WithoutSecrets changed the parsed bundle")
	}
}
