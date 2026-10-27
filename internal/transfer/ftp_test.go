package transfer_test

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	ftpserver "github.com/fclairamb/ftpserverlib"
	"github.com/spf13/afero"

	"hostbay/internal/apperr"
	"hostbay/internal/sshx"
	"hostbay/internal/sshx/proxytest"
	"hostbay/internal/tlstrust"
	"hostbay/internal/tlstrust/tlstest"
	"hostbay/internal/transfer"
	"hostbay/internal/transfer/sourcetest"
)

// ftpServer is an in-process FTP server over a temp folder. It counts USER
// commands, so a test can tell whether a sign-in was even attempted.
type ftpServer struct {
	dir   string
	mode  ftpserver.TLSRequirement
	cert  *tls.Certificate // nil: the server refuses AUTH TLS
	users atomic.Int32
	conns atomic.Int32
	addr  string
}

func newFTPServer(t *testing.T, mode ftpserver.TLSRequirement, cert *tls.Certificate) *ftpServer {
	t.Helper()
	s := &ftpServer{dir: t.TempDir(), mode: mode, cert: cert}
	srv := ftpserver.NewFtpServer(s)
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	t.Cleanup(func() { srv.Stop() })
	s.addr = srv.Addr()
	return s
}

func (s *ftpServer) GetSettings() (*ftpserver.Settings, error) {
	return &ftpserver.Settings{ListenAddr: "127.0.0.1:0", TLSRequired: s.mode}, nil
}
func (s *ftpServer) ClientConnected(ftpserver.ClientContext) (string, error) {
	s.conns.Add(1)
	return "test", nil
}
func (s *ftpServer) ClientDisconnected(ftpserver.ClientContext) {}
func (s *ftpServer) PreAuthUser(ftpserver.ClientContext, string) error {
	s.users.Add(1)
	return nil
}
func (s *ftpServer) AuthUser(_ ftpserver.ClientContext, user, pass string) (ftpserver.ClientDriver, error) {
	if user != "u" || pass != "pw" {
		return nil, errors.New("bad login")
	}
	return afero.NewBasePathFs(afero.NewOsFs(), s.dir), nil
}
func (s *ftpServer) GetTLSConfig() (*tls.Config, error) {
	if s.cert == nil {
		return nil, errors.New("no TLS here")
	}
	return &tls.Config{Certificates: []tls.Certificate{*s.cert}}, nil
}

