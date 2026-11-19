package app

import (
	"context"
	"database/sql"
	"path/filepath"
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
