package database

import (
	"strings"
	"testing"
)

// Explain adds the dialect's prefix to one statement and refuses
// anything that would run: a second statement, a lead it does not plan
// (ANALYZE runs the statement), or a MySQL /*! comment.
func TestExplain(t *testing.T) {
	for _, c := range []struct {
		d    dialect
		text string
		want string
	}{
		{pgDialect, "SELECT * FROM t", "EXPLAIN SELECT * FROM t"},
		{mysqlDialect, " DELETE FROM t WHERE id = 1; ", "EXPLAIN DELETE FROM t WHERE id = 1;"},
		{sqliteDialect, "WITH x AS (SELECT 1) SELECT * FROM x", "EXPLAIN QUERY PLAN WITH x AS (SELECT 1) SELECT * FROM x"},
	} {
		got, err := (&sqlEngine{d: c.d}).Explain(c.text)
		if err != nil || got != c.want {
			t.Errorf("%q: got %q, %v", c.text, got, err)
		}
	}
	e := &sqlEngine{d: pgDialect}
	for _, text := range []string{
		"SELECT 1; DELETE FROM t",
		"ANALYZE DELETE FROM t",
		// PostgreSQL reads a quoted option name as the option itself.
		`("analyze") DELETE FROM t`,
		"(analyze) DELETE FROM t",
		"/* x */ (analyze) DELETE FROM t",
		"EXPLAIN ANALYZE DELETE FROM t",
		"DROP TABLE t",
		"SELECT 1 /*! ; DELETE FROM t */",
		"SELECT 'unclosed",
		"",
	} {
		if q, err := e.Explain(text); err == nil {
			t.Errorf("%q explained as %q", text, q)
		}
	}
	// In MySQL "--1" is two minus signs, not a comment, so this is two
	// statements (in PostgreSQL the rest of the line is a comment).
	if q, err := (&sqlEngine{d: mysqlDialect}).Explain("SELECT 1--1; DELETE FROM t"); err == nil {
		t.Errorf("MySQL explained %q", q)
	}
	for _, d := range []dialect{mssqlDialect, oraDialect} {
		if e := (&sqlEngine{d: d}); e.CanExplain() {
			t.Errorf("explain offered on a dialect with no plan prefix: %+v", strings.TrimSpace(d.explain))
		}
	}
}
