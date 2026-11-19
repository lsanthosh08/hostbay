package app

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"hostbay/internal/database"
)

// sqliteTab saves a SQLite host whose file holds setup, connects it and
// returns the service with the tab's database ID.
func sqliteTab(t *testing.T, setup string) (*DatabaseService, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(setup); err != nil {
		t.Fatal(err)
	}
	db.Close()
	d, s, _, _ := newDatabaseService(t)
	d.pickOpen = func() (string, error) { return path, nil }
	pick, _ := d.DatabasePickFile()
	h, err := s.SaveHost(Host{Alias: "local", Kind: new("sqlite"), FilePick: &pick.Token})
	if err != nil {
		t.Fatal(err)
	}
	info, err := d.DatabaseConnect(context.Background(), h.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.DatabaseClose(info.DatabaseID) })
	return d, info.DatabaseID
}

// readAll reads a query's pages to the end and returns its rows' first
// cells.
func readAll(t *testing.T, d *DatabaseService, queryID string) ([]string, error) {
	t.Helper()
	var out []string
	for {
		p, err := d.DatabaseNextPage(context.Background(), queryID)
		if err != nil {
			return out, err
		}
		for _, r := range p.Rows {
			out = append(out, r[0].Text)
		}
		if p.Done {
			return out, nil
		}
	}
}

