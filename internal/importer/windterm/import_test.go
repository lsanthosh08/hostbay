package windterm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hostbay/internal/importer/importertest"
)

var fixture = filepath.Join(importertest.Fixtures, "windterm", "profiles", "default.v10", "terminal", "user.sessions")

func TestImportDecryptsLoginsAndMapsGroups(t *testing.T) {
	st, v, dir := importertest.Store(t)
	res, err := Parse(fixture, "wt-master")
	if err != nil {
		t.Fatal(err)
	}
	rep := importertest.Import(t, st, v, res)
	if rep.Hosts != 4 || rep.Folders != 2 {
		t.Fatalf("report = %+v", rep)
	}
	app := importertest.Host(t, st, "wt-app")
	if app.User != "deploy" || *app.AuthType != "password" || importertest.Password(t, v, app) != "Wt-Pass-1!" || importertest.FolderPath(t, st, app) != "Cloud/Prod" {
		t.Errorf("wt-app = %+v", app)
	}
	home, _ := os.UserHomeDir()
	if k := importertest.Host(t, st, "wt-key"); k.IdentityFile != filepath.Join(home, ".ssh", "id_ed25519") || k.User != "root" {
		t.Errorf("wt-key = %+v", k)
	}
	if e := importertest.Host(t, st, "wt-escape"); e.IdentityFile != "" {
		t.Errorf("traversing key path kept: %q", e.IdentityFile)
	}
	if h := importertest.Host(t, st, "wt-switch"); h.KindName() != "telnet" {
		t.Errorf("wt-switch = %+v", h)
	}
	if got := importertest.Skipped(rep, "wt-serial"); !strings.Contains(got, "serial") {
		t.Errorf("wt-serial skipped for %q", got)
	}
	if got := importertest.Skipped(rep, "wt-evil"); !strings.Contains(got, "not a valid address") {
		t.Errorf("wt-evil skipped for %q", got)
	}
	importertest.NoSecrets(t, dir, "Wt-Pass-1!")

	res, _ = Parse(fixture, "wt-master")
	importertest.Reimport(t, st, v, res)
}

func TestMasterPasswordRequiredAndChecked(t *testing.T) {
	if _, err := Parse(fixture, ""); err == nil || !strings.Contains(err.Error(), "master password") {
		t.Fatalf("no password: %v", err)
	}
	res, err := Parse(fixture, "wrong")
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range res.Bundle.Hosts {
		if h.Password != "" {
			t.Fatalf("%s got a password from the wrong master password", h.Alias)
		}
	}
	if !strings.Contains(strings.Join(res.Notes, "\n"), "wt-app: its saved login could not be read") {
		t.Fatalf("notes = %v", res.Notes)
	}
}
