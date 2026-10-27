package uniterm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hostbay/internal/importer/importertest"
	"hostbay/internal/store"
)

var fixture = filepath.Join(importertest.Fixtures, "uniterm", "export.utm")

func TestImportMapsKindsGroupsJumpsAndSecrets(t *testing.T) {
	st, v, dir := importertest.Store(t)
	res, err := Parse(fixture, "utm-pass")
	if err != nil {
		t.Fatal(err)
	}
	rep := importertest.Import(t, st, v, res)
	if rep.Hosts != 10 || rep.Folders != 3 || rep.Keys != 1 {
		t.Fatalf("report = %+v", rep)
	}
	jump := importertest.Host(t, st, "jump")
	if importertest.Password(t, v, jump) != "Utm-Pass-1!" || importertest.FolderPath(t, st, jump) != "Lab" {
		t.Errorf("jump = %+v", jump)
	}
	inner := importertest.Host(t, st, "inner")
	if inner.JumpHostID == nil || *inner.JumpHostID != jump.ID || !inner.Forwards() || importertest.FolderPath(t, st, inner) != "Lab/Edge" {
		t.Errorf("inner = %+v", inner)
	}
	d, _ := st.Data()
	name, ok := strings.CutPrefix(inner.IdentityFile, store.KeyRefPrefix)
	if k := d.KeyByName(name); !ok || k == nil {
		t.Fatalf("inner key = %q", inner.IdentityFile)
	} else if raw, err := v.Open(k.Data, store.KeyAAD(k.ID)); err != nil || !strings.Contains(string(raw), "c3ludGhldGljLWhvc3RiYXktaW1wb3J0LWtleQ==") {
		t.Errorf("key opens to %q, %v", raw, err)
	}
	legacy := importertest.Host(t, st, "legacy")
	if legacy.Algorithms == nil || legacy.Algorithms.KeyExchanges[0] != "diffie-hellman-group1-sha1" || !legacy.ForwardsX11() || legacy.ProxyID == nil {
		t.Errorf("legacy = %+v", legacy)
	}
	for _, text := range []string{"extra SSH algorithms: diffie-hellman-group1-sha1, aes128-cbc", "forwards X11", "uniterm proxy"} {
		if !importertest.Noted(rep, "legacy", text) {
			t.Errorf("legacy note %q missing: %v", text, rep.Notes)
		}
	}
	desk := importertest.Host(t, st, "desk")
	if desk.User != `CORP\alice` || *desk.RdpPort != 3390 || *desk.RdpSecurity != "nla" || importertest.Password(t, v, desk) != "Utm-Rdp-4!" {
		t.Errorf("desk = %+v", desk)
	}
	files := importertest.Host(t, st, "files")
	if files.FTPTLS() != "none" || !importertest.Noted(rep, "files", "unencrypted") {
		t.Errorf("files = %+v", files)
	}
	s3 := importertest.Host(t, st, "bucket")
	if *s3.URL != "https://minio.example:9000" || s3.Hostname != "minio.example" || *s3.Bucket != "backups" || !*s3.PathStyle || importertest.Password(t, v, s3) != "Utm-S3-6!" {
		t.Errorf("bucket = %+v", s3)
	}
	if dav := importertest.Host(t, st, "dav"); *dav.URL != "https://dav.example/remote.php/webdav" || importertest.Password(t, v, dav) != "Utm-Dav-7!" {
		t.Errorf("dav = %+v", dav)
	}
	if vm := importertest.Host(t, st, "vm"); vm.KindName() != "vnc" || importertest.Password(t, v, vm) != "Utm-Vnc-11!" {
		t.Errorf("vm = %+v", vm)
	}
	// A database behind an SSH connection is reached through it as its jump
	// host; its skip-verify option is dropped, so it verifies TLS.
	orders := importertest.Host(t, st, "orders")
	if orders.KindName() != "mysql" || orders.JumpHostID == nil || *orders.JumpHostID != jump.ID || orders.DatabaseTLS() != "verify" ||
		store.Deref(orders.Database) != "orders" || importertest.Password(t, v, orders) != "Utm-Db-12!" || importertest.FolderPath(t, st, orders) != "Lab" {
		t.Errorf("orders = %+v", orders)
	}
	if !importertest.Noted(rep, "Lab/orders", "asked about at first connect") || !strings.Contains(importertest.Skipped(rep, "Lab/orders: driver options"), "charset") {
		t.Errorf("orders report = %+v", rep)
	}
	search := importertest.Host(t, st, "search")
	if search.KindName() != "elasticsearch" || store.Deref(search.URL) != "https://198.51.100.64:9200/es" || search.Hostname != "198.51.100.64" ||
		importertest.Password(t, v, search) != "Utm-Es-13!" || !importertest.Noted(rep, "search", "asked about at first connect") {
		t.Errorf("search = %+v, notes %v", search, rep.Notes)
	}
	for name, want := range map[string]string{"tty": "serial", "db": "database", "evil": "not a valid address", "sentinel": "Sentinel"} {
		if got := importertest.Skipped(rep, name); !strings.Contains(got, want) {
			t.Errorf("%s skipped for %q, want %q", name, got, want)
		}
	}
	importertest.NoSecrets(t, dir, "Utm-Pass-1!", "Utm-Pass-3!", "Utm-Rdp-4!", "Utm-Ftp-5!", "Utm-S3-6!", "Utm-Dav-7!", "Utm-Vnc-11!", "Utm-Db-12!", "Utm-Es-13!", "c3ludGhldGljLWhvc3RiYXktaW1wb3J0LWtleQ==")

	res, _ = Parse(fixture, "utm-pass")
	importertest.Reimport(t, st, v, res)
}

