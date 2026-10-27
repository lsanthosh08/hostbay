package database

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"hostbay/internal/apperr"
	"hostbay/internal/database/dbtest"
	"hostbay/internal/tlstrust"
	"hostbay/internal/tlstrust/tlstest"
)

const pgPassword = "pg-Secret-7!"

// pgHost is a PostgreSQL host at srv whose dials are recorded and go to
// to (srv itself when empty).
type pgHost struct {
	cfg    PGConfig
	mu     sync.Mutex
	dialed []string
}

func newPGHost(t *testing.T, srv *dbtest.PG, certs *tlstrust.Store, to string) *pgHost {
	h := &pgHost{}
	h.cfg = PGConfig{
		Host: srv.Host, Port: srv.Port, User: "app", Password: pgPassword, Database: "app", TLS: "verify",
		NoClientCert: filepath.Join(t.TempDir(), "never", "client.crt"),
		Trust: func(try func(*tls.Config) error) (*tls.Config, error) {
			return certs.Capture("postgres", "h1", srv.Host, srv.Port, try)
		},
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			h.mu.Lock()
			h.dialed = append(h.dialed, addr)
			h.mu.Unlock()
			if to == "" {
				to = addr
			}
			var d net.Dialer
			return d.DialContext(ctx, network, to)
		},
	}
	return h
}

func (h *pgHost) open() (Engine, error) { return OpenPostgres(context.Background(), h.cfg) }

// trustOnce answers the first connect's certificate prompt with accept.
func trustOnce(t *testing.T, h *pgHost, certs *tlstrust.Store, accept bool) {
	t.Helper()
	_, err := h.open()
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != apperr.HostKeyUnknown || ae.Details["kind"] != "postgresCert" {
		t.Fatalf("first connect: %v", err)
	}
	if err := certs.Answer(ae.Details["requestId"], accept); err != nil {
		t.Fatal(err)
	}
}

// A self-signed server raises the prompt; declining sends no startup
// message, so the password never left (KTD25).
func TestPostgresAsksForAnUnknownCertificateBeforeSigningIn(t *testing.T) {
	cert := tlstest.Cert(t)
	srv := dbtest.NewPG(t, &cert)
	certs := tlstrust.New(t.TempDir())
	h := newPGHost(t, srv, certs, "")
	trustOnce(t, h, certs, false)
	trustOnce(t, h, certs, false)
	if len(srv.Startups()) != 0 || len(srv.Passwords()) != 0 {
		t.Fatalf("startups %v, passwords %d before the certificate was trusted", srv.Startups(), len(srv.Passwords()))
	}
}

// A pinned certificate that changed is refused, naming both fingerprints,
// before any sign-in packet.
func TestPostgresRefusesAChangedCertificate(t *testing.T) {
	old, cur := tlstest.Cert(t), tlstest.Cert(t)
	srv := dbtest.NewPG(t, &old)
	certs := tlstrust.New(t.TempDir())
	trustOnce(t, newPGHost(t, srv, certs, ""), certs, true)

	renewed := dbtest.NewPG(t, &cur) // the same host and port, now with cur
	_, err := newPGHost(t, srv, certs, renewed.Addr()).open()
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != apperr.HostKeyChanged || ae.Details["expected"] != tlstrust.Fingerprint(old.Certificate[0]) || ae.Details["fingerprint"] != tlstrust.Fingerprint(cur.Certificate[0]) {
		t.Fatalf("err = %v", err)
	}
	if len(renewed.Startups()) != 0 {
		t.Fatal("a startup message reached a server whose certificate changed")
	}
}

// PG* variables, a service file and HTTPS_PROXY change nothing: TLS stays
// on and every dial goes to the configured host (KTD26).
func TestPostgresIgnoresTheEnvironment(t *testing.T) {
	cert := tlstest.Cert(t)
	srv := dbtest.NewPG(t, &cert)
	certs := tlstrust.New(t.TempDir())
	h := newPGHost(t, srv, certs, "")
	trustOnce(t, h, certs, true)
	for k, v := range map[string]string{
		"PGSSLMODE": "disable", "PGHOST": "elsewhere.invalid", "PGPORT": "1", "PGDATABASE": "other",
		"PGUSER": "other", "PGPASSWORD": "other", "PGSERVICE": "x", "HTTPS_PROXY": "http://127.0.0.1:1",
	} {
		t.Setenv(k, v)
	}
	before := srv.SSLRequests()
	if _, err := h.open(); Category(err) != "auth" {
		t.Fatalf("err = %v, want the fake server's sign-in refusal", err)
	}
	if srv.SSLRequests() == before {
		t.Fatal("the connect did not ask for TLS")
	}
	if s := srv.Startups(); len(s) != 1 || s[0]["database"] != "app" || s[0]["user"] != "app" {
		t.Fatalf("startups = %v", s)
	}
	if p := srv.Passwords(); len(p) != 1 || p[0] != pgPassword {
		t.Fatalf("passwords = %q", p)
	}
	for _, a := range h.dialed {
		if a != srv.Addr() {
			t.Fatalf("dialled %s, not the configured %s", a, srv.Addr())
		}
	}
}

