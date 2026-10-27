//go:build integration

package database

import (
	"context"
	"net"
	"testing"
	"time"

	"hostbay/internal/dockertest"
)

// An Oracle Free database reachable only through a two-hop SSH chain
// browses, queries, pages and edits through it; NUMBER(38) arrives whole
// as a number, and a large CLOB as a cut text cell.
func TestOracleThroughATwoHopChain(t *testing.T) {
	dockertest.Require(t)
	network, c := twoHopChain(t)
	name := dockertest.Name("hostbay-ora")
	const pw = "it-Ora-Pass-8" // a throwaway container's password
	dockertest.Start(t, name, "--network", network, "-e", "ORACLE_PASSWORD="+pw, "-e", "APP_USER=app", "-e", "APP_USER_PASSWORD="+pw,
		"gvenzl/oracle-free:23-slim")
	dockertest.WaitExec(t, name, 10*time.Minute, "healthcheck.sh")

	d := &chainDial{c: c, want: net.JoinHostPort(name, "1521")}
	e, err := OpenOracle(context.Background(), ServerConfig{Host: name, Port: 1521, User: "app", Password: pw, Database: "FREEPDB1", TLS: "none",
		Dial: d.dial})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	r := NewRegistry(func(State) {})
	all(t, r, e, "CREATE TABLE n (i NUMBER(38) PRIMARY KEY)")
	all(t, r, e, "INSERT INTO n SELECT level FROM dual CONNECT BY level <= 1200")
	all(t, r, e, `CREATE TABLE q (id NUMBER PRIMARY KEY, "a b;" VARCHAR2(10))`)
	all(t, r, e, `INSERT INTO q VALUES (1, 'x')`)
	all(t, r, e, "COMMIT")
	browseQueryPageEdit(t, e, "APP", "N", "I")
	d.check(t)
	editQuoted(t, e, []string{"APP", "Q"}, "ID", "a b;", `SELECT "a b;" FROM q WHERE id = 1`)

	id := r.Start(context.Background(), "tab", func(ctx context.Context) (Results, error) {
		return e.Query(ctx, "SELECT CAST(12345678901234567890123456789012345678 AS NUMBER(38)) num, RPAD(TO_CLOB('x'), 100000, 'x') big FROM dual")
	})
	p, err := r.Next(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	num, big := p.Rows[0][0], p.Rows[0][1]
	if p.Columns[0].Type != "number" || num.Text != "12345678901234567890123456789012345678" {
		t.Fatalf("NUMBER(38) = %+v as %+v", num, p.Columns[0])
	}
	if !big.Cut || big.Size != 100000 || len(big.Text) != CellCap {
		t.Fatalf("CLOB = cut %v, size %d, %d shown", big.Cut, big.Size, len(big.Text))
	}
}
