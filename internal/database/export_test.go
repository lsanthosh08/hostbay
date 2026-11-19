package database

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func exportOf(t *testing.T, sets []resultSet, format string) (string, int64) {
	t.Helper()
	var b bytes.Buffer
	n, err := Export(context.Background(), &b, &memResults{sets: sets}, format)
	if err != nil {
		t.Fatal(err)
	}
	return b.String(), n
}

// CSV quotes as RFC 4180 does, leaves NULL empty, writes binary as hex,
// keeps a cell from running as a spreadsheet formula, and never cuts a
// value the grid would show cut.
func TestExportCSV(t *testing.T) {
	long := strings.Repeat("x", CellCap+10)
	sets := []resultSet{
		{}, // a statement without rows comes first
		{cols: []Column{{Name: "id"}, {Name: "note"}, {Name: "bin", Binary: true}}, next: rowList([][]any{
			{int64(1), "a,\"b\"\nc", []byte{0xde, 0xad}},
			{int64(-5), nil, nil},
			{int64(3), "=HYPERLINK(\"x\")", nil},
			{int64(4), "-1+1", nil},
			{int64(5), long, nil},
		})},
	}
	out, n := exportOf(t, sets, "csv")
	want := "id,note,bin\n1,\"a,\"\"b\"\"\nc\",0xdead\n-5,,\n3,\"'=HYPERLINK(\"\"x\"\")\",\n4,'-1+1,\n5," + long + ",\n"
	if n != 5 || out != want {
		t.Fatalf("csv (%d rows):\n%q\nwant\n%q", n, out[:min(len(out), 200)], want[:200])
	}
}

// JSON is an array of objects with keys in column order, numbers as
// numbers and NULL as null.
func TestExportJSON(t *testing.T) {
	sets := []resultSet{{cols: []Column{{Name: "id"}, {Name: "name"}, {Name: "ok"}}, next: rowList([][]any{
		{int64(1), "ann", true},
		{int64(2), nil, false},
	})}}
	out, n := exportOf(t, sets, "json")
	if n != 2 || out != "[\n{\"id\":1,\"name\":\"ann\",\"ok\":true},\n{\"id\":2,\"name\":null,\"ok\":false}\n]\n" {
		t.Fatalf("json:\n%s", out)
	}
	var v []map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatal(err)
	}
	if empty, _ := exportOf(t, []resultSet{{cols: []Column{{Name: "x"}}, next: rowList(nil)}}, "json"); empty != "[\n]\n" {
		t.Fatalf("empty = %q", empty)
	}
}

// An export runs on a connection of its own: the tab's session, and the
// cursor open on it, are left as they are.
func TestExportLeavesTheSessionAlone(t *testing.T) {
	e := openSQLite(t, sqliteFile(t, "x.db", "CREATE TABLE t (id INTEGER PRIMARY KEY); INSERT INTO t VALUES (1), (2), (3);"))
	ctx := context.Background()
	grid, err := e.Query(ctx, "SELECT id FROM t ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := grid.Next(); !ok || err != nil {
		t.Fatal(err)
	}
	r, err := e.(Exporter).ExportTable(ctx, []string{"t"}, Browse{Sort: "id", Desc: true})
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if n, err := Export(ctx, &b, r, "csv"); err != nil || n != 3 || b.String() != "id\n3\n2\n1\n" {
		t.Fatalf("export = %q, %d, %v", b.String(), n, err)
	}
	r.Close()
	if row, ok, err := grid.Next(); !ok || err != nil || row[0] != int64(2) {
		t.Fatalf("the grid's cursor after the export: %v %v %v", row, ok, err)
	}
	grid.Close()
}