// A database name that looks like options is one literal name: the server
// sees it whole, and TLS stays on.
func TestPostgresDatabaseNameIsLiteral(t *testing.T) {
	cert := tlstest.Cert(t)
	srv := dbtest.NewPG(t, &cert)
	certs := tlstrust.New(t.TempDir())
	h := newPGHost(t, srv, certs, "")
	h.cfg.Database = "x sslmode=disable"
	trustOnce(t, h, certs, true)
	srv.Fail("3D000", `database "x sslmode=disable" does not exist`)
	_, err := h.open()
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("err = %v", err)
	}
	if s := srv.Startups(); len(s) != 1 || s[0]["database"] != "x sslmode=disable" || srv.SSLRequests() != srv.Conns() {
		t.Fatalf("startups = %v, TLS requests %d of %d connections", s, srv.SSLRequests(), srv.Conns())
	}
}

// writeCertFiles writes a client certificate, its key and a root
// certificate where lib/pq looks by default.
func writeCertFiles(t *testing.T, dir string) {
	t.Helper()
	c := tlstest.Cert(t)
	key, err := x509.MarshalPKCS8PrivateKey(c.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	crt := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Certificate[0]})
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string][]byte{
		"postgresql.crt": crt,
		"postgresql.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}),
		"root.crt":       crt,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// Certificate files in the home folder are never read: the server gets
// no client certificate, and the root certificate there trusts nothing.
func TestPostgresReadsNoCertificateFromTheHomeFolder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("APPDATA", home)
	writeCertFiles(t, filepath.Join(home, ".postgresql"))
	writeCertFiles(t, filepath.Join(home, "postgresql"))
	cert := tlstest.Cert(t)
	srv := dbtest.NewPG(t, &cert)
	certs := tlstrust.New(t.TempDir())
	h := newPGHost(t, srv, certs, "")
	trustOnce(t, h, certs, true)
	if _, err := h.open(); Category(err) != "auth" {
		t.Fatalf("err = %v", err)
	}
	if srv.ClientCerts() != 0 {
		t.Fatal("a client certificate from the home folder was sent")
	}
}

// With PQGO_DEBUG set, lib/pq would trace the password to stderr: the
// connect is refused and nothing is dialled or written.
func TestPostgresRefusesToConnectWhilePQGODebugIsSet(t *testing.T) {
	cert := tlstest.Cert(t)
	srv := dbtest.NewPG(t, &cert)
	h := newPGHost(t, srv, tlstrust.New(t.TempDir()), "")
	t.Setenv("PQGO_DEBUG", "1")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := os.Stderr
	os.Stderr = w
	_, err = h.open()
	os.Stderr = stderr
	w.Close()
	out, _ := io.ReadAll(r)
	if err == nil || !strings.Contains(err.Error(), "PQGO_DEBUG") {
		t.Fatalf("err = %v", err)
	}
	if srv.Conns() != 0 || len(out) != 0 {
		t.Fatalf("connections %d, stderr %q", srv.Conns(), out)
	}
}

// A server error that echoes the password reaches the page without it.
func TestPostgresErrorEchoingThePasswordIsScrubbed(t *testing.T) {
	cert := tlstest.Cert(t)
	srv := dbtest.NewPG(t, &cert)
	certs := tlstrust.New(t.TempDir())
	h := newPGHost(t, srv, certs, "")
	trustOnce(t, h, certs, true)
	srv.Fail("28P01", "password "+pgPassword+" is wrong")
	_, err := h.open()
	ae, ok := AppError(err, pgPassword).(*apperr.Error)
	if !ok || strings.Contains(ae.Message, pgPassword) || strings.Contains(ae.Details["detail"], pgPassword) || ae.Details["category"] != "auth" {
		t.Fatalf("err = %+v", ae)
	}
	if !strings.Contains(ae.Message, "is wrong") {
		t.Fatalf("the engine's message was lost: %q", ae.Message)
	}
}