// Read-only mode refuses, in Go, every request that may change data, row
// and document edits too, while reads still run; turning it off restores
// writing.
func TestDatabaseReadOnlyMode(t *testing.T) {
	d, id := sqliteTab(t, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT); INSERT INTO users VALUES (1, 'ann');`)
	ctx := context.Background()
	if ro, err := d.DatabaseReadOnly(id, "DELETE FROM users"); err != nil || ro {
		t.Fatalf("DELETE judged read-only = %v, %v", ro, err)
	}
	if ro, _ := d.DatabaseReadOnly(id, "SELECT * FROM users"); !ro {
		t.Fatal("SELECT judged as writing")
	}
	if err := d.DatabaseSetReadOnly(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if _, err := d.DatabaseQuery(id, "DELETE FROM users"); err == nil {
		t.Fatal("a DELETE ran in read-only mode")
	}
	one, bob := "1", "bob"
	if err := d.DatabaseEditRow(ctx, id, database.RowEdit{Path: []string{"users"}, Key: map[string]*string{"id": &one}, Set: map[string]*string{"name": &bob}}); err == nil {
		t.Fatal("a row edit ran in read-only mode")
	}
	q, err := d.DatabaseQuery(id, "SELECT name FROM users")
	if err != nil {
		t.Fatal(err)
	}
	if rows, err := readAll(t, d, q); err != nil || len(rows) != 1 || rows[0] != "ann" {
		t.Fatalf("read in read-only mode = %v, %v", rows, err)
	}
	if err := d.DatabaseSetReadOnly(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	q, err = d.DatabaseQuery(id, "DELETE FROM users")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readAll(t, d, q); err != nil {
		t.Fatalf("delete after read-only was turned off: %v", err)
	}
}

// A table opened from the tree sorts and filters on the server, and its
// filtered rows are counted.
func TestDatabaseBrowseThroughTheService(t *testing.T) {
	d, id := sqliteTab(t, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT); INSERT INTO users VALUES (1, 'ann'), (2, 'bob'), (3, 'anna');`)
	ctx := context.Background()
	b := database.Browse{Sort: "id", Desc: true, Filters: []database.Filter{{Column: "name", Op: "contains", Value: "an"}}}
	tbl, err := d.DatabaseOpenTable(ctx, id, []string{"users"}, b)
	if err != nil {
		t.Fatal(err)
	}
	if rows, err := readAll(t, d, tbl.QueryID); err != nil || len(rows) != 2 || rows[0] != "3" || rows[1] != "1" {
		t.Fatalf("browse = %v, %v", rows, err)
	}
	if n, err := d.DatabaseCountRows(ctx, id, []string{"users"}, b); err != nil || n != 2 {
		t.Fatalf("count = %d, %v", n, err)
	}
	bad, err := d.DatabaseOpenTable(ctx, id, []string{"users"}, database.Browse{Sort: "nope"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readAll(t, d, bad.QueryID); err == nil { // the refusal arrives with the first page
		t.Fatal("a sort by an unknown column ran")
	}
}

// An export reruns a read and writes every row in full to the chosen
// file; a request that writes is refused before the dialog, and a
// cancelled dialog writes nothing.
func TestDatabaseExportThroughTheService(t *testing.T) {
	d, id := sqliteTab(t, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT); INSERT INTO users VALUES (1, 'ann'), (2, 'bob');`)
	ctx := context.Background()
	out := filepath.Join(t.TempDir(), "users.csv")
	d.pickSave = func(string, string) (string, error) { return out, nil }
	got, err := d.DatabaseExport(ctx, id, DatabaseExportRequest{Path: []string{"users"}, Browse: database.Browse{Sort: "id", Desc: true}, Format: "csv"})
	if err != nil || !got.Saved || got.Rows != 2 {
		t.Fatalf("export = %+v, %v", got, err)
	}
	if b, _ := os.ReadFile(out); string(b) != "id,name\n2,bob\n1,ann\n" {
		t.Fatalf("file = %q", b)
	}
	asked := false
	d.pickSave = func(string, string) (string, error) { asked = true; return out, nil }
	if _, err := d.DatabaseExport(ctx, id, DatabaseExportRequest{Text: "DELETE FROM users", Format: "csv"}); err == nil || asked {
		t.Fatalf("a writing request was exported: %v, dialog %v", err, asked)
	}
	d.pickSave = func(string, string) (string, error) { return "", nil }
	if got, err := d.DatabaseExport(ctx, id, DatabaseExportRequest{Text: "SELECT 1", Format: "json"}); err != nil || got.Saved {
		t.Fatalf("a cancelled dialog = %+v, %v", got, err)
	}
}

// Each request run is kept in the host's history on this computer, once
// when repeated and never when it looks like it holds a credential;
// saved queries are kept by name per host.
func TestDatabaseHistoryAndSavedQueries(t *testing.T) {
	d, id := sqliteTab(t, `CREATE TABLE t (x INTEGER);`)
	for _, q := range []string{"SELECT 1", "SELECT 1", "  SELECT 2  ", "CREATE USER bob PASSWORD 'pw'"} {
		if _, err := d.DatabaseQuery(id, q); err != nil {
			t.Fatal(err)
		}
	}
	if h, err := d.DatabaseHistory(id); err != nil || !slices.Equal(h, []string{"SELECT 1", "SELECT 2"}) {
		t.Fatalf("history = %q, %v", h, err)
	}
	if err := d.DatabaseClearHistory(id); err != nil {
		t.Fatal(err)
	}
	if h, _ := d.DatabaseHistory(id); len(h) != 0 {
		t.Fatalf("history after clear = %q", h)
	}
	if _, err := d.DatabaseSaveQuery(id, "b", "SELECT 2"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.DatabaseSaveQuery(id, "a", "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	qs, err := d.DatabaseSaveQuery(id, "b", "SELECT 3")
	if err != nil || len(qs) != 2 || qs[0].Name != "a" || qs[1].Text != "SELECT 3" {
		t.Fatalf("saved = %+v, %v", qs, err)
	}
	if qs, err = d.DatabaseDeleteQuery(id, "a"); err != nil || len(qs) != 1 || qs[0].Name != "b" {
		t.Fatalf("after delete = %+v, %v", qs, err)
	}
	if _, err := d.DatabaseSaveQuery(id, " ", "SELECT 1"); err == nil {
		t.Fatal("a query was saved without a name")
	}
}

// Insert and delete go through the service, after a preview of the
// statement, and are refused in read-only mode.
func TestDatabaseInsertAndDeleteRows(t *testing.T) {
	d, id := sqliteTab(t, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT);`)
	ctx := context.Background()
	s := func(v string) *string { return &v }
	ins := database.RowEdit{Path: []string{"users"}, Set: map[string]*string{"id": s("7"), "name": s("ann")}}
	if p, err := d.DatabasePreviewRow(ctx, id, "insert", ins); err != nil || p.Statement != `INSERT INTO "users" ("id", "name") VALUES (?, ?)` {
		t.Fatalf("preview = %+v, %v", p, err)
	}
	if err := d.DatabaseInsertRow(ctx, id, ins); err != nil {
		t.Fatal(err)
	}
	del := database.RowEdit{Path: []string{"users"}, Key: map[string]*string{"id": s("7")}}
	if err := d.DatabaseSetReadOnly(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if err := d.DatabaseDeleteRow(ctx, id, del); err == nil {
		t.Fatal("a delete ran in read-only mode")
	}
	if err := d.DatabaseInsertRow(ctx, id, database.RowEdit{Path: []string{"users"}, Set: map[string]*string{"id": s("8")}}); err == nil {
		t.Fatal("an insert ran in read-only mode")
	}
	d.DatabaseSetReadOnly(ctx, id, false)
	if err := d.DatabaseDeleteRow(ctx, id, del); err != nil {
		t.Fatal(err)
	}
	q, _ := d.DatabaseQuery(id, "SELECT COUNT(*) FROM users")
	if rows, err := readAll(t, d, q); err != nil || rows[0] != "0" {
		t.Fatalf("rows left = %v, %v", rows, err)
	}
}

// The editor's completion names come from the catalog: every table and
// view with its columns.
func TestDatabaseSchema(t *testing.T) {
	d, id := sqliteTab(t, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT); CREATE VIEW names AS SELECT name FROM users;`)
	tables, err := d.DatabaseSchema(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, tb := range tables {
		got[tb.Name] = tb.Columns
	}
	if !slices.Equal(got["users"], []string{"id", "name"}) || !slices.Equal(got["names"], []string{"name"}) {
		t.Fatalf("schema = %v", tables)
	}
}

// Explain shows a DELETE's plan without deleting anything, refuses a
// second statement, and in read-only mode refuses text that writes.
func TestDatabaseExplain(t *testing.T) {
	d, id := sqliteTab(t, `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT); INSERT INTO users VALUES (1, 'ann');`)
	ctx := context.Background()
	q, err := d.DatabaseExplain(id, "DELETE FROM users WHERE id = 1")
	if err != nil {
		t.Fatal(err)
	}
	if plan, err := readAll(t, d, q); err != nil || len(plan) == 0 {
		t.Fatalf("plan = %v, %v", plan, err)
	}
	if _, err := d.DatabaseExplain(id, "SELECT 1; DELETE FROM users"); err == nil {
		t.Fatal("a second statement was explained")
	}
	q, _ = d.DatabaseQuery(id, "SELECT name FROM users")
	if rows, _ := readAll(t, d, q); len(rows) != 1 {
		t.Fatalf("rows after explain = %v", rows)
	}
	if err := d.DatabaseSetReadOnly(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if _, err := d.DatabaseExplain(id, "DELETE FROM users"); err == nil {
		t.Fatal("explain of a DELETE ran in read-only mode")
	}
	if _, err := d.DatabaseExplain(id, "SELECT * FROM users"); err != nil {
		t.Fatalf("explain of a SELECT in read-only mode: %v", err)
	}
}

// History keeps no request that carries a credential in any engine's
// syntax: SQL, Redis or MongoDB, or a URL with a password in it.
func TestSecretLike(t *testing.T) {
	for _, c := range []struct {
		text   string
		secret bool
	}{
		{"CREATE USER bob PASSWORD 'pw'", true},
		{"ALTER USER bob IDENTIFIED BY pw", true},
		{"AUTH bob s3cret", true},
		{"GET a\nauth s3cret", true},
		{"HELLO 3 AUTH bob s3cret", true},
		{"ACL SETUSER bob on >s3cret", true},
		{"CONFIG SET requirepass s3cret", true},
		{"CONFIG SET masterauth s3cret", true},
		{"MIGRATE h 6379 k 0 5000 AUTH s3cret", true},
		{`{"createUser": "bob", "pwd": "s3cret", "roles": []}`, true},
		{"SELECT * FROM dblink('postgresql://bob:s3cret@db.example.test/app', 'SELECT 1')", true},
		{"SELECT 1", false},
		{"GET author", false},
		{`{"find": "users", "filter": {"name": "ann"}}`, false},
		{"SELECT 'see https://example.test/a@b'", false},
	} {
		if got := secretLike(c.text); got != c.secret {
			t.Errorf("secretLike(%q) = %v, want %v", c.text, got, c.secret)
		}
	}
}

// fakeDocs is a document engine that records saves.
type fakeDocs struct{ saves int }

func (f *fakeDocs) Children(context.Context, []string, int) ([]database.Node, bool, error) {
	return nil, false, nil
}
func (f *fakeDocs) Query(context.Context, string) (database.Results, error) { return nil, nil }
func (f *fakeDocs) Close() error                                            { return nil }
func (f *fakeDocs) EditableTable(_ context.Context, path []string) (*database.Table, error) {
	return &database.Table{Path: path, Key: []string{"_id"}}, nil
}
func (f *fakeDocs) SelectTable([]string) string { return "" }
func (f *fakeDocs) Document(context.Context, []string, string) (string, string, error) {
	return "{}", "v1", nil
}
func (f *fakeDocs) EditDocument(context.Context, []string, string, string, string) error {
	f.saves++
	return nil
}

// A document save is refused in read-only mode before the engine sees it.
func TestDatabaseSaveDocumentReadOnly(t *testing.T) {
	d, _, _, _ := newDatabaseService(t)
	f := &fakeDocs{}
	d.dbs["docs"] = &dbSession{engine: f, kind: "mongodb", ctx: context.Background(), cancel: func() {}}
	ctx := context.Background()
	if err := d.DatabaseSetReadOnly(ctx, "docs", true); err != nil {
		t.Fatal(err)
	}
	if err := d.DatabaseSaveDocument(ctx, "docs", []string{"app", "users"}, `"1"`, "v1", `{"_id": "1"}`); err == nil || f.saves != 0 {
		t.Fatalf("save in read-only mode: err = %v, saves %d", err, f.saves)
	}
	d.DatabaseSetReadOnly(ctx, "docs", false)
	if err := d.DatabaseSaveDocument(ctx, "docs", []string{"app", "users"}, `"1"`, "v1", `{"_id": "1"}`); err != nil || f.saves != 1 {
		t.Fatalf("save: err = %v, saves %d", err, f.saves)
	}
}

// The connection's password is hidden in history and in saved queries.
func TestDatabaseHistoryScrubsThePassword(t *testing.T) {
	d, id := sqliteTab(t, `CREATE TABLE t (x TEXT);`)
	d.dbs[id].secret = "s3cret-pw"
	if _, err := d.DatabaseQuery(id, "SELECT 's3cret-pw'"); err != nil {
		t.Fatal(err)
	}
	if h, _ := d.DatabaseHistory(id); !slices.Equal(h, []string{"SELECT '[hidden]'"}) {
		t.Fatalf("history = %q", h)
	}
	qs, err := d.DatabaseSaveQuery(id, "q", "SELECT 's3cret-pw'")
	if err != nil || len(qs) != 1 || qs[0].Text != "SELECT '[hidden]'" {
		t.Fatalf("saved = %+v, %v", qs, err)
	}
}