func TestExportPasswordRequiredAndChecked(t *testing.T) {
	if _, err := Parse(fixture, ""); err == nil || !strings.Contains(err.Error(), "password-protected") {
		t.Errorf("no password: %v", err)
	}
	if _, err := Parse(fixture, "wrong"); err == nil || !strings.Contains(err.Error(), "wrong") {
		t.Errorf("wrong password: %v", err)
	}
}

// A plain export's field still sealed to the exporting computer is not
// taken as a password.
func TestPlainExportLeavesMachineSealedFields(t *testing.T) {
	p := filepath.Join(t.TempDir(), "plain.utm")
	body := `{"format":"uniterm","encrypted":false,"connections":[{"id":"c","name":"a","type":"ssh","host":"192.0.2.1","port":22,"user":"u","authType":"password","password":"enc:v1:AAAA:BBBB"}]}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := Parse(p, "")
	if err != nil || res.Bundle.Hosts[0].Password != "" || len(res.Notes) != 1 {
		t.Fatalf("res = %+v, %v", res, err)
	}
}

// A file host has no way through a jump host: its jump is dropped and it
// imports with a missing proxy, so it cannot connect directly until its
// settings are looked at.
func TestFileHostImportsWithoutItsJumpHost(t *testing.T) {
	p := filepath.Join(t.TempDir(), "plain.utm")
	body := `{"format":"uniterm","encrypted":false,"connections":[` +
		`{"id":"j","name":"jump","type":"ssh","host":"192.0.2.1","port":22,"user":"u"},` +
		`{"id":"f","name":"files","type":"ftp","host":"192.0.2.2","port":21,"user":"u","ftpEncryption":"explicit","tunnelSSHConnId":"j"}]}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := Parse(p, "")
	if err != nil {
		t.Fatal(err)
	}
	st, v, _ := importertest.Store(t)
	rep := importertest.Import(t, st, v, res)
	files := importertest.Host(t, st, "files")
	if files.Jumped() || !files.Proxied() || !importertest.Noted(rep, "files", "jump host") {
		t.Fatalf("files = %+v, notes %v", files, rep.Notes)
	}
}
