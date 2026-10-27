package ai

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"hostbay/internal/apperr"
	"hostbay/internal/sshx"
	"hostbay/internal/tlstrust"
	"hostbay/internal/tlstrust/tlstest"
)

func direct(ctx context.Context, addr string) (net.Conn, error) {
	return sshx.DialTCP(ctx, nil, addr)
}

// counting serves 200 and counts what reached it.
func counting(t *testing.T) (*httptest.Server, *atomic.Int32) {
	var n atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { n.Add(1) }))
	t.Cleanup(s.Close)
	return s, &n
}

// A redirect is refused, because Go would copy x-api-key to its target.
func TestClientRefusesRedirects(t *testing.T) {
	target, hits := counting(t)
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v1/messages", http.StatusTemporaryRedirect)
	}))
	defer src.Close()
	c, err := HTTPClient(context.Background(), Endpoint{ID: "p1", BaseURL: src.URL}, direct, tlstrust.New(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", src.URL+"/v1/messages", strings.NewReader("{}"))
	req.Header.Set("x-api-key", "sk-sample")
	if _, err := c.Do(req); err == nil {
		t.Fatal("the redirect was followed")
	}
	if hits.Load() != 0 {
		t.Fatal("the redirect target saw a request")
	}
}

// The environment's proxy is never used.
func TestClientIgnoresTheEnvironmentProxy(t *testing.T) {
	proxy, proxied := counting(t)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("ALL_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")
	target, hits := counting(t)
	c, err := HTTPClient(context.Background(), Endpoint{ID: "p1", BaseURL: target.URL}, direct, tlstrust.New(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(target.URL + "/v1/models"); err != nil {
		t.Fatal(err)
	}
	if proxied.Load() != 0 || hits.Load() != 1 {
		t.Fatalf("proxy saw %d, target %d", proxied.Load(), hits.Load())
	}
}

// When the provider's saved proxy refuses, the request fails; nothing is
// dialled directly instead.
func TestClientFailsWhenItsProxyRefuses(t *testing.T) {
	target, hits := counting(t)
	dead, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := dead.Addr().(*net.TCPAddr)
	dead.Close()
	viaProxy := func(ctx context.Context, a string) (net.Conn, error) {
		return sshx.DialTCP(ctx, &sshx.Proxy{Type: "socks5", Host: "127.0.0.1", Port: addr.Port}, a)
	}
	c, err := HTTPClient(context.Background(), Endpoint{ID: "p1", BaseURL: target.URL}, viaProxy, tlstrust.New(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(target.URL + "/v1/models"); err == nil {
		t.Fatal("the request succeeded without its proxy")
	}
	if hits.Load() != 0 {
		t.Fatal("the target was reached directly")
	}
}

// http:// is for this computer, or an explicit choice marked unencrypted.
func TestClientRefusesPlainHTTPElsewhere(t *testing.T) {
	_, err := HTTPClient(context.Background(), Endpoint{ID: "p1", BaseURL: "http://gateway.example:8080/v1"}, direct, tlstrust.New(t.TempDir()))
	if err == nil {
		t.Fatal("plain http to another host was allowed")
	}
	if _, err := HTTPClient(context.Background(), Endpoint{ID: "p1", BaseURL: "http://gateway.example:8080/v1", Unencrypted: true}, direct, tlstrust.New(t.TempDir())); err != nil {
		t.Fatalf("the explicit unencrypted choice: %v", err)
	}
}

// A self-signed gateway asks to be pinned; once accepted it is trusted, and
// a changed certificate is refused.
func TestClientPinsASelfSignedGateway(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{tlstest.Cert(t)}}
	srv.StartTLS()
	defer srv.Close()
	pins := tlstrust.New(t.TempDir())
	ep := Endpoint{ID: "p1", BaseURL: srv.URL}
	_, err := HTTPClient(context.Background(), ep, direct, pins)
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != apperr.HostKeyUnknown || ae.Details["kind"] != "aiCert" {
		t.Fatalf("first use = %v", err)
	}
	if err := pins.Answer(ae.Details["requestId"], true); err != nil {
		t.Fatal(err)
	}
	c, err := HTTPClient(context.Background(), ep, direct, pins)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(srv.URL + "/v1/models"); err != nil {
		t.Fatal(err)
	}

	// Same address, another certificate.
	srv.Close()
	ln, err := net.Listen("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Skip("port reuse unavailable:", err)
	}
	other := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	other.Listener = ln
	other.TLS = &tls.Config{Certificates: []tls.Certificate{tlstest.Cert(t)}}
	other.StartTLS()
	defer other.Close()
	if _, err := HTTPClient(context.Background(), ep, direct, pins); apperr.CodeOf(err) != apperr.HostKeyChanged {
		t.Fatalf("changed certificate = %v", err)
	}
}
