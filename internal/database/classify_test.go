package database

import (
	"context"
	"strings"
	"testing"
)

// Every engine judges a request before it runs, the same way it judges
// whether results may run again: a read-only tab and the confirmation
// before a write both rest on it.
func TestReadOnlyJudgement(t *testing.T) {
	cases := []struct {
		name string
		e    Engine
		text string
		want bool
	}{
		{"sql select", &sqlEngine{d: pgDialect}, "SELECT * FROM t", true},
		{"sql with in a comment", &sqlEngine{d: pgDialect}, "SELECT 1 -- DELETE", true},
		{"sql delete", &sqlEngine{d: pgDialect}, "DELETE FROM t", false},
		{"sql select into", &sqlEngine{d: pgDialect}, "SELECT * INTO x FROM t", false},
		{"sql set", &sqlEngine{d: pgDialect}, "SET default_transaction_read_only = off", false},
		{"sql unterminated", &sqlEngine{d: pgDialect}, "SELECT 'x", false},
		{"t-sql batch without semicolons", &sqlEngine{d: mssqlDialect}, "SELECT 1 DROP TABLE t", false},
		{"t-sql exec", &sqlEngine{d: mssqlDialect}, "SELECT 1\nEXEC sp_who", false},
		{"t-sql truncate", &sqlEngine{d: mssqlDialect}, "SELECT 1 TRUNCATE TABLE t", false},
		{"t-sql updatetext", &sqlEngine{d: mssqlDialect}, "SELECT 1 UPDATETEXT t.c @p 0 NULL 'x'", false},
		{"t-sql writetext", &sqlEngine{d: mssqlDialect}, "SELECT 1 WRITETEXT t.c @p 'x'", false},
		{"t-sql receive", &sqlEngine{d: mssqlDialect}, "SELECT 1 RECEIVE TOP(1) * FROM q", false},
		{"t-sql disable trigger", &sqlEngine{d: mssqlDialect}, "SELECT 1 DISABLE TRIGGER tr ON t", false},
		{"t-sql show-led batch", &sqlEngine{d: mssqlDialect}, "SHOW x DELETE FROM t", false},
		{"mysql executable comment", &sqlEngine{d: mysqlDialect}, "/*!40000 ALTER TABLE t DISABLE KEYS */; SELECT 1", false},
		{"mysql show create", &sqlEngine{d: mysqlDialect}, "SHOW CREATE TABLE t", true},
		{"mysql show then drop", &sqlEngine{d: mysqlDialect}, "SHOW TABLES; DROP TABLE t", false},
		{"mysql plain comment", &sqlEngine{d: mysqlDialect}, "/* note */ SELECT 1", true},
		// MySQL starts a -- comment only before whitespace: here it is two
		// minus signs, and the DELETE is a second statement.
		{"mysql -- without a space", &sqlEngine{d: mysqlDialect}, "SELECT 1--1; DELETE FROM t", false},
		{"mysql -- comment", &sqlEngine{d: mysqlDialect}, "SELECT 1 -- DELETE", true},
		{"mysql -- at the end", &sqlEngine{d: mysqlDialect}, "SELECT 1 --", true},
		{"sql word in a string", &sqlEngine{d: pgDialect}, "SELECT 'drop table' AS x", true},
		{"redis reads", &redisEngine{}, "GET a\nHGETALL b", true},
		{"redis write", &redisEngine{}, "GET a\nDEL b", false},
		{"redis empty", &redisEngine{}, "\n", false},
		{"mongo find", &mongoEngine{}, `{"find": "users", "filter": {}}`, true},
		{"mongo find with $db", &mongoEngine{}, `{"$db": "app", "find": "users"}`, true},
		{"mongo aggregate $out", &mongoEngine{}, `{"aggregate": "u", "pipeline": [{"$out": "x"}]}`, false},
		// JSON escapes decode to the same key the server runs.
		{"mongo escaped $out", &mongoEngine{}, `{"aggregate": "u", "pipeline": [{"\u0024out": "x"}]}`, false},
		{"mongo $merge in a $lookup", &mongoEngine{}, `{"aggregate": "u", "pipeline": [{"$lookup": {"from": "o", "as": "o", "pipeline": [{"$merge": "x"}]}}]}`, false},
		{"mongo $out as a value", &mongoEngine{}, `{"find": "u", "filter": {"note": "$out"}}`, true},
		{"mongo delete", &mongoEngine{}, `{"delete": "users", "deletes": []}`, false},
		{"mongo not json", &mongoEngine{}, `find users`, false},
		{"es get", &esEngine{}, "GET /logs/_search", true},
		{"es post search", &esEngine{}, "POST /logs/_search\n{}", true},
		{"es delete", &esEngine{}, "DELETE /logs", false},
		{"es post doc", &esEngine{}, "POST /logs/_doc\n{}", false},
		{"es bad", &esEngine{}, "logs", false},
	}
	for _, c := range cases {
		if got := ReadOnly(c.e, c.text); got != c.want {
			t.Errorf("%s: ReadOnly(%q) = %v, want %v", c.name, c.text, got, c.want)
		}
	}
}

// An engine that cannot judge is treated as writing.
func TestReadOnlyWithoutClassifier(t *testing.T) {
	if ReadOnly(noClassifier{}, "SELECT 1") {
		t.Fatal("an engine without a classifier was judged read-only")
	}
}

type noClassifier struct{ Engine }

// SetReadOnly holds the SQLite session read-only on the server, so a
// function the text judgement cannot see still cannot write, and a session
// opened after the setting (a dropped one) is read-only too.
func TestSQLiteSessionReadOnly(t *testing.T) {
	e := openSQLite(t, sqliteFile(t, "ro.db", "CREATE TABLE t (id INTEGER PRIMARY KEY)"))
	ctx := context.Background()
	run := func(q string) error {
		r, err := e.Query(ctx, q)
		if err != nil {
			return err
		}
		defer r.Close()
		for {
			_, ok, err := r.Next()
			if err != nil || !ok {
				return err
			}
		}
	}
	if err := run("SELECT 1"); err != nil { // the session is open
		t.Fatal(err)
	}
	if err := e.(SessionGuard).SetReadOnly(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := run("INSERT INTO t VALUES (1)"); err == nil || !strings.Contains(err.Error(), "readonly") {
		t.Fatalf("insert on a read-only session: %v", err)
	}
	se := e.(*sqlEngine)
	se.mu.Lock()
	se.dropSession()
	se.mu.Unlock()
	if err := run("INSERT INTO t VALUES (2)"); err == nil {
		t.Fatal("a new session was not read-only")
	}
	if err := e.(SessionGuard).SetReadOnly(ctx, false); err != nil {
		t.Fatal(err)
	}
	if err := run("INSERT INTO t VALUES (3)"); err != nil {
		t.Fatalf("insert after read-only was turned off: %v", err)
	}
}
