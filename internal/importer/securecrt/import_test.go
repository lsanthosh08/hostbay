package securecrt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hostbay/internal/importer/importertest"
)

var fixture = filepath.Join(importertest.Fixtures, "securecrt", "sessions.xml")

func TestImportMapsSessionsFoldersAndPasswords(t *testing.T) {
	st, v, dir := importertest.Store(t)
	res, err := Parse(fixture, "crt-pass")
	if err != nil {
		t.Fatal(err)
	}
	rep := importertest.Import(t, st, v, res)
	if rep.Hosts != 4 || rep.Folders != 1 {
		t.Fatalf("report = %+v", rep)
	}
	app := importertest.Host(t, st, "app-1")
	if app.Port != 2200 || app.User != "deploy" || !app.Forwards() || app.ProxyID != nil || importertest.Password(t, v, app) != "Crt-Pass-1!" {
		t.Errorf("app-1 = %+v", app)
	}
	if importertest.FolderPath(t, st, app) != "Production" {
		t.Errorf("app-1 folder = %q", importertest.FolderPath(t, st, app))
	}
	if !importertest.Noted(rep, "Production/app-1", "forwards this computer's SSH agent") {
		t.Errorf("agent forwarding not noted: %v", rep.Notes)
	}
	app2 := importertest.Host(t, st, "app-2")
	if app2.IdentityFile != "" || app2.ProxyID == nil || !app2.ForwardsX11() || app2.Password != "" {
		t.Errorf("app-2 = %+v", app2)
	}
	for _, text := range []string{"is not a path on this computer", "firewall \"corp-socks\"", "forwards X11", "\"03\" format"} {
		if !importertest.Noted(rep, "Production/app-2", text) {
			t.Errorf("app-2 note %q missing: %v", text, rep.Notes)
		}
	}
	if pw := importertest.Password(t, v, importertest.Host(t, st, "legacy")); pw != "Crt-Old-2!" {
		t.Errorf("legacy password = %q", pw)
	}
	if h := importertest.Host(t, st, "switch"); h.KindName() != "telnet" || h.Port != 23 {
		t.Errorf("switch = %+v", h)
	}
	for name, want := range map[string]string{"console": "serial", "rlogin-box": "no Hostbay equivalent"} {
		if got := importertest.Skipped(rep, name); !strings.Contains(got, want) {
			t.Errorf("%s skipped for %q, want %q", name, got, want)
		}
	}
	if importertest.Skipped(rep, "Default") != "" {
		t.Error("the Default template was reported")
	}
	importertest.NoSecrets(t, dir, "Crt-Pass-1!", "Crt-Old-2!")

	res, _ = Parse(fixture, "crt-pass")
	importertest.Reimport(t, st, v, res)
}

func TestWrongPassphraseLeavesPasswordBehind(t *testing.T) {
	res, err := Parse(fixture, "wrong")
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range res.Bundle.Hosts {
		if h.Alias == "app-1" && h.Password != "" {
			t.Fatal("password decrypted under the wrong passphrase")
		}
	}
	if !strings.Contains(strings.Join(res.Notes, "\n"), "check the configuration passphrase") {
		t.Fatalf("notes = %v", res.Notes)
	}
}

func TestHostileFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.xml")
	body := `<!DOCTYPE k [<!ENTITY x SYSTEM "file:///etc/passwd">]><VanDyke><key name="Sessions"><key name="a"><string name="Protocol Name">SSH2</string><string name="Hostname">&x;</string></key></key></VanDyke>`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(p, ""); err == nil {
		t.Fatal("imported a file with an external entity")
	}
}
