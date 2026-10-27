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

// oraHost is an Oracle host at srv whose dials are recorded and go to srv.
type oraHost struct {
	cfg    ServerConfig
	mu     sync.Mutex
	dialed []string
}

func newOraHost(srv *dbtest.TNS, certs *tlstrust.Store, tlsMode string) *oraHost {
	h := &oraHost{}
	h.cfg = ServerConfig{Host: srv.Host, Port: srv.Port, User: "app", Password: "ora-Secret-3!", Database: "FREEPDB1", TLS: tlsMode,
		Trust: func(try func(*tls.Config) error) (*tls.Config, error) {
			return certs.Capture("oracle", "h1", srv.Host, srv.Port, try)
		},
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			h.mu.Lock()
			h.dialed = append(h.dialed, addr)
			h.mu.Unlock()
			if addr != srvAddr(srv) {
				return nil, errors.New("refused " + addr)
			}
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}}
	return h
}

func srvAddr(srv *dbtest.TNS) string { return net.JoinHostPort(srv.Host, strconv.Itoa(srv.Port)) }

func (h *oraHost) open() (Engine, error) { return OpenOracle(context.Background(), h.cfg) }

// Over TCPS a self-signed listener raises the prompt before any connect
// packet; once trusted, a redirect to the same address over plain TCP is
// refused, not followed out of TLS; a changed certificate is refused
// before any connect packet too (KTD25).
func TestOracleChecksTheCertificateBeforeConnecting(t *testing.T) {
	old, cur := tlstest.Cert(t), tlstest.Cert(t)
	srv := dbtest.NewTNS(t, &old)
	certs := tlstrust.New(t.TempDir())
	h := newOraHost(srv, certs, "verify")
	_, err := h.open()
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != apperr.HostKeyUnknown || ae.Details["kind"] != "oracleCert" || srv.Connects() != 0 {
		t.Fatalf("err = %v, connects %d", err, srv.Connects())
	}
	if err := certs.Answer(ae.Details["requestId"], true); err != nil {
		t.Fatal(err)
	}
	if _, err := h.open(); err == nil || !strings.Contains(err.Error(), "redirected") || srv.Connects() != 1 {
		t.Fatalf("trusted: err = %v, connects %d", err, srv.Connects())
	}
	srv.SetCert(&cur)
	if _, err := h.open(); !errors.As(err, &ae) || ae.Code != apperr.HostKeyChanged || srv.Connects() != 1 {
		t.Fatalf("changed: err = %v, connects %d", err, srv.Connects())
	}
}

// A listener redirect to another address is refused, naming it, and only
// the host's own address is dialled (KTD24).
func TestOracleRefusesARedirect(t *testing.T) {
	srv := dbtest.NewTNS(t, nil)
	srv.SetRedirect("(DESCRIPTION=(ADDRESS=(PROTOCOL=TCP)(HOST=10.9.9.9)(PORT=1521)))")
	h := newOraHost(srv, tlstrust.New(t.TempDir()), "none")
	_, err := h.open()
	if err == nil || !strings.Contains(err.Error(), "10.9.9.9:1521") || !strings.Contains(err.Error(), "listener address") {
		t.Fatalf("err = %v", err)
	}
	if len(h.dialed) != 1 || h.dialed[0] != srvAddr(srv) {
		t.Fatalf("dialed %v", h.dialed)
	}
}

// A service name is placed in the connect descriptor as it is, so one
// that could add to the descriptor is refused before anything is dialled.
func TestOracleServiceNameCannotAddToTheDescriptor(t *testing.T) {
	srv := dbtest.NewTNS(t, nil)
	h := newOraHost(srv, tlstrust.New(t.TempDir()), "none")
	h.cfg.Database = "x)(SERVER=DEDICATED"
	if _, err := h.open(); err == nil || len(h.dialed) != 0 {
		t.Fatalf("err = %v, dialed %v", err, h.dialed)
	}
	if got := oraQuote(`a"b`); got != `"a""b"` {
		t.Fatalf("quote = %s", got)
	}
}
