package mobaxterm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hostbay/internal/importer/importertest"
)

var (
	sessions = filepath.Join(importertest.Fixtures, "mobaxterm", "sessions.mxtsessions")
	ini      = filepath.Join(importertest.Fixtures, "mobaxterm", "MobaXterm.ini")
)

func TestImportMapsSessionsAndFolders(t *testing.T) {
	st, v, _ := importertest.Store(t)
	res, err := Parse(sessions, "")
	if err != nil {
		t.Fatal(err)
	}
	rep := importertest.Import(t, st, v, res)
	if rep.Hosts != 7 || rep.Folders != 2 {
		t.Fatalf("report = %+v", rep)
	}
	home, _ := os.UserHomeDir()
	b := importertest.Host(t, st, "bastion")
	if b.KindName() != "ssh" || b.User != "admin" || *b.AuthType != "key" || b.IdentityFile != filepath.Join(home, ".ssh", "id_ed25519") || b.ForwardsX11() {
		t.Errorf("bastion = %+v", b)
	}
	if !importertest.Noted(rep, "bastion", "X11 forwarding was not imported") {
		t.Errorf("X11 default not noted: %v", rep.Notes)
	}
	web := importertest.Host(t, st, "web-1")
	if web.Port != 2222 || importertest.FolderPath(t, st, web) != "Production/Web" {
		t.Errorf("web-1 = %+v in %q", web, importertest.FolderPath(t, st, web))
	}
	if !importertest.Noted(rep, "Production/Web/web-1", "gateway jump.example") {
		t.Errorf("gateway not noted: %v", rep.Notes)
	}
	if h := importertest.Host(t, st, "legacy-switch"); h.KindName() != "telnet" || h.Port != 23 {
		t.Errorf("telnet = %+v", h)
	}
	if h := importertest.Host(t, st, "desktop"); h.KindName() != "rdp" || *h.RdpPort != 3389 || h.User != "alice" {
		t.Errorf("rdp = %+v", h)
	}
	if h := importertest.Host(t, st, "console"); h.KindName() != "vnc" || h.Port != 5901 {
		t.Errorf("vnc = %+v", h)
	}
	if h := importertest.Host(t, st, "files"); h.KindName() != "ftp" || h.FTPTLS() != "explicit" {
		t.Errorf("ftp = %+v", h)
	}
	if h := importertest.Host(t, st, "traverse"); h.IdentityFile != "" {
		t.Errorf("traversing key path kept: %q", h.IdentityFile)
	}
	for name, want := range map[string]string{
		"Production/Web/uplink": "serial",
		"Production/Web/local":  "Shell",
		"evil":                  "not a valid address",
	} {
		if got := importertest.Skipped(rep, name); !strings.Contains(got, want) {
			t.Errorf("%s skipped for %q, want %q", name, got, want)
		}
	}
	res, _ = Parse(sessions, "")
	importertest.Reimport(t, st, v, res)
}

func TestIniPasswordsNeedTheMasterPassword(t *testing.T) {
	st, v, dir := importertest.Store(t)
	res, err := Parse(ini, "moba-master")
	if err != nil {
		t.Fatal(err)
	}
	importertest.Import(t, st, v, res)
	if pw := importertest.Password(t, v, importertest.Host(t, st, "bastion")); pw != "Moba-Pass-1!" {
		t.Errorf("bastion password = %q", pw)
	}
	web := importertest.Host(t, st, "web-1")
	if pw := importertest.Password(t, v, web); pw != "Moba-Pass-2!" || *web.AuthType != "password" {
		t.Errorf("web-1 password = %q, auth %v", pw, *web.AuthType)
	}
	importertest.NoSecrets(t, dir, "Moba-Pass-1!", "Moba-Pass-2!")

	res, err = Parse(ini, "")
	if err != nil || res.Bundle.Hosts[0].Password != "" || !strings.Contains(strings.Join(res.Notes, "\n"), "master password") {
		t.Fatalf("no master password: %v, notes %v", err, res.Notes)
	}
	res, err = Parse(ini, "wrong")
	if err != nil || !strings.Contains(strings.Join(res.Notes, "\n"), "could not be decrypted") {
		t.Fatalf("wrong master password: %v, notes %v", err, res.Notes)
	}
	for _, h := range res.Bundle.Hosts {
		if h.Password != "" {
			t.Errorf("%s got a password from a wrong master password", h.Alias)
		}
	}
}

func TestNotAMobaXtermFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.mxtsessions")
	if err := os.WriteFile(p, []byte("[Other]\nfoo=bar\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(p, ""); err == nil {
		t.Fatal("imported a file with no sessions")
	}
}
