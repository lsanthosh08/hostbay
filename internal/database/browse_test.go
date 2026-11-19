package database

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// browseRows runs a browse of t to the end and returns each row's cells
// joined by "|".
func browseRows(t *testing.T, e Engine, path []string, b Browse) ([]string, error) {
	t.Helper()
	r, err := e.(Browser).BrowseTable(context.Background(), path, b)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var out []string
	for {
		vals, ok, err := r.Next()
		if err != nil || !ok {
			return out, err
		}
		cells := make([]string, len(vals))
		for i, v := range vals {
			c, _ := encode(v, false)
			cells[i] = c.Text
			if c.Null {
				cells[i] = "NULL"
			}
		}
		out = append(out, strings.Join(cells, "|"))
	}
}

const browseSetup = `CREATE TABLE "my ""t""" (id INTEGER PRIMARY KEY, "na""me" TEXT, n INTEGER);
INSERT INTO "my ""t""" VALUES (1, 'Ann', 3), (2, 'bob', NULL), (3, '50% off', 1), (4, 'x_y', 2), (5, ''' OR 1=1 --', 5);`

// A browse sorts and filters on the server by columns the catalog names,
// with every value a parameter: a value never becomes SQL, and LIKE's
// wildcards in it match only themselves.
func TestBrowseSortsAndFiltersByParameters(t *testing.T) {
	e := openSQLite(t, sqliteFile(t, "b.db", browseSetup))
	path := []string{`my "t"`}
	cases := []struct {
		name string
		b    Browse
		want []string
	}{
		{"sort desc", Browse{Sort: "n", Desc: true}, []string{"5|' OR 1=1 --|5", "1|Ann|3", "4|x_y|2", "3|50% off|1", "2|bob|NULL"}},
		{"contains ignores case", Browse{Filters: []Filter{{Column: `na"me`, Op: "contains", Value: "AN"}}}, []string{"1|Ann|3"}},
		{"percent is literal", Browse{Filters: []Filter{{Column: `na"me`, Op: "contains", Value: "%"}}}, []string{"3|50% off|1"}},
		{"underscore is literal", Browse{Filters: []Filter{{Column: `na"me`, Op: "contains", Value: "_"}}}, []string{"4|x_y|2"}},
		{"escape char is literal", Browse{Filters: []Filter{{Column: `na"me`, Op: "contains", Value: "!"}}}, nil},
		{"injection is a value", Browse{Filters: []Filter{{Column: `na"me`, Op: "equals", Value: "' OR 1=1 --"}}}, []string{"5|' OR 1=1 --|5"}},
		{"equals on a number", Browse{Filters: []Filter{{Column: "n", Op: "equals", Value: "2"}}}, []string{"4|x_y|2"}},
		{"is null", Browse{Filters: []Filter{{Column: "n", Op: "null"}}}, []string{"2|bob|NULL"}},
		{"not null and contains", Browse{Sort: "id", Filters: []Filter{{Column: "n", Op: "notNull"}, {Column: `na"me`, Op: "contains", Value: "o"}}}, []string{"3|50% off|1", "5|' OR 1=1 --|5"}},
	}
	for _, c := range cases {
		got, err := browseRows(t, e, path, c.b)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%s: %q, %v; want %q", c.name, got, err, c.want)
		}
	}
	n, err := e.(Browser).CountRows(context.Background(), path, Browse{Filters: []Filter{{Column: "n", Op: "notNull"}}})
	if err != nil || n != 4 {
		t.Fatalf("count = %d, %v", n, err)
	}
}

// Only columns the catalog names and operators from FilterOps are taken,
// so the page cannot put SQL into a sort or filter.
func TestBrowseRefusesUnknownColumnsAndOperators(t *testing.T) {
	e := openSQLite(t, sqliteFile(t, "b.db", browseSetup))
	path := []string{`my "t"`}
	for _, b := range []Browse{
		{Sort: "id; DROP TABLE x"},
		{Filters: []Filter{{Column: "nope", Op: "equals", Value: "1"}}},
		{Filters: []Filter{{Column: "id", Op: "LIKE", Value: "1"}}},
	} {
		if _, err := browseRows(t, e, path, b); err == nil {
			t.Errorf("%+v was taken", b)
		}
	}
	if _, err := browseRows(t, e, []string{"a", "b"}, Browse{}); err == nil {
		t.Error("a path that is not a table was browsed")
	}
}

// SQL Server's LIKE also treats [ as a wildcard, so its filters escape it;
// the others do not, because Oracle refuses an escape before an ordinary
// character.
func TestBrowseEscapesEachDialectsWildcards(t *testing.T) {
	for _, c := range []struct {
		d    dialect
		want string
	}{
		{mssqlDialect, "%a![b!%%"},
		{oraDialect, "%a[b!%%"},
	} {
		e := &sqlEngine{d: c.d}
		_, _, args, err := e.browseWhere(Browse{Filters: []Filter{{Column: "c", Op: "contains", Value: "a[b%"}}}, []string{"c"}, 1)
		if err != nil || len(args) != 1 || args[0] != c.want {
			t.Errorf("args = %q, %v; want %q", args, err, c.want)
		}
	}
}
