package dbeaver

import (
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
	if rep.Hosts != 6 || rep.Folders != 1 {
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
	// An SSH tunnel is never imported as a direct connection.
	tunnelled := importertest.Host(t, st, "tunnelled")
	if tunnelled.Jumped() || !tunnelled.Proxied() || importertest.Password(t, v, tunnelled) != "Dbv-Tun-4!" || !importertest.Noted(rep, "tunnelled", "bastion.example") {
		t.Errorf("tunnelled = %+v, notes %v", tunnelled, rep.Notes)
	}
	if h := importertest.Host(t, st, "local file"); h.KindName() != "sqlite" || store.Deref(h.File) != "/srv/data/app.db" || h.Proxied() {
		t.Errorf("local file = %+v", h)
	}
	if !strings.Contains(importertest.Skipped(rep, "by url"), "JDBC URL") || !strings.Contains(importertest.Skipped(rep, "spreadsheets"), "csv") {
		t.Errorf("skipped = %+v", rep.Skipped)
	}
	importertest.NoSecrets(t, dir, "Dbv-Orders-1!", "Dbv-Pg-2!", "Dbv-Sql-3!", "Dbv-Tun-4!", "Dbv-Ssh-5!")

	res, _ = Parse(workspace, "")
	importertest.Reimport(t, st, v, res)
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
