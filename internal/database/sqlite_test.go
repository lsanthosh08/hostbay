package database

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// sqliteFile creates a SQLite file named name holding setup, written with
// the driver's defaults (not through OpenSQLite).
func sqliteFile(t *testing.T, name, setup string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(setup); err != nil {
		t.Fatal(err)
	}
	return path
}

func openSQLite(t *testing.T, path string) Engine {
	t.Helper()
	e, err := OpenSQLite(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

// events records query-state events.
type events struct {
	mu sync.Mutex
	s  []State
}

func (e *events) emit(s State) {
	e.mu.Lock()
	e.s = append(e.s, s)
	e.mu.Unlock()
}

func (e *events) last(id string) State {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := len(e.s) - 1; i >= 0; i-- {
		if e.s[i].QueryID == id {
			return e.s[i]
		}
	}
	return State{}
}

func run(e Engine, text string) func(context.Context) (Results, error) {
	return func(ctx context.Context) (Results, error) { return e.Query(ctx, text) }
}

const numbers = `CREATE TABLE n (i INTEGER PRIMARY KEY);
WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < 1200) INSERT INTO n SELECT x FROM c;`

func TestSQLiteTreeListsTablesViewsAndColumns(t *testing.T) {
	e := openSQLite(t, sqliteFile(t, "app.db", `CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT);
		CREATE VIEW names AS SELECT name FROM users;`))
	nodes, more, err := e.Children(context.Background(), nil, 0)
	if err != nil || more || len(nodes) != 2 || nodes[0].Name != "names" || nodes[0].Kind != "view" || nodes[1].Kind != "table" || !nodes[1].HasChildren {
		t.Fatalf("root = %+v, %v, %v", nodes, more, err)
	}
	cols, _, err := e.Children(context.Background(), nodes[1].Path, 0)
	if err != nil || len(cols) != 2 || cols[1].Name != "name" || cols[1].Detail != "TEXT" || cols[1].HasChildren {
		t.Fatalf("columns = %+v, %v", cols, err)
	}
}

// 1,200 rows arrive as pages of 500, 500 and 200.
func TestQueryPagesRows(t *testing.T) {
	e := openSQLite(t, sqliteFile(t, "n.db", numbers))
	ev := &events{}
	r := NewRegistry(ev.emit)
	id := r.Start(context.Background(), "tab", run(e, "SELECT i FROM n ORDER BY i"))
	var sizes []int
	for {
		p, err := r.Next(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Rows) > 0 || !p.Done {
			sizes = append(sizes, len(p.Rows))
		}
		if len(sizes) == 1 && (len(p.Columns) != 1 || p.Columns[0].Name != "i" || p.Rows[0][0].Text != "1") {
			t.Fatalf("first page = %+v", p.Columns)
		}
		if p.Done {
			break
		}
	}
	if len(sizes) != 3 || sizes[0] != 500 || sizes[1] != 500 || sizes[2] != 200 {
		t.Fatalf("pages = %v", sizes)
	}
	if s := ev.last(id); s.State != "done" || s.Rows != 1200 {
		t.Fatalf("last state = %+v", s)
	}
}

// A cell over 64 KiB arrives cut, with a marker, and SaveCell writes all
// of it.
func TestLongCellIsCutAndSavedWhole(t *testing.T) {
	e := openSQLite(t, sqliteFile(t, "big.db", `CREATE TABLE b (v TEXT); INSERT INTO b VALUES (printf('%.*c', 204800, 'x') || 'end');`))
	r := NewRegistry(func(State) {})
	id := r.Start(context.Background(), "tab", run(e, "SELECT v FROM b"))
	p, err := r.Next(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	c := p.Rows[0][0]
	if !c.Cut || len(c.Text) != CellCap || c.Size != 204803 {
		t.Fatalf("cell: cut %v, %d bytes shown of %d", c.Cut, len(c.Text), c.Size)
	}
	var out bytes.Buffer
	if err := r.SaveCell(id, 0, 0, 0, &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 204803 || !strings.HasSuffix(out.String(), "end") {
		t.Fatalf("saved %d bytes", out.Len())
	}
}

// A view calling load_extension cannot run it: trusted_schema is off.
func TestSQLiteViewCannotLoadAnExtension(t *testing.T) {
	e := openSQLite(t, sqliteFile(t, "ext.db", `CREATE VIEW v AS SELECT load_extension('/nonexistent/evil') AS x;`))
	r := NewRegistry(func(State) {})
	id := r.Start(context.Background(), "tab", run(e, "SELECT * FROM v"))
	_, err := r.Next(context.Background(), id)
	if err == nil || !strings.Contains(err.Error(), "unsafe use of load_extension") {
		t.Fatalf("err = %v", err)
	}
	p, err := r.Next(context.Background(), r.Start(context.Background(), "tab", run(e, "PRAGMA trusted_schema")))
	if err != nil || p.Rows[0][0].Text != "0" {
		t.Fatalf("trusted_schema = %+v, %v", p.Rows, err)
	}
}

// A file name holding ? and # opens as that file, with no pragma from the
// name run; a missing file is reported and not created.
func TestSQLitePathIsOnlyAPath(t *testing.T) {
	plain := sqliteFile(t, "plain.db", `CREATE TABLE t (v);`)
	name := "a?_pragma=journal_mode(WAL)#x.db"
	if runtime.GOOS == "windows" { // no ? in Windows file names
		name = "a#x&_pragma=journal_mode(WAL).db"
	}
	path := filepath.Join(filepath.Dir(plain), name)
	if err := os.Rename(plain, path); err != nil {
		t.Fatal(err)
	}
	e := openSQLite(t, path)
	r := NewRegistry(func(State) {})
	p, err := r.Next(context.Background(), r.Start(context.Background(), "tab", run(e, "PRAGMA journal_mode")))
	if err != nil || p.Rows[0][0].Text != "delete" {
		t.Fatalf("journal mode = %+v, %v", p.Rows, err)
	}
	if _, err := os.Stat(path + "-wal"); err == nil {
		t.Fatal("a pragma from the file name ran")
	}

	missing := filepath.Join(t.TempDir(), "gone.db")
	if _, err := OpenSQLite(context.Background(), missing); err == nil || !strings.Contains(err.Error(), "gone.db") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("opening a missing file created it")
	}
}

// After the idle close, a request that only reads may be run again with a
// click; one that also writes may not (KTD23).
func TestExpiredResultsOfferARerunOnlyForReadingRequests(t *testing.T) {
	e := openSQLite(t, sqliteFile(t, "n.db", numbers+"CREATE TABLE log (v);"))
	ev := &events{}
	r := NewRegistry(ev.emit)
	r.idle = 50 * time.Millisecond
	for text, want := range map[string]bool{
		"SELECT i FROM n": true,
		"INSERT INTO log VALUES (1); SELECT i FROM n": false,
	} {
		id := r.Start(context.Background(), "tab", run(e, text))
		if _, err := r.Next(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for ev.last(id).State != "expired" && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if s := ev.last(id); s.State != "expired" || s.Rerun != want {
			t.Fatalf("%q: state %+v, want rerun %v", text, s, want)
		}
		if _, err := r.Next(context.Background(), id); !errors.Is(err, ErrExpired) {
			t.Fatalf("%q: next after expiry = %v", text, err)
		}
	}
}
