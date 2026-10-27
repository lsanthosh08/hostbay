package store

import (
	"path/filepath"
	"strings"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "Hostbay"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// Ported from parses_config_blocks_and_skips_wildcards in src-tauri/src/hosts.rs.
func TestParseSSHConfigSkipsWildcardBlocks(t *testing.T) {
	cfg := "Host *\n    User root\n\nHost web\n    HostName 1.2.3.4\n    User deploy\n    Port 2222\n    IdentityFile ~/.ssh/id_web\n"
	hosts, err := parseSSHConfig(strings.NewReader(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 {
		t.Fatalf("got %d hosts, want 1: %+v", len(hosts), hosts)
	}
	h := hosts[0].Host
	if h.Alias != "web" || h.Hostname != "1.2.3.4" || h.User != "deploy" || h.Port != 2222 || h.IdentityFile != "~/.ssh/id_web" {
		t.Fatalf("parsed %+v", h)
	}
}

func importConfig(t *testing.T, s *Store, cfg string) (int, []string, *Data) {
	t.Helper()
	var n int
	var notes []string
	err := s.Edit(func(d *Data) error {
		var err error
		n, notes, err = d.ImportSSHConfig(strings.NewReader(cfg))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.Data()
	if err != nil {
		t.Fatal(err)
	}
	return n, notes, d
}

func hostByAlias(d *Data, alias string) *Host {
	for i := range d.Hosts.Items {
		if d.Hosts.Items[i].Alias == alias {
			return &d.Hosts.Items[i]
		}
	}
	return nil
}

func TestImportSSHConfigLinksProxyJumpByAlias(t *testing.T) {
	s := openTestStore(t)
	cfg := "Host app\n  HostName 10.0.0.5\n  User deploy\n  ProxyJump bastion\n\nHost bastion\n  HostName bastion.example.com\n  User jump\n"
	n, notes, d := importConfig(t, s, cfg)
	if n != 2 || len(notes) != 0 {
		t.Fatalf("imported %d, notes %v; want 2, none", n, notes)
	}
	app, bastion := hostByAlias(d, "app"), hostByAlias(d, "bastion")
	if app == nil || bastion == nil {
		t.Fatalf("hosts = %+v", d.Hosts.Items)
	}
	if app.JumpHostID == nil || *app.JumpHostID != bastion.ID {
		t.Fatalf("app jump = %v, want bastion %s", app.JumpHostID, bastion.ID)
	}
	if bastion.JumpHostID != nil {
		t.Fatalf("bastion has a jump host %v", *bastion.JumpHostID)
	}
	if app.Revision != 1 || app.DeviceID != s.DeviceID() || app.ModifiedAt.IsZero() {
		t.Fatalf("imported host meta not touched: %+v", app.Meta)
	}
}

func TestImportSSHConfigLinksToExistingHost(t *testing.T) {
	s := openTestStore(t)
	importConfig(t, s, "Host bastion\n  HostName b.example.com\n")
	n, notes, d := importConfig(t, s, "Host bastion\n  HostName other\n\nHost db\n  ProxyJump admin@bastion:22\n")
	if n != 1 || len(notes) != 0 {
		t.Fatalf("imported %d, notes %v; want 1, none", n, notes)
	}
	db, bastion := hostByAlias(d, "db"), hostByAlias(d, "bastion")
	if db.JumpHostID == nil || *db.JumpHostID != bastion.ID || bastion.Hostname != "b.example.com" {
		t.Fatalf("db jump %v, bastion %+v", db.JumpHostID, bastion)
	}
}

func TestImportSSHConfigUnknownJumpAliasIsReported(t *testing.T) {
	s := openTestStore(t)
	n, notes, d := importConfig(t, s, "Host app\n  HostName 10.0.0.5\n  ProxyJump nowhere\n")
	if n != 1 {
		t.Fatalf("imported %d, want 1", n)
	}
	if app := hostByAlias(d, "app"); app == nil || app.JumpHostID != nil {
		t.Fatalf("app = %+v, want imported without a jump host", app)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "nowhere") || !strings.Contains(notes[0], "app") {
		t.Fatalf("notes = %v, want one naming app and nowhere", notes)
	}
}
