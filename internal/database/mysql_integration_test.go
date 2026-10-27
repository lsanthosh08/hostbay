//go:build integration

package database

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hostbay/internal/dockertest"
	"hostbay/internal/tlstrust"
)

// certDir writes a self-signed certificate for name, readable by the
// container's server, and returns its folder.
func certDir(t *testing.T, name string) string {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, _ := x509.MarshalPKCS8PrivateKey(key)
	dir := t.TempDir()
	os.Chmod(dir, 0o755)
	for file, b := range map[string][]byte{
		"cert.pem": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		"key.pem":  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}),
	} {
		if err := os.WriteFile(filepath.Join(dir, file), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// MySQL and MariaDB servers reachable only through a two-hop SSH chain
// ask to pin their self-signed certificates, then browse, query, page and
// edit through it; a cancelled SLEEP(60) ends on the server. A MySQL
// server's own generated certificate, which names no host, pins and
// connects too.
func TestMySQLThroughATwoHopChain(t *testing.T) {
	dockertest.Require(t)
	network, c := twoHopChain(t)
	for _, s := range []struct {
		test, image, env, cli string
		ownCert               bool
	}{
		{"mysql", "mysql:8.4", "MYSQL", "mysql", false},
		{"mariadb", "mariadb:11.4", "MARIADB", "mariadb", false},
		{"mysql-own-certificate", "mysql:8.4", "MYSQL", "mysql", true},
	} {
		t.Run(s.test, func(t *testing.T) {
			name := dockertest.Name("hostbay-my")
			const pw = "it-My-Pass-7!" // a throwaway container's password
			args := []string{"--network", network, "-e", s.env + "_ROOT_PASSWORD=" + pw, "-e", s.env + "_DATABASE=app"}
			if s.ownCert {
				args = append(args, s.image)
			} else {
				args = append(args, "-v", certDir(t, name)+":/certs:ro", s.image, "--ssl-cert=/certs/cert.pem", "--ssl-key=/certs/key.pem")
			}
			dockertest.Start(t, name, args...)
			sql := func(q string) string {
				out := dockertest.Docker(t, "exec", "-e", "MYSQL_PWD="+pw, name, s.cli, "-uroot", "-h127.0.0.1", "--protocol=tcp", "-N", "-e", q)
				return out[strings.LastIndex(out, "\n")+1:] // after any warning

			}
			// The first start runs a server without networking; TCP
			// answers once the real one is up.
			dockertest.WaitExec(t, name, 3*time.Minute, s.cli, "-uroot", "-p"+pw, "-h127.0.0.1", "--protocol=tcp", "-e", "SELECT 1")

			ctx := context.Background()
			certs := tlstrust.New(t.TempDir())
			d := &chainDial{c: c, want: net.JoinHostPort(name, "3306")}
			cfg := ServerConfig{Host: name, Port: 3306, User: "root", Password: pw, Database: "app", TLS: "verify",
				Dial: d.dial,
				Trust: func(try func(*tls.Config) error) (*tls.Config, error) {
					return certs.Capture("mysql", "h1", name, 3306, try)
				}}
			e := pinThenOpen(t, certs, func() (Engine, error) { return OpenMySQL(ctx, cfg) })
			defer e.Close()
			r := NewRegistry(func(State) {})
			if rows := all(t, r, e, "SHOW SESSION STATUS LIKE 'Ssl_cipher'"); len(rows) != 1 || rows[0][1].Text == "" {
				t.Fatalf("the session is not TLS: %+v", rows)
			}
			if s.ownCert {
				d.check(t)
				return
			}
			all(t, r, e, `CREATE TABLE n (i int PRIMARY KEY); CREATE TABLE d (x int);
				INSERT INTO d VALUES (0), (1), (2), (3), (4), (5), (6), (7), (8), (9);
				INSERT INTO n SELECT a.x*1000 + b.x*100 + c.x*10 + e.x + 1 FROM d a, d b, d c, d e WHERE a.x*1000 + b.x*100 + c.x*10 + e.x < 1200;
				CREATE TABLE q (id int PRIMARY KEY, `+"`a``b`"+` text); INSERT INTO q VALUES (1, 'x')`)
			browseQueryPageEdit(t, e, "app", "n", "i")
			d.check(t)
			editQuoted(t, e, []string{"app", "q"}, "id", "a`b", "SELECT `a``b` FROM q WHERE id = 1")

			kills := func() string { return sql("SHOW GLOBAL STATUS LIKE 'Com_kill'") }
			before := kills()
			sleep := r.Start(ctx, "tab", func(ctx context.Context) (Results, error) { return e.Query(ctx, "SELECT SLEEP(60)") })
			time.Sleep(time.Second)
			start := time.Now()
			r.Cancel(sleep)
			if _, err := r.Next(ctx, sleep); err == nil || time.Since(start) > 2*time.Second {
				t.Fatalf("cancel: %v after %v", err, time.Since(start))
			}
			running := "SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE INFO = 'SELECT SLEEP(60)'"
			for deadline := time.Now().Add(5 * time.Second); sql(running) != "0"; time.Sleep(200 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatal("the server still runs the cancelled SLEEP(60)")
				}
			}
			// The driver only drops its connection; Hostbay's KILL QUERY,
			// on another connection through the chain, is what stops it.
			for deadline := time.Now().Add(5 * time.Second); kills() == before; time.Sleep(200 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatal("no KILL QUERY reached the server")
				}
			}
			if rows := all(t, r, e, "SELECT 2"); rows[0][0].Text != "2" {
				t.Fatalf("after the cancel: %+v", rows)
			}
		})
	}
}
