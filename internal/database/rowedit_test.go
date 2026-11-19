package database

import (
	"context"
	"strings"
	"testing"
)

const weird = `a"; DROP TABLE t; --`

func editable(t *testing.T) (Engine, RowEditor) {
	t.Helper()
	e := openSQLite(t, sqliteFile(t, "edit.db", `CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT NOT NULL CHECK (length(name) < 10), "a""; DROP TABLE t; --" TEXT);
		INSERT INTO t VALUES (1, 'one', 'x'), (2, 'two', 'y');
		CREATE TABLE loose (v TEXT);
		CREATE TABLE coded (code TEXT NOT NULL UNIQUE, v TEXT);`))
	return e, e.(RowEditor)
}

func str(s string) *string { return &s }

// rowsOf reads table t as id -> name|weird.
func rowsOf(t *testing.T, e Engine) map[string]string {
	t.Helper()
	r := NewRegistry(func(State) {})
	p, err := r.Next(context.Background(), r.Start(context.Background(), "tab", run(e, `SELECT * FROM t ORDER BY id`)))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, row := range p.Rows {
		out[row[0].Text] = row[1].Text + "|" + row[2].Text
	}
	return out
}

// An edit by primary key changes that row only, also of a column whose
// name is an injection attempt.
func TestEditRowChangesOnlyThatRow(t *testing.T) {
	e, ed := editable(t)
	tbl, err := ed.EditableTable(context.Background(), []string{"t"})
	if err != nil || tbl == nil || len(tbl.Key) != 1 || tbl.Key[0] != "id" || tbl.Edit != nil || tbl.NoNull {
		t.Fatalf("table = %+v, %v; want every column editable, NULL allowed", tbl, err)
	}
	if err := ed.EditRow(context.Background(), RowEdit{Path: []string{"t"}, Key: map[string]*string{"id": str("2")}, Set: map[string]*string{"name": str("deux")}}); err != nil {
		t.Fatal(err)
	}
	if err := ed.EditRow(context.Background(), RowEdit{Path: []string{"t"}, Key: map[string]*string{"id": str("2")}, Set: map[string]*string{weird: nil}}); err != nil {
		t.Fatal(err)
	}
	if got := rowsOf(t, e); got["1"] != "one|x" || got["2"] != "deux|" || len(got) != 2 {
		t.Fatalf("rows = %v", got)
	}
}

// A constraint violation shows the database's error and changes nothing;
// a row deleted meanwhile changes nothing and says so.
func TestEditRowRollsBackUnlessExactlyOneRowChanged(t *testing.T) {
	e, ed := editable(t)
	err := ed.EditRow(context.Background(), RowEdit{Path: []string{"t"}, Key: map[string]*string{"id": str("1")}, Set: map[string]*string{"name": str("far too long a name")}})
	if err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Fatalf("err = %v", err)
	}
	err = ed.EditRow(context.Background(), RowEdit{Path: []string{"t"}, Key: map[string]*string{"id": str("99")}, Set: map[string]*string{"name": str("new")}})
	if err == nil || !strings.Contains(err.Error(), "deleted meanwhile") {
		t.Fatalf("err = %v", err)
	}
	if got := rowsOf(t, e); got["1"] != "one|x" || got["2"] != "two|y" {
		t.Fatalf("rows = %v", got)
	}
}

// A table without a primary or unique key opens read-only; a NOT NULL
// unique column is a key.
func TestTableWithoutKeyIsReadOnly(t *testing.T) {
	_, ed := editable(t)
	if tbl, err := ed.EditableTable(context.Background(), []string{"loose"}); err != nil || tbl != nil {
		t.Fatalf("loose = %+v, %v", tbl, err)
	}
	if err := ed.EditRow(context.Background(), RowEdit{Path: []string{"loose"}, Key: map[string]*string{"v": str("a")}, Set: map[string]*string{"v": str("b")}}); err == nil {
		t.Fatal("a table without a key was edited")
	}
	if tbl, err := ed.EditableTable(context.Background(), []string{"coded"}); err != nil || tbl == nil || tbl.Key[0] != "code" {
		t.Fatalf("coded = %+v, %v", tbl, err)
	}
}

