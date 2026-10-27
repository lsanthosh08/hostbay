package navicat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hostbay/internal/importer"
	"hostbay/internal/importer/importertest"
	"hostbay/internal/store"
)

var fixture = filepath.Join(importertest.Fixtures, "navicat", "connections.ncx")

func TestImportMapsEnginesFoldersTunnelsAndSecrets(t *testing.T) {
	st, v, dir := importertest.Store(t)
	res, err := Parse(fixture, "")
	if err != nil {
		t.Fatal(err)
	}
	rep := importertest.Import(t, st, v, res)
	if rep.Hosts != 7 || rep.Folders != 3 {
		t.Fatalf("report = %+v", rep)
	}
	for alias, want := range map[string]struct{ kind, folder, password string }{
		"shop":    {"mysql", "Prod", "Ncx-Mysql-1!"},
		"billing": {"postgres", "Prod", "Ncx-Pg-2!"},
		"erp":     {"mssql", "Windows", "Ncx-Sql-3!"},
		"cache":   {"redis", "", "Ncx-Redis-4!"},
		"docs":    {"mongodb", "", "Ncx-Mongo-5!"},
		// v11 Blowfish, longer than one block.
		"ledger": {"oracle", "Legacy", "Ncx-Old-Pass-7!"},
	} {
		h := importertest.Host(t, st, alias)
		if h.KindName() != want.kind || importertest.FolderPath(t, st, h) != want.folder || importertest.Password(t, v, h) != want.password || h.DatabaseTLS() != "verify" {
			t.Errorf("%s = %+v", alias, h)
		}
	}
	if h := importertest.Host(t, st, "billing"); store.Deref(h.Database) != "billing" || !importertest.Noted(rep, "Prod/billing", "asked about at first connect") {
		t.Errorf("billing = %+v, notes %v", h, rep.Notes)
	}
	if h := importertest.Host(t, st, "shop"); !importertest.Noted(rep, "Prod/shop", "server without TLS") {
		t.Errorf("shop = %+v, notes %v", h, rep.Notes)
	}
	if h := importertest.Host(t, st, "erp"); h.Hostname != "db1" || h.Port != 0 || !importertest.Noted(rep, "Windows/erp", "port") {
		t.Errorf("erp = %+v, notes %v", h, rep.Notes)
	}
	if h := importertest.Host(t, st, "cache"); store.Deref(h.Database) != "2" || h.Port != 6379 {
		t.Errorf("cache = %+v", h)
	}
	// Its SSH tunnel is not carried, so it cannot connect directly.
	if h := importertest.Host(t, st, "docs"); h.Jumped() || !h.Proxied() || !importertest.Noted(rep, "docs", "bastion.example") {
		t.Errorf("docs = %+v, notes %v", h, rep.Notes)
	}
	if h := importertest.Host(t, st, "local"); store.Deref(h.File) != "/srv/data/local.db" || h.Hostname != "" {
		t.Errorf("local = %+v", h)
	}
	if got := importertest.Skipped(rep, "warehouse"); !strings.Contains(got, "SNOWFLAKE") {
		t.Errorf("warehouse skipped for %q", got)
	}
	importertest.NoSecrets(t, dir, "Ncx-Mysql-1!", "Ncx-Pg-2!", "Ncx-Sql-3!", "Ncx-Redis-4!", "Ncx-Mongo-5!", "Ncx-Ssh-6!", "Ncx-Old-Pass-7!")

	res, _ = Parse(fixture, "")
	importertest.Reimport(t, st, v, res)
}

func TestOversizedAndDeeplyNestedFilesAreRefused(t *testing.T) {
	big := filepath.Join(t.TempDir(), "big.ncx")
	if err := os.WriteFile(big, make([]byte, importer.MaxFileSize+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(big, ""); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("big: %v", err)
	}
	deep := filepath.Join(t.TempDir(), "deep.ncx")
	body := "<Connections>" + strings.Repeat("<a>", 10_000) + strings.Repeat("</a>", 10_000) + "</Connections>"
	if err := os.WriteFile(deep, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(deep, ""); err == nil || !strings.Contains(err.Error(), "nested") {
		t.Errorf("deep: %v", err)
	}
}
