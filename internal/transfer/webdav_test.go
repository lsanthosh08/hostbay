package transfer_test

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/webdav"

	"hostbay/internal/apperr"
	"hostbay/internal/sshx"
	"hostbay/internal/sshx/proxytest"
	"hostbay/internal/tlstrust"
	"hostbay/internal/tlstrust/tlstest"
	"hostbay/internal/transfer"
	"hostbay/internal/transfer/sourcetest"
)

// davServer serves a temp folder over WebDAV at /dav/ to user u, password
// pw, counting the requests that carried credentials.
func davServer(t *testing.T, cert *tls.Certificate) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	h := &webdav.Handler{Prefix: "/dav", FileSystem: webdav.Dir(t.TempDir()), LockSystem: webdav.NewMemLS()}
	var authed atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pw, ok := r.BasicAuth()
		if ok {
			authed.Add(1)
		}
		if user != "u" || pw != "pw" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	if cert != nil {
		srv.TLS = &tls.Config{Certificates: []tls.Certificate{*cert}}
		srv.StartTLS()
	} else {
		srv.Start()
	}
	t.Cleanup(srv.Close)
	return srv, &authed
}

func TestPlainWebDAVPassesTheSourceSuite(t *testing.T) {
	srv, _ := davServer(t, nil)
	src, err := transfer.DialWebDAV(context.Background(), transfer.WebDAVConfig{URL: srv.URL + "/dav/", User: "u", Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	sourcetest.Run(t, src, "/")
}

func TestWebDAVOverHTTPSPinsTheCertificateBeforeSendingThePassword(t *testing.T) {
	cert := tlstest.Cert(t)
	srv, authed := davServer(t, &cert)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	p, _ := strconv.Atoi(port)
	certs := tlstrust.New(t.TempDir())
	connect := func() (*transfer.WebDAV, error) {
		var src *transfer.WebDAV
		_, err := certs.Connect("webdav", "h", "127.0.0.1", p, func(cfg *tls.Config) (err error) {
			src, err = transfer.DialWebDAV(context.Background(), transfer.WebDAVConfig{URL: srv.URL + "/dav", User: "u", Password: "pw", TLS: cfg})
			return err
		})
		return src, err
	}

	_, err := connect()
	ae, ok := err.(*apperr.Error)
	if !ok || ae.Code != apperr.HostKeyUnknown || ae.Details["kind"] != "webdavCert" {
		t.Fatalf("err = %v", err)
	}
	if authed.Load() != 0 {
		t.Fatal("the password reached a server whose certificate was not trusted")
	}
	if err := certs.Answer(ae.Details["requestId"], true); err != nil {
		t.Fatal(err)
	}
	src, err := connect()
	if err != nil {
		t.Fatal(err)
	}
	sourcetest.Run(t, src, "/")
}

func TestWebDAVRefusesARedirectToAnotherServer(t *testing.T) {
	var reached atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Add(1) }))
	defer other.Close()
	srv := httptest.NewServer(http.RedirectHandler(other.URL+"/dav/", http.StatusTemporaryRedirect))
	defer srv.Close()
	if _, err := transfer.DialWebDAV(context.Background(), transfer.WebDAVConfig{URL: srv.URL + "/dav/", User: "u", Password: "pw"}); err == nil {
		t.Fatal("followed a redirect to another server")
	}
	if reached.Load() != 0 {
		t.Fatal("the password was sent to the server redirected to")
	}
}

func TestHTTPSURLWithoutTLSIsRefused(t *testing.T) {
	if _, err := transfer.DialWebDAV(context.Background(), transfer.WebDAVConfig{URL: "https://dav.example/"}); err == nil {
		t.Fatal("an https URL was dialled without the pin store's TLS")
	}
}

// WebDAV over https reaches the server through an HTTP CONNECT proxy, and
// a proxy that is down fails without a direct request (KTD22).
func TestWebDAVThroughAProxy(t *testing.T) {
	cert := tlstest.Cert(t)
	srv, authed := davServer(t, &cert)
	certs := tlstrust.New(t.TempDir())
	p := proxytest.HTTP(t, "pu", "pp", 0)
	dial := dialVia(&sshx.Proxy{Type: "http", Host: p.Host, Port: p.Port, User: "pu", Password: "pp"})
	var src *transfer.WebDAV
	try := func(cfg *tls.Config) (err error) {
		src, err = transfer.DialWebDAV(context.Background(), transfer.WebDAVConfig{URL: srv.URL + "/dav/", User: "u", Password: "pw", TLS: cfg, Dial: dial})
		return err
	}
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	n, _ := strconv.Atoi(port)
	_, err := certs.Connect("webdav", "h1", "127.0.0.1", n, try)
	if ae, ok := err.(*apperr.Error); ok && ae.Code == apperr.HostKeyUnknown {
		certs.Answer(ae.Details["requestId"], true)
		_, err = certs.Connect("webdav", "h1", "127.0.0.1", n, try)
	}
	if err != nil {
		t.Fatal(err)
	}
	sourcetest.Run(t, src, "/")
	if len(p.Targets()) == 0 {
		t.Fatal("the proxy carried nothing")
	}

	before := authed.Load()
	host, dport := proxytest.Down(t)
	_, err = transfer.DialWebDAV(context.Background(), transfer.WebDAVConfig{URL: srv.URL + "/dav/", User: "u", Password: "pw",
		TLS: &tls.Config{}, Dial: dialVia(&sshx.Proxy{Type: "http", Host: host, Port: dport})})
	if apperr.CodeOf(err) != apperr.ProxyFailed || authed.Load() != before {
		t.Fatalf("err = %v; the server saw %d more signed requests", err, authed.Load()-before)
	}
}

// A slow server finishes a cancelled upload late: it refuses the first
// DELETEs and writes the cut-off .part file a while after the client gave
// up. Abort still leaves no .part behind.
func TestWebDAVAbortRemovesALatePartFile(t *testing.T) {
	dir := t.TempDir()
	h := &webdav.Handler{FileSystem: webdav.Dir(dir), LockSystem: webdav.NewMemLS()}
	var refused atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, ".part"):
			if _, err := io.Copy(io.Discard, r.Body); err != nil {
				name := filepath.Join(dir, filepath.Base(r.URL.Path))
				time.AfterFunc(1200*time.Millisecond, func() { os.WriteFile(name, []byte("partial"), 0o600) })
			}
			return
		case r.Method == http.MethodDelete && refused.Add(1) <= 3:
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	src, err := transfer.DialWebDAV(context.Background(), transfer.WebDAVConfig{URL: srv.URL + "/"})
	if err != nil {
		t.Fatal(err)
	}
	w, err := src.Create(context.Background(), "/", "big.bin", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(make([]byte, 64<<10))
	w.Abort()

	parts := func() []string {
		m, _ := filepath.Glob(filepath.Join(dir, ".hostbay-*.part"))
		return m
	}
	deadline := time.Now().Add(20 * time.Second)
	for len(parts()) == 0 && time.Now().Before(deadline) { // wait for the late write
		time.Sleep(50 * time.Millisecond)
	}
	for len(parts()) > 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if p := parts(); len(p) > 0 {
		t.Fatalf("left behind: %v", p)
	}
}
