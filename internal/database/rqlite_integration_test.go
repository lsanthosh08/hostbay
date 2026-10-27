//go:build integration

package database

import (
	"context"
	"net"
	"testing"
	"time"

	"hostbay/internal/dockertest"
)

// An rqlite node reachable only through a two-hop SSH chain browses,
// queries, pages and edits through it, over its HTTP API.
func TestRqliteThroughATwoHopChain(t *testing.T) {
	dockertest.Require(t)
	network, c := twoHopChain(t)
	name := dockertest.Name("hostbay-rq")
	dockertest.Start(t, name, "--network", network, "rqlite/rqlite:8.43.0")
	dockertest.WaitExec(t, name, 2*time.Minute, "wget", "-qO-", "http://127.0.0.1:4001/readyz")

	d := &chainDial{c: c, want: net.JoinHostPort(name, "4001")}
	e, err := OpenRqlite(context.Background(), ServerConfig{URL: "http://" + name + ":4001",
		Dial: d.dial})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	r := NewRegistry(func(State) {})
	all(t, r, e, "CREATE TABLE n (i INTEGER PRIMARY KEY)")
	all(t, r, e, "INSERT INTO n WITH RECURSIVE s(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM s WHERE i < 1200) SELECT i FROM s")
	all(t, r, e, `CREATE TABLE q (id INTEGER PRIMARY KEY, "a""b" TEXT, b BLOB)`)
	all(t, r, e, `INSERT INTO q VALUES (1, 'x', x'00ff')`)
	browseQueryPageEdit(t, e, "", "n", "i")
	editQuoted(t, e, []string{"q"}, "id", `a"b`, `SELECT "a""b" FROM q WHERE id = 1`)
	if rows := all(t, r, e, "SELECT b FROM q"); !rows[0][0].Binary || rows[0][0].Text != "00ff" {
		t.Fatalf("blob = %+v", rows)
	}
	d.check(t)
}
