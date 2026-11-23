package dbeaver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hostbay/internal/importer"
	"hostbay/internal/importer/importertest"
	"hostbay/internal/store"
)

var workspace = filepath.Join(importertest.Fixtures, "dbeaver", "workspace")

// skipped reports whether the report lists name as not imported for a
// reason containing text.
func skipped(rep importer.Report, name, text string) bool {
	for _, s := range rep.Skipped {
		if s.File == name && strings.Contains(s.Reason, text) {
			return true
		}
	}
	return false
}

func TestImportMapsConnectionsCredentialsAndOptions(t *testing.T) {
	st, v, dir := importertest.Store(t)
	// The folder and its data-sources.json import the same.
	res, err := Parse(workspace, "")
	if err != nil {
		t.Fatal(err)
	}
	if again, err := Parse(filepath.Join(workspace, "data-sources.json"), ""); err != nil || len(again.Bundle.Hosts) != len(res.Bundle.Hosts) {
		t.Fatalf("by file: %d hosts, %v", len(again.Bundle.Hosts), err)
	}
	rep := importertest.Import(t, st, v, res)
	// Nine connections and the two SSH jump hosts their tunnels use.
	if rep.Hosts != 11 || rep.Folders != 1 {
		t.Fatalf("report = %+v", rep)
	}
	// Only the first folder level is used.
	orders := importertest.Host(t, st, "orders")
	if orders.KindName() != "mysql" || orders.User != "orders_app" || importertest.Password(t, v, orders) != "Dbv-Orders-1!" ||
		store.Deref(orders.Database) != "orders" || orders.Port != 3306 || importertest.FolderPath(t, st, orders) != "Prod" {
		t.Errorf("orders = %+v", orders)
	}
	// Bootstrap SQL and a client program path are never imported.
	if !skipped(rep, "Prod/orders: bootstrap queries", "no SQL") || !skipped(rep, "Prod/orders: local client", "program") || !skipped(rep, "Prod/orders: driver options", "connectTimeout") {
		t.Errorf("orders skipped = %+v", rep.Skipped)
	}
	// sslmode=disable imports as verify, with a note.
	analytics := importertest.Host(t, st, "analytics")
	if analytics.KindName() != "postgres" || analytics.DatabaseTLS() != "verify" || importertest.Password(t, v, analytics) != "Dbv-Pg-2!" ||
		!importertest.Noted(rep, "Prod/analytics", "sslmode was not imported") {
		t.Errorf("analytics = %+v, notes %v", analytics, rep.Notes)
	}
	// trustServerCertificate imports as verify; the certificate is asked
	// about at first connect. The instance name needs its port.
	erp := importertest.Host(t, st, "erp")
	if erp.KindName() != "mssql" || erp.DatabaseTLS() != "verify" || erp.Hostname != "db1" || erp.Port != 0 ||
		!importertest.Noted(rep, "erp", "trustServerCertificate was not imported") || !importertest.Noted(rep, "erp", "asked about at first connect") ||
		!importertest.Noted(rep, "erp", `instance "SQLEXPRESS"`) {
		t.Errorf("erp = %+v, notes %v", erp, rep.Notes)
	}
	if h := importertest.Host(t, st, "nonvalidating"); h.DatabaseTLS() != "verify" || !importertest.Noted(rep, "nonvalidating", "sslFactory was not imported") {
		t.Errorf("nonvalidating = %+v, notes %v", h, rep.Notes)
	}
	// An SSH tunnel imports as a jump host with the tunnel's sign-in, and
	// two connections through the same bastion share one jump host.
	tunnelled, reports := importertest.Host(t, st, "tunnelled"), importertest.Host(t, st, "reports")
	bastion := importertest.Host(t, st, "bastion.example")
	if !tunnelled.Jumped() || tunnelled.Proxied() || *tunnelled.JumpHostID != bastion.ID || importertest.Password(t, v, tunnelled) != "Dbv-Tun-4!" {
		t.Errorf("tunnelled = %+v", tunnelled)
	}
	if !reports.Jumped() || *reports.JumpHostID != bastion.ID || reports.User != "reporter" || importertest.Password(t, v, reports) != "Dbv-Rep-7!" {
		t.Errorf("reports = %+v", reports)
	}
	if bastion.KindName() != "ssh" || bastion.Port != 22 || bastion.User != "jump" || store.Deref(bastion.AuthType) != "password" ||
		importertest.Password(t, v, bastion) != "Dbv-Ssh-5!" || bastion.FolderID != nil {
		t.Errorf("bastion = %+v", bastion)
	}
	// A key tunnel keeps its key file; its passphrase is asked at connect.
	keyed, jump := importertest.Host(t, st, "keyed"), importertest.Host(t, st, "jump.example.test")
	home, _ := os.UserHomeDir()
	if !keyed.Jumped() || *keyed.JumpHostID != jump.ID || jump.Port != 2222 || jump.User != "deploy" || store.Deref(jump.AuthType) != "key" ||
		jump.IdentityFile != filepath.Join(home, ".ssh", "id_dbeaver") || jump.Password != "" || !importertest.Noted(rep, "keyed", "passphrase") {
		t.Errorf("keyed = %+v, jump = %+v, notes %v", keyed, jump, rep.Notes)
	}
	// A proxy is not in DBeaver's export, so it is never connected around.
	if h := importertest.Host(t, st, "proxied"); h.Jumped() || !h.Proxied() || !importertest.Noted(rep, "proxied", "socks.example.test") {
		t.Errorf("proxied = %+v, notes %v", h, rep.Notes)
	}
	if h := importertest.Host(t, st, "local file"); h.KindName() != "sqlite" || store.Deref(h.File) != "/srv/data/app.db" || h.Proxied() {
		t.Errorf("local file = %+v", h)
	}
	if !strings.Contains(importertest.Skipped(rep, "by url"), "JDBC URL") || !strings.Contains(importertest.Skipped(rep, "spreadsheets"), "csv") {
		t.Errorf("skipped = %+v", rep.Skipped)
	}
	importertest.NoSecrets(t, dir, "Dbv-Orders-1!", "Dbv-Pg-2!", "Dbv-Sql-3!", "Dbv-Tun-4!", "Dbv-Ssh-5!", "Dbv-Rep-7!", "Dbv-Key-8!", "Dbv-Phrase-9!")

	res, _ = Parse(workspace, "")
	importertest.Reimport(t, st, v, res)

	// A connection deleted here and imported again links to the jump host
	// already saved, not to one from the export.
	if err := st.Edit(func(d *store.Data) error { return d.PurgeHost(tunnelled.ID) }); err != nil {
		t.Fatal(err)
	}
	res, _ = Parse(workspace, "")
	if rep := importertest.Import(t, st, v, res); rep.Hosts != 1 {
		t.Fatalf("third import added %d hosts", rep.Hosts)
	}
	if h := importertest.Host(t, st, "tunnelled"); !h.Jumped() || *h.JumpHostID != bastion.ID || h.Proxied() {
		t.Errorf("re-imported tunnelled = %+v", h)
	}
}

