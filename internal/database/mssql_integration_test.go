//go:build integration

package database

import (
	"context"
	"crypto/tls"
	"net"
	"runtime"
	"testing"
	"time"

	"hostbay/internal/dockertest"
	"hostbay/internal/tlstrust"
)

// A SQL Server reachable only through a two-hop SSH chain browses,
// queries, pages and edits through it, also a column named with "]".
// SQL Server's image is amd64 only and crashes under emulation, so an ARM
// machine runs Azure SQL Edge, the same engine built for ARM.
func TestMSSQLThroughATwoHopChain(t *testing.T) {
	dockertest.Require(t)
	network, c := twoHopChain(t)
	name := dockertest.Name("hostbay-ms")
	const pw = "it-Ms-Pass-5!" // a throwaway container's password
	image := "mcr.microsoft.com/mssql/server:2022-latest"
	if runtime.GOARCH == "arm64" {
		image = "mcr.microsoft.com/azure-sql-edge:latest"
	}
	dockertest.Start(t, name, "--network", network, "-e", "ACCEPT_EULA=Y", "-e", "MSSQL_SA_PASSWORD="+pw, image)

	d := &chainDial{c: c, want: net.JoinHostPort(name, "1433")}
	cfg := ServerConfig{Host: name, Port: 1433, User: "sa", Password: pw, TLS: "none",
		Dial: d.dial}
	var e Engine
	var err error
	for deadline := time.Now().Add(5 * time.Minute); ; time.Sleep(2 * time.Second) { // until the server is up
		if e, err = OpenMSSQL(context.Background(), cfg); err == nil || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	r := NewRegistry(func(State) {})
	all(t, r, e, `CREATE TABLE n (i int PRIMARY KEY);
		WITH d(x) AS (SELECT x FROM (VALUES (0), (1), (2), (3), (4), (5), (6), (7), (8), (9)) v(x))
		INSERT INTO n SELECT a.x*1000 + b.x*100 + c.x*10 + e.x + 1 FROM d a, d b, d c, d e WHERE a.x*1000 + b.x*100 + c.x*10 + e.x < 1200;
		CREATE TABLE q (id int PRIMARY KEY, [a]]b] nvarchar(10)); INSERT INTO q VALUES (1, 'x')`)
	browseQueryPageEdit(t, e, "dbo", "n", "i")
	d.check(t)
	editQuoted(t, e, []string{"dbo", "q"}, "id", "a]b", "SELECT [a]]b] FROM q WHERE id = 1")

	// Under verify, the certificate the server made for itself, which
	// names no host Hostbay dials, pins and then connects encrypted.
	certs := tlstrust.New(t.TempDir())
	cfg.TLS = "verify"
	cfg.Trust = func(try func(*tls.Config) error) (*tls.Config, error) {
		return certs.Capture("mssql", "h1", name, 1433, try)
	}
	ve := pinThenOpen(t, certs, func() (Engine, error) { return OpenMSSQL(context.Background(), cfg) })
	defer ve.Close()
	if rows := all(t, r, ve, "SELECT encrypt_option FROM sys.dm_exec_connections WHERE session_id = @@SPID"); rows[0][0].Text != "TRUE" {
		t.Fatalf("the verified session is not encrypted: %+v", rows)
	}
	d.check(t)
}