func TestPlainFTPPassesTheSourceSuite(t *testing.T) {
	s := newFTPServer(t, ftpserver.ClearOrEncrypted, nil)
	src, err := transfer.DialFTP(context.Background(), transfer.FTPConfig{Addr: s.addr, User: "u", Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { src.Close() })
	sourcetest.Run(t, src, "/")
}

// An overwrite that is aborted part-way leaves the original file as it
// was: the upload goes to a temporary name until Commit.
func TestFTPAbortedOverwriteKeepsTheOriginal(t *testing.T) {
	s := newFTPServer(t, ftpserver.ClearOrEncrypted, nil)
	src, err := transfer.DialFTP(context.Background(), transfer.FTPConfig{Addr: s.addr, User: "u", Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { src.Close() })
	writeFile(t, s.dir+"/a.txt", "original", 0o644)
	w, err := src.Create(context.Background(), "/", "a.txt", 7)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, "partial")
	if err := w.Abort(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, s.dir+"/a.txt"); got != "original" {
		t.Fatalf("after an aborted overwrite a.txt = %q", got)
	}
	if got := dirNames(t, s.dir); len(got) != 1 {
		t.Fatalf("left beside it: %v", got)
	}
}

// connectFTPS connects through the pin store, as TransferService does. The
// pin is keyed to port 990 whatever port the test server listens on.
func connectFTPS(certs *tlstrust.Store, s *ftpServer, implicit bool) (*transfer.FTP, error) {
	var src *transfer.FTP
	_, err := certs.Connect("ftp", "host1", "127.0.0.1", 990, func(cfg *tls.Config) (err error) {
		src, err = transfer.DialFTP(context.Background(), transfer.FTPConfig{Addr: s.addr, User: "u", Password: "pw", TLS: cfg, Implicit: implicit})
		return err
	})
	return src, err
}

func TestFTPSAsksForASelfSignedCertificateAndSendsNoPasswordUntilPinned(t *testing.T) {
	for _, implicit := range []bool{false, true} {
		mode := ftpserver.MandatoryEncryption
		if implicit {
			mode = ftpserver.ImplicitEncryption
		}
		cert := tlstest.Cert(t)
		s := newFTPServer(t, mode, &cert)
		certs := tlstrust.New(t.TempDir())

		_, err := connectFTPS(certs, s, implicit)
		ae, ok := err.(*apperr.Error)
		if !ok || ae.Code != apperr.HostKeyUnknown || ae.Details["kind"] != "ftpCert" {
			t.Fatalf("implicit=%v: err = %v", implicit, err)
		}
		if err := certs.Answer(ae.Details["requestId"], false); err != nil {
			t.Fatal(err)
		}
		if _, err := connectFTPS(certs, s, implicit); apperr.CodeOf(err) != apperr.HostKeyUnknown {
			t.Fatalf("after declining: %v", err)
		}
		if n := s.users.Load(); n != 0 {
			t.Fatalf("implicit=%v: %d sign-ins reached a server whose certificate was not trusted", implicit, n)
		}

		_, err = connectFTPS(certs, s, implicit)
		if err := certs.Answer(err.(*apperr.Error).Details["requestId"], true); err != nil {
			t.Fatal(err)
		}
		src, err := connectFTPS(certs, s, implicit)
		if err != nil {
			t.Fatalf("implicit=%v: pinned: %v", implicit, err)
		}
		sourcetest.Run(t, src, "/")
		src.Close()
	}
}

func TestFTPSRefusesAChangedCertificateWithBothFingerprints(t *testing.T) {
	old, cur := tlstest.Cert(t), tlstest.Cert(t)
	certs := tlstrust.New(t.TempDir())
	_, err := connectFTPS(certs, newFTPServer(t, ftpserver.MandatoryEncryption, &old), false)
	if err := certs.Answer(err.(*apperr.Error).Details["requestId"], true); err != nil {
		t.Fatal(err)
	}
	s := newFTPServer(t, ftpserver.MandatoryEncryption, &cur)
	_, err = connectFTPS(certs, s, false)
	ae, ok := err.(*apperr.Error)
	if !ok || ae.Code != apperr.HostKeyChanged || ae.Details["expected"] != tlstrust.Fingerprint(old.Certificate[0]) ||
		ae.Details["fingerprint"] != tlstrust.Fingerprint(cur.Certificate[0]) {
		t.Fatalf("err = %v", err)
	}
	if s.users.Load() != 0 {
		t.Fatal("signed in to a server whose certificate changed")
	}
}

func TestFTPSNeverFallsBackToPlainFTP(t *testing.T) {
	// The server speaks plain FTP and refuses AUTH TLS.
	s := newFTPServer(t, ftpserver.ClearOrEncrypted, nil)
	_, err := connectFTPS(tlstrust.New(t.TempDir()), s, false)
	if err == nil {
		t.Fatal("FTPS connected to a server without TLS")
	}
	if n := s.users.Load(); n != 0 {
		t.Fatalf("retried in plain FTP: %d sign-ins reached the server", n)
	}
}

func dialVia(p *sshx.Proxy) transfer.DialFunc {
	return func(ctx context.Context, _, addr string) (net.Conn, error) { return sshx.DialTCP(ctx, p, addr) }
}

// Through a proxy, FTP's control and data connections, FTPS's TLS on both
// included, all go via the proxy, which is handed the server's name
// (KTD22).
func TestFTPThroughAProxy(t *testing.T) {
	for _, mode := range []string{"none", "explicit", "implicit"} {
		t.Run(mode, func(t *testing.T) {
			req := map[string]ftpserver.TLSRequirement{"none": ftpserver.ClearOrEncrypted, "explicit": ftpserver.MandatoryEncryption, "implicit": ftpserver.ImplicitEncryption}[mode]
			cert := tlstest.Cert(t)
			s := newFTPServer(t, req, &cert)
			_, port, _ := net.SplitHostPort(s.addr)
			p := proxytest.SOCKS5(t, "", "")
			cfg := transfer.FTPConfig{Addr: "localhost:" + port, User: "u", Password: "pw", Implicit: mode == "implicit",
				Dial: dialVia(&sshx.Proxy{Type: "socks5", Host: p.Host, Port: p.Port})}
			var src *transfer.FTP
			var err error
			if mode == "none" {
				src, err = transfer.DialFTP(context.Background(), cfg)
			} else {
				certs := tlstrust.New(t.TempDir())
				try := func(tc *tls.Config) (err error) {
					cfg.TLS = tc
					src, err = transfer.DialFTP(context.Background(), cfg)
					return err
				}
				_, err = certs.Connect("ftp", "host1", "localhost", 990, try)
				if ae, ok := err.(*apperr.Error); ok && ae.Code == apperr.HostKeyUnknown {
					certs.Answer(ae.Details["requestId"], true)
					_, err = certs.Connect("ftp", "host1", "localhost", 990, try)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { src.Close() })
			sourcetest.Run(t, src, "/")
			targets := p.Targets()
			if len(targets) < 2 {
				t.Fatalf("proxy carried %v; the data connections went around it", targets)
			}
			for _, tg := range targets {
				if !strings.HasPrefix(tg, "localhost:") {
					t.Fatalf("proxy was asked for %s, not the server's name", tg)
				}
			}
		})
	}
}

// An FTP host whose proxy is down fails at the proxy step, and the FTP
// server sees no connection.
func TestFTPProxyDownNeverDialsTheServer(t *testing.T) {
	s := newFTPServer(t, ftpserver.ClearOrEncrypted, nil)
	host, port := proxytest.Down(t)
	_, err := transfer.DialFTP(context.Background(), transfer.FTPConfig{Addr: s.addr, User: "u", Password: "pw",
		Dial: dialVia(&sshx.Proxy{Type: "socks5", Host: host, Port: port})})
	if ae, ok := errors.AsType[*apperr.Error](err); !ok || ae.Code != apperr.ProxyFailed || ae.Details["step"] != "proxy" {
		t.Fatalf("err = %v, want ProxyFailed", err)
	}
	if n := s.conns.Load(); n != 0 {
		t.Fatalf("the FTP server saw %d connections", n)
	}
}