// Credentials sealed under a DBeaver master password cannot be read: the
// hosts import without passwords, and the report says so.
func TestMasterPasswordLeavesPasswordsBehind(t *testing.T) {
	st, v, dir := importertest.Store(t)
	res, err := Parse(filepath.Join(importertest.Fixtures, "dbeaver", "master", "data-sources.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	rep := importertest.Import(t, st, v, res)
	h := importertest.Host(t, st, "locked")
	if rep.Hosts != 1 || h.Password != "" || !importertest.Noted(rep, "credentials-config.json", "master password") {
		t.Fatalf("locked = %+v, report %+v", h, rep)
	}
	importertest.NoSecrets(t, dir, "Dbv-Locked-6!")
}

func TestPathIsRequired(t *testing.T) {
	if _, err := Parse("", ""); err == nil || !strings.Contains(err.Error(), "data-sources.json") {
		t.Errorf("empty path: %v", err)
	}
}

// A proxy or a second hop in front of an SSH tunnel is not in the
// export, so the connection keeps the missing-proxy marker and no jump
// host is imported that would be dialed around it.
func TestTunnelBehindAnotherHopKeepsMissingProxy(t *testing.T) {
	for name, handlers := range map[string]string{
		"proxy": `"proxy":{"type":"PROXY","enabled":true,"properties":{"host":"socks.example.test","port":1080}},` +
			`"ssh_tunnel":{"type":"TUNNEL","enabled":true,"properties":{"host":"bastion.example","port":22}}`,
		"two tunnels": `"a_tunnel":{"type":"TUNNEL","enabled":true,"properties":{"host":"first.example.test"}},` +
			`"b_tunnel":{"type":"TUNNEL","enabled":true,"properties":{"host":"bastion.example"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			sources := `{"connections":{"pg-1":{"provider":"postgresql","name":"chained","configuration":{"host":"192.0.2.40","port":5432,"handlers":{` + handlers + `}}}}}`
			if err := os.WriteFile(filepath.Join(dir, "data-sources.json"), []byte(sources), 0o600); err != nil {
				t.Fatal(err)
			}
			res, err := Parse(dir, "")
			if err != nil {
				t.Fatal(err)
			}
			st, v, _ := importertest.Store(t)
			rep := importertest.Import(t, st, v, res)
			if h := importertest.Host(t, st, "chained"); rep.Hosts != 1 || h.Jumped() || !h.Proxied() || !importertest.Noted(rep, "chained", "was not imported") {
				t.Errorf("chained = %+v, report %+v", h, rep)
			}
		})
	}
}
