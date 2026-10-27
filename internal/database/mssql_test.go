package database

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	"hostbay/internal/apperr"
	"hostbay/internal/database/dbtest"
	"hostbay/internal/tlstrust"
	"hostbay/internal/tlstrust/tlstest"
)

// msHost is a SQL Server host named host at srv, whose dials are recorded
// and all go to srv.
type msHost struct {
	cfg    ServerConfig
	mu     sync.Mutex
	dialed []string
}

func newMSHost(srv *dbtest.TDS, certs *tlstrust.Store, host, tlsMode string) *msHost {
	h := &msHost{}
	h.cfg = ServerConfig{Host: host, Port: srv.Port, User: "app", Password: "ms-Secret-9!", Database: "app", TLS: tlsMode,
		Trust: func(try func(*tls.Config) error) (*tls.Config, error) {
			return certs.Capture("mssql", "h1", host, srv.Port, try)
		},
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			h.mu.Lock()
			h.dialed = append(h.dialed, addr)
			h.mu.Unlock()
			var d net.Dialer
			return d.DialContext(ctx, network, srv.Addr())
		}}
	return h
}

func (h *msHost) open() (Engine, error) { return OpenMSSQL(context.Background(), h.cfg) }

func certErr(t *testing.T, err error, code apperr.Code) *apperr.Error {
	t.Helper()
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != code || ae.Details["kind"] != "mssqlCert" {
		t.Fatalf("err = %v, want %s for mssqlCert", err, code)
	}
	return ae
}

// A self-signed TDS 8 server raises the prompt, and a changed pinned
// certificate is refused, both before any LOGIN7 (KTD25).
func TestMSSQLChecksTheCertificateBeforeLogin(t *testing.T) {
	old, cur := tlstest.Cert(t), tlstest.Cert(t)
	srv := dbtest.NewTDS(t, &old)
	certs := tlstrust.New(t.TempDir())
	h := newMSHost(srv, certs, "127.0.0.1", "verify")
	ae := certErr(t, func() error { _, err := h.open(); return err }(), apperr.HostKeyUnknown)
	if srv.Logins() != 0 {
		t.Fatal("LOGIN7 reached a server whose certificate was not trusted")
	}
	if err := certs.Answer(ae.Details["requestId"], true); err != nil {
		t.Fatal(err)
	}
	if _, err := h.open(); Category(err) != "auth" || srv.Logins() != 1 {
		t.Fatalf("trusted: err = %v, logins %d", err, srv.Logins())
	}
	srv.SetCert(&cur)
	_, err := h.open()
	certErr(t, err, apperr.HostKeyChanged)
	if srv.Logins() != 1 {
		t.Fatal("LOGIN7 reached a server whose certificate changed")
	}
}

// Under "verify" a server that offers no encryption is refused before
// LOGIN7, never signed in to unencrypted.
func TestMSSQLVerifyRefusesAServerWithoutTLS(t *testing.T) {
	srv := dbtest.NewTDS(t, nil)
	h := newMSHost(srv, tlstrust.New(t.TempDir()), "127.0.0.1", "verify")
	if _, err := h.open(); err == nil {
		t.Fatal("signed in without TLS under verify")
	}
	if srv.Conns() == 0 || srv.Logins() != 0 {
		t.Fatalf("connections %d, logins %d", srv.Conns(), srv.Logins())
	}
}

// The dial function gets the host's name, not an address resolved here,
// and a server that routes the client elsewhere is refused with the
// address and what to do (KTD24).
func TestMSSQLRefusesRouting(t *testing.T) {
	srv := dbtest.NewTDS(t, nil)
	srv.Route("10.9.9.9", 1433)
	h := newMSHost(srv, tlstrust.New(t.TempDir()), "localhost", "none")
	_, err := h.open()
	if err == nil || !strings.Contains(err.Error(), "10.9.9.9:1433") || !strings.Contains(err.Error(), "Proxy connection policy") {
		t.Fatalf("err = %v", err)
	}
	want := net.JoinHostPort("localhost", strconv.Itoa(srv.Port))
	if len(h.dialed) != 1 || h.dialed[0] != want || srv.Logins() != 1 {
		t.Fatalf("dialed %v, want only %s; logins %d", h.dialed, want, srv.Logins())
	}
}

// A Windows login would sign in as this computer's user or by NTLM; only
// SQL Server logins are offered.
func TestMSSQLOffersOnlySQLServerLogins(t *testing.T) {
	srv := dbtest.NewTDS(t, nil)
	h := newMSHost(srv, tlstrust.New(t.TempDir()), "127.0.0.1", "none")
	h.cfg.User = `CORP\app`
	if _, err := h.open(); err == nil || srv.Conns() != 0 {
		t.Fatalf("err = %v, connections %d", err, srv.Conns())
	}
	if got := mssqlQuote("a]b"); got != "[a]]b]" {
		t.Fatalf("quote = %s", got)
	}
}
