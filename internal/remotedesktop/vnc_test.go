package remotedesktop

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"hostbay/internal/apperr"
	"hostbay/internal/remotedesktop/rfbtest"
	"hostbay/internal/tlstrust/tlstest"
)

func signIn(t *testing.T, srv *rfbtest.Server, a VNCAuth) (net.Conn, error) {
	t.Helper()
	c, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	rc, err := SignIn(c, a)
	if err != nil {
		c.Close()
		return nil, err
	}
	t.Cleanup(func() { rc.Close() })
	return rc, nil
}

func TestSignInWithVNCPassword(t *testing.T) {
	srv := rfbtest.New(t, "Vnc-Secret-7", 2)
	if _, err := signIn(t, srv, VNCAuth{Security: "none", Password: "Vnc-Secret-7"}); err != nil {
		t.Fatal(err)
	}
	if got := srv.Log(); !slices.Equal(got[:2], []string{"type 2", "auth ok"}) {
		t.Fatalf("server log %v", got)
	}

	// A wrong password asks for the password again; nothing retries.
	_, err := signIn(t, srv, VNCAuth{Security: "none", Password: "wrong"})
	if apperr.CodeOf(err) != apperr.SecretNeeded || !strings.Contains(err.Error(), "wrong password") {
		t.Fatalf("err = %v, want SecretNeeded with the server's reason", err)
	}
	if n := srv.Conns(); n != 2 {
		t.Fatalf("the server saw %d connections, want 2", n)
	}
}

// macOS Screen Sharing speaks RFB 3.889 and offers Apple Remote Desktop
// first; a host with a user name signs in that way.
func TestSignInWithAppleRemoteDesktop(t *testing.T) {
	srv := rfbtest.New(t, "Mac-Secret-1", 30, 2)
	srv.Set(func(c *rfbtest.Config) { c.Version, c.User = "RFB 003.889\n", "me" })
	if _, err := signIn(t, srv, VNCAuth{Security: "none", User: "me", Password: "Mac-Secret-1"}); err != nil {
		t.Fatal(err)
	}
	if got := srv.Log(); !slices.Equal(got[:2], []string{"type 30", "auth ok"}) {
		t.Fatalf("server log %v", got)
	}
	if _, err := signIn(t, srv, VNCAuth{Security: "none", User: "me", Password: "nope"}); apperr.CodeOf(err) != apperr.SecretNeeded {
		t.Fatalf("err = %v, want SecretNeeded", err)
	}
}

// A host set to VeNCrypt never answers a server that does not offer it,
// so not even a challenge response leaves (KTD18).
func TestVeNCryptHostRefusesAServerWithoutIt(t *testing.T) {
	srv := rfbtest.New(t, "pw", 2, 1)
	_, err := signIn(t, srv, VNCAuth{Security: "vencrypt", Password: "pw"})
	if err == nil || !strings.Contains(err.Error(), "VNC password, None") {
		t.Fatalf("err = %v, want a refusal naming the offered types", err)
	}
	if got := srv.Log(); len(got) != 0 {
		t.Fatalf("the client answered the server: %v", got)
	}
}

// VeNCrypt with an X509 subtype signs in inside TLS, verifying the
// server's certificate against the config it is given.
func TestSignInWithVeNCrypt(t *testing.T) {
	cert := tlstest.Cert(t)
	srv := rfbtest.New(t, "pw", 19)
	srv.Set(func(c *rfbtest.Config) { c.SubTypes, c.Cert = []uint32{258, 261}, cert })
	roots := x509.NewCertPool()
	roots.AddCert(cert.Leaf)
	cfg := &tls.Config{ServerName: "127.0.0.1", RootCAs: roots}
	if _, err := signIn(t, srv, VNCAuth{Security: "vencrypt", Password: "pw", TLS: cfg}); err != nil {
		t.Fatal(err)
	}
	if got := srv.Log(); !slices.Equal(got[:3], []string{"type 19", "subtype 261", "auth ok"}) {
		t.Fatalf("server log %v", got)
	}

	// An unverified certificate stops before the password is sent.
	_, err := signIn(t, srv, VNCAuth{Security: "vencrypt", Password: "pw", TLS: &tls.Config{ServerName: "127.0.0.1"}})
	var ve *tls.CertificateVerificationError
	if !asCertErr(err, &ve) {
		t.Fatalf("err = %v, want a certificate verification error", err)
	}

	// Only anonymous-TLS subtypes, which Go cannot verify: refused by name.
	srv.Set(func(c *rfbtest.Config) { c.SubTypes = []uint32{258} })
	if _, err := signIn(t, srv, VNCAuth{Security: "vencrypt", Password: "pw", TLS: cfg}); err == nil || !strings.Contains(err.Error(), "TLSVnc") {
		t.Fatalf("err = %v, want a refusal naming TLSVnc", err)
	}
}

// noDeadlineConn ignores deadlines, as an SSH channel through a jump host
// does.
type noDeadlineConn struct{ net.Conn }

func (noDeadlineConn) SetDeadline(time.Time) error { return errors.New("deadline not supported") }

// A server that accepts but never answers cannot hang a sign-in over a
// conn that ignores deadlines: closing the conn bounds it.
func TestSignInGivesUpOnASilentServerWithoutDeadlines(t *testing.T) {
	old := handshakeTimeout
	handshakeTimeout = 100 * time.Millisecond
	defer func() { handshakeTimeout = old }()
	for name, signIn := range map[string]func(net.Conn) error{
		"vnc": func(c net.Conn) error {
			_, err := SignIn(c, VNCAuth{Security: "none"})
			return err
		},
		"spice": func(c net.Conn) error {
			_, _, err := SpiceSignIn(c, []byte("link"), "pw", nil)
			return err
		},
	} {
		client, server := net.Pipe()
		done := make(chan error, 1)
		go func() { done <- signIn(noDeadlineConn{client}) }()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%s: a silent server signed in", name)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("%s: sign-in still waiting on a silent server", name)
		}
		client.Close()
		server.Close()
	}
}
