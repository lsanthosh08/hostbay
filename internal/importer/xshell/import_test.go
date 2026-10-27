package xshell

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hostbay/internal/importer/importertest"
)

var fixture = filepath.Join(importertest.Fixtures, "xshell", "sessions.xts")

func TestImportMapsArchiveFoldersAndPasswords(t *testing.T) {
	st, v, dir := importertest.Store(t)
	res, err := Parse(fixture, "xs-master")
	if err != nil {
		t.Fatal(err)
	}
	rep := importertest.Import(t, st, v, res)
	if rep.Hosts != 4 || rep.Folders != 2 {
		t.Fatalf("report = %+v", rep)
	}
	web := importertest.Host(t, st, "web")
	if web.User != "deploy" || !web.ForwardsX11() || importertest.Password(t, v, web) != "Xs-Pass-1!" || importertest.FolderPath(t, st, web) != "Prod" {
		t.Errorf("web = %+v", web)
	}
	api := importertest.Host(t, st, "api")
	if api.Port != 2222 || api.ProxyID == nil {
		t.Errorf("api = %+v", api)
	}
	if !importertest.Noted(rep, "Prod/api", "user key \"id_rsa_2048\"") || !importertest.Noted(rep, "Prod/api", "proxy \"corp-http\"") {
		t.Errorf("api notes missing: %v", rep.Notes)
	}
	if pw := importertest.Password(t, v, importertest.Host(t, st, "old")); pw != "Xs-Old-2!" {
		t.Errorf("Xshell 5.0 password = %q", pw)
	}
	// "Xshell/Sessions/../../escape/switch.xsh" names no folder above the export.
	sw := importertest.Host(t, st, "switch")
	if sw.KindName() != "telnet" || importertest.FolderPath(t, st, sw) != "escape" {
		t.Errorf("switch = %+v in %q", sw, importertest.FolderPath(t, st, sw))
	}
	if got := importertest.Skipped(rep, "tty"); !strings.Contains(got, "serial") {
		t.Errorf("tty skipped for %q", got)
	}
	importertest.NoSecrets(t, dir, "Xs-Pass-1!", "Xs-Old-2!")

	res, _ = Parse(fixture, "xs-master")
	importertest.Reimport(t, st, v, res)
}

func TestNewerPasswordsNeedTheMasterPassword(t *testing.T) {
	for _, master := range []string{"", "wrong"} {
		res, err := Parse(fixture, master)
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range res.Bundle.Hosts {
			if h.Alias == "web" && h.Password != "" {
				t.Fatalf("master %q opened the password", master)
			}
		}
		if !strings.Contains(strings.Join(res.Notes, "\n"), "Prod/web: its saved password") {
			t.Fatalf("master %q: notes %v", master, res.Notes)
		}
	}
}

// An entry whose size is over the cap refuses the whole archive before any
// entry is unpacked.
func TestOversizeEntryRefusesArchive(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("Xshell/Sessions/a.xsh")
	w.Write([]byte("[CONNECTION]\r\nProtocol=SSH\r\nHost=192.0.2.1\r\n"))
	w, _ = zw.Create("Xshell/Sessions/bomb.xsh")
	w.Write(bytes.Repeat([]byte("A"), maxEntrySize+1))
	zw.Close()
	p := filepath.Join(t.TempDir(), "bomb.xts")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(p, ""); err == nil || !strings.Contains(err.Error(), "bomb.xsh") {
		t.Fatalf("err = %v, want the oversize entry named", err)
	}
}
