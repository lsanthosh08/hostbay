package remotedesktop

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"hostbay/internal/apperr"
	"hostbay/internal/remotedesktop/rfbtest"
	"hostbay/internal/remotedesktop/spicetest"
	"hostbay/internal/tlstrust/tlstest"
)

func spiceSignIn(t *testing.T, srv *spicetest.Server, link []byte, pw string, cfg *tls.Config) (net.Conn, []byte, error) {
	t.Helper()
	c, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	rc, reply, err := SpiceSignIn(c, link, pw, cfg)
	if err != nil {
		c.Close()
		return nil, nil, err
	}
	t.Cleanup(func() { rc.Close() })
	return rc, reply, nil
}

// The main channel's link message is the one spice-html5 0.3.0's send_hdr
// builds: version 2.2, auth selection and mini header common caps, and the
// agent-tokens main cap. The page's main stream must match it exactly.
func TestSpiceMainLinkMatchesSpiceHTML5(t *testing.T) {
	want := []byte{'R', 'E', 'D', 'Q', 2, 0, 0, 0, 2, 0, 0, 0, 26, 0, 0, 0,
		0, 0, 0, 0, 1, 0, 1, 0, 0, 0, 1, 0, 0, 0, 18, 0, 0, 0, 9, 0, 0, 0, 4, 0, 0, 0}
	if !bytes.Equal(SpiceMainLink, want) {
		t.Fatalf("main link % x, want % x", SpiceMainLink, want)
	}
}

// Go seals the ticket under each channel's own link key, so the server
// decrypts it to the password; a wrong one asks for the password.
func TestSpiceSignInSealsTheTicket(t *testing.T) {
	srv := spicetest.New(t, "Spice-Secret-3")
	if _, _, err := spiceSignIn(t, srv, SpiceMainLink, "Spice-Secret-3", nil); err != nil {
		t.Fatal(err)
	}
	display := spiceLink(2, 0, 1<<spiceCapAuthSelect, 0)
	if _, _, err := spiceSignIn(t, srv, display, "Spice-Secret-3", nil); err != nil {
		t.Fatal(err)
	}
	// Without auth selection the ticket follows the reply directly.
	if _, _, err := spiceSignIn(t, srv, spiceLink(3, 0, 0, 0), "Spice-Secret-3", nil); err != nil {
		t.Fatal(err)
	}
	if got := srv.Log(); !slices.Equal(got[:6], []string{"link 1:0", "ticket ok", "link 2:0", "ticket ok", "link 3:0", "ticket ok"}) {
		t.Fatalf("server log %v", got)
	}

	_, _, err := spiceSignIn(t, srv, SpiceMainLink, "wrong", nil)
	if apperr.CodeOf(err) != apperr.SecretNeeded {
		t.Fatalf("err = %v, want SecretNeeded", err)
	}
	if _, _, err := spiceSignIn(t, srv, SpiceMainLink, strings.Repeat("x", 61), nil); err == nil {
		t.Fatal("a password longer than SPICE allows was sent")
	}
}

// A server that refuses the link is named, and no ticket is sent.
func TestSpiceLinkErrorIsNamed(t *testing.T) {
	srv := spicetest.New(t, "pw")
	srv.Set(func(c *spicetest.Config) { c.LinkError = 5 })
	_, _, err := spiceSignIn(t, srv, SpiceMainLink, "pw", nil)
	if err == nil || !strings.Contains(err.Error(), "needs TLS") {
		t.Fatalf("err = %v, want the link error named", err)
	}
}

// A TLS host speaks TLS from the first byte and checks the certificate
// against its config; a plain port, or an unverified certificate, stops
// before any ticket is sent (KTD18).
func TestSpiceSignInOverTLS(t *testing.T) {
	cert := tlstest.Cert(t)
	srv := spicetest.New(t, "pw")
	srv.Set(func(c *spicetest.Config) { c.Cert = cert })
	roots := x509.NewCertPool()
	roots.AddCert(cert.Leaf)
	cfg := &tls.Config{ServerName: "127.0.0.1", RootCAs: roots}
	if _, _, err := spiceSignIn(t, srv, SpiceMainLink, "pw", cfg); err != nil {
		t.Fatal(err)
	}

	_, _, err := spiceSignIn(t, srv, SpiceMainLink, "pw", &tls.Config{ServerName: "127.0.0.1"})
	var ve *tls.CertificateVerificationError
	if !asCertErr(err, &ve) {
		t.Fatalf("err = %v, want a certificate verification error", err)
	}

	plain := spicetest.New(t, "pw")
	if _, _, err := spiceSignIn(t, plain, SpiceMainLink, "pw", cfg); err == nil {
		t.Fatal("a plain SPICE port was accepted for TLS")
	}
	if got := plain.Log(); slices.Contains(got, "ticket ok") || slices.Contains(got, "ticket wrong") {
		t.Fatalf("a ticket went to the plain port: %v", got)
	}
}

// The page links its channel as usual and seals an empty password; Go
// drops that ticket, the server gets Go's, and the page is told the link
// succeeded. No byte of the password reaches the page (KTD19).
func TestRelaySpiceKeepsThePasswordInGo(t *testing.T) {
	const pw = "Relay-Spice-9"
	srv := spicetest.New(t, pw)
	page := rfbtest.NewPage()
	done := make(chan error, 1)
	go func() {
		done <- RelaySpice(page, func(link []byte) (net.Conn, []byte, error) {
			c, err := net.Dial("tcp", srv.Addr())
			if err != nil {
				return nil, nil, err
			}
			return SpiceSignIn(c, link, "Relay-Spice-9", nil)
		})
	}()

	display := spiceLink(2, 0, 1<<spiceCapAuthSelect|1<<3, 0)
	page.Feed(display[:10]) // frames need not line up with messages
	page.Feed(display[10:])
	reply := page.Read(t, 0, 16)
	n := 16 + int(reply[12])
	if got := page.Read(t, 16, n); got[0] != 0 || len(got) < 4+spicePubKeyLen {
		t.Fatalf("link reply % x", got)
	}
	page.Feed(make([]byte, 4+spiceTicketLen)) // the page's empty-password ticket
	if got := page.Read(t, n, n+4); !bytes.Equal(got, []byte{0, 0, 0, 0}) {
		t.Fatalf("link result %v", got)
	}
	page.Feed([]byte("hello"))
	if got := page.Read(t, n+4, n+9); string(got) != "hello" {
		t.Fatalf("echo %q", got)
	}
	if bytes.Contains(page.Sent(), []byte(pw)) {
		t.Fatal("the password reached the page")
	}
	if got := srv.Log(); !slices.Equal(got[:2], []string{"link 2:0", "ticket ok"}) {
		t.Fatalf("server log %v", got)
	}

	page.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the relay outlived the page")
	}
}
