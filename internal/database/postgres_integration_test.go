//go:build integration

package database

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hostbay/internal/dockertest"
)

// A PostgreSQL server reachable only from the far end of a two-hop SSH
// chain browses, queries, pages and cancels through it; every dial,
// cancel requests included, goes through the chain.
func TestPostgresThroughATwoHopChain(t *testing.T) {
	dockertest.Require(t)
	network, c := twoHopChain(t)
	pg := dockertest.Name("hostbay-pg")
	const pw = "it-Pg-Pass-3!" // a throwaway container's password
	dockertest.Start(t, pg, "--network", network, "-e", "POSTGRES_PASSWORD="+pw, "-e", "POSTGRES_DB=app", "postgres:17-alpine")
	// The image's first start runs a server on the socket only; TCP
	// answers once the real one is up.
	dockertest.WaitExec(t, pg, 2*time.Minute, "pg_isready", "-h", "127.0.0.1", "-U", "postgres")

	ctx := context.Background()
	d := &chainDial{c: c, want: net.JoinHostPort(pg, "5432")} // lib/pq dials cancel requests from its own goroutine
	cfg := PGConfig{
		Host: pg, Port: 5432, User: "postgres", Password: pw, Database: "app", TLS: "none",
		NoClientCert: filepath.Join(t.TempDir(), "never", "client.crt"),
		Dial:         d.dial,
	}
	e, err := OpenPostgres(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	r := NewRegistry(func(State) {})
	if rows := all(t, r, e, "SELECT 1"); len(rows) != 1 || rows[0][0].Text != "1" {
		t.Fatalf("SELECT 1 = %+v", rows)
	}
	all(t, r, e, "CREATE TABLE n (i int PRIMARY KEY); INSERT INTO n SELECT generate_series(1, 1200)")
	browseQueryPageEdit(t, e, "public", "n", "i")

	before := d.count()
	sleep := r.Start(ctx, "tab", func(ctx context.Context) (Results, error) { return e.Query(ctx, "SELECT pg_sleep(60)") })
	time.Sleep(time.Second)
	start := time.Now()
	r.Cancel(sleep)
	_, err = r.Next(ctx, sleep)
	if err == nil || (!errors.Is(err, context.Canceled) && Category(err) != "cancelled") || time.Since(start) > 2*time.Second {
		t.Fatalf("cancel: %v after %v", err, time.Since(start))
	}
	if d.count() == before {
		t.Fatal("the cancel request did not go through the dial function")
	}
	if n := dockertest.Docker(t, "exec", pg, "psql", "-U", "postgres", "-tAc",
		"SELECT count(*) FROM pg_stat_activity WHERE query = 'SELECT pg_sleep(60)' AND state = 'active'"); n != "0" {
		t.Fatalf("the server still runs %s cancelled queries", n)
	}

	cfg.Database = "x sslmode=disable"
	if _, err := OpenPostgres(ctx, cfg); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("literal database name: %v", err)
	}
}