// The key comes from the table, never from the edit: naming other columns
// as the key is refused.
func TestEditRowRefusesAKeyThatIsNotTheTables(t *testing.T) {
	_, ed := editable(t)
	err := ed.EditRow(context.Background(), RowEdit{Path: []string{"t"}, Key: map[string]*string{"name": str("one")}, Set: map[string]*string{"name": str("x")}})
	if err == nil || !strings.Contains(err.Error(), "key does not match") {
		t.Fatalf("err = %v", err)
	}
}

// A row is inserted with only the columns given, each checked against
// the catalog and bound as a parameter; the others take their defaults.
func TestInsertRow(t *testing.T) {
	e, _ := editable(t)
	w := e.(RowWriter)
	ctx := context.Background()
	if err := w.InsertRow(ctx, RowEdit{Path: []string{"t"}, Set: map[string]*string{"id": str("3"), "name": str(weird[:9]), weird: nil}}); err != nil {
		t.Fatal(err)
	}
	if got := rowsOf(t, e)["3"]; got != weird[:9]+"|" {
		t.Fatalf("inserted row = %q", got)
	}
	for _, bad := range []map[string]*string{{"nope": str("1")}, {}, {"name": str("far too long a name")}} {
		if err := w.InsertRow(ctx, RowEdit{Path: []string{"t"}, Set: bad}); err == nil {
			t.Errorf("insert of %v was taken", bad)
		}
	}
	if len(rowsOf(t, e)) != 3 {
		t.Fatal("a refused insert left a row")
	}
}

// A delete names its row by the table's own key and commits only when
// exactly that row went.
func TestDeleteRow(t *testing.T) {
	e, _ := editable(t)
	w := e.(RowWriter)
	ctx := context.Background()
	if err := w.DeleteRow(ctx, RowEdit{Path: []string{"t"}, Key: map[string]*string{"id": str("2")}}); err != nil {
		t.Fatal(err)
	}
	if rows := rowsOf(t, e); len(rows) != 1 || rows["1"] == "" {
		t.Fatalf("rows after delete = %v", rows)
	}
	if err := w.DeleteRow(ctx, RowEdit{Path: []string{"t"}, Key: map[string]*string{"id": str("2")}}); err == nil || !strings.Contains(err.Error(), "meanwhile") {
		t.Fatalf("a delete of a row already gone: %v", err)
	}
	if err := w.DeleteRow(ctx, RowEdit{Path: []string{"loose"}, Key: map[string]*string{"v": str("x")}}); err == nil {
		t.Fatal("a row of a table without a key was deleted")
	}
	if err := w.DeleteRow(ctx, RowEdit{Path: []string{"t"}, Key: map[string]*string{"name": str("one")}}); err == nil {
		t.Fatal("a row named by a column that is not the key was deleted")
	}
}

// The preview is the statement the change would run, built the same way,
// with its values in order; it changes nothing.
func TestPreviewRow(t *testing.T) {
	e, _ := editable(t)
	w := e.(RowWriter)
	p, err := w.PreviewRow(context.Background(), "update", RowEdit{Path: []string{"t"}, Key: map[string]*string{"id": str("1")}, Set: map[string]*string{"name": str("uno"), weird: nil}})
	if err != nil {
		t.Fatal(err)
	}
	want := `UPDATE "t" SET "a""; DROP TABLE t; --" = ?, "name" = ? WHERE "id" = ?`
	if p.Statement != want || len(p.Values) != 3 || p.Values[0] != nil || *p.Values[1] != "uno" || *p.Values[2] != "1" {
		t.Fatalf("preview = %q %v", p.Statement, p.Values)
	}
	if d, err := w.PreviewRow(context.Background(), "delete", RowEdit{Path: []string{"t"}, Key: map[string]*string{"id": str("1")}}); err != nil || d.Statement != `DELETE FROM "t" WHERE "id" = ?` {
		t.Fatalf("delete preview = %q, %v", d.Statement, err)
	}
	if rowsOf(t, e)["1"] != "one|x" {
		t.Fatal("a preview changed the row")
	}
}
