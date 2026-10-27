package tlstrust

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"hostbay/internal/apperr"
	"hostbay/internal/tlstrust/tlstest"
)

func TestUnknownCertificateAsksThenPins(t *testing.T) {
	dir := t.TempDir()
	ct := New(dir)
	der := []byte("cert-one")

	err := ct.Check("rdp", "host1", "win.lan", 3389, der)
	var ae *apperr.Error
	if apperr.CodeOf(err) != apperr.HostKeyUnknown {
		t.Fatalf("err = %v", err)
	}
	ae = err.(*apperr.Error)
	if ae.Details["fingerprint"] != Fingerprint(der) || ae.Details["kind"] != "rdpCert" || ae.Details["requestId"] == "" {
		t.Fatalf("details = %v", ae.Details)
	}

	// Declining pins nothing; the request is spent.
	if err := ct.Answer(ae.Details["requestId"], false); err != nil {
		t.Fatal(err)
	}
	if err := ct.Answer(ae.Details["requestId"], true); err == nil {
		t.Fatal("a spent request was answered")
	}
	if apperr.CodeOf(ct.Check("rdp", "host1", "win.lan", 3389, der)) != apperr.HostKeyUnknown {
		t.Fatal("a declined certificate was pinned")
	}

	// Accepting pins it, across a restart, for that host and target only.
	ae = ct.Check("rdp", "host1", "win.lan", 3389, der).(*apperr.Error)
	if err := ct.Answer(ae.Details["requestId"], true); err != nil {
		t.Fatal(err)
	}
	ct = New(dir)
	if err := ct.Check("rdp", "host1", "win.lan", 3389, der); err != nil {
		t.Fatalf("pinned certificate refused: %v", err)
	}
	if apperr.CodeOf(ct.Check("rdp", "host1", "win.lan", 3390, der)) != apperr.HostKeyUnknown {
		t.Fatal("the pin leaked to another port")
	}
	if apperr.CodeOf(ct.Check("rdp", "host2", "win.lan", 3389, der)) != apperr.HostKeyUnknown {
		t.Fatal("the pin leaked to another host")
	}
	fi, err := os.Stat(filepath.Join(dir, certFile("rdp")))
	if err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600) {
		t.Fatalf("pin file: %v %v", fi, err)
	}
}

func TestChangedCertificateIsRefusedWithBothFingerprints(t *testing.T) {
	ct := New(t.TempDir())
	old, cur := []byte("cert-one"), []byte("cert-two")
	ae := ct.Check("rdp", "h", "win.lan", 3389, old).(*apperr.Error)
	if err := ct.Answer(ae.Details["requestId"], true); err != nil {
		t.Fatal(err)
	}

	err := ct.Check("rdp", "h", "win.lan", 3389, cur)
	if apperr.CodeOf(err) != apperr.HostKeyChanged {
		t.Fatalf("err = %v", err)
	}
	ae = err.(*apperr.Error)
	if ae.Details["expected"] != Fingerprint(old) || ae.Details["fingerprint"] != Fingerprint(cur) || ae.Details["requestId"] != "" {
		t.Fatalf("details = %v", ae.Details)
	}
	if !strings.Contains(ae.Message, Fingerprint(old)) || !strings.Contains(ae.Message, Fingerprint(cur)) {
		t.Fatalf("message = %q", ae.Message)
	}
}

func TestExpiredCertificatePromptIsRefused(t *testing.T) {
	ct := New(t.TempDir())
	now := time.Now()
	ct.now = func() time.Time { return now }
	ae := ct.Check("rdp", "h", "win.lan", 3389, []byte("c")).(*apperr.Error)
	now = now.Add(pendingTTL + time.Second)
	if err := ct.Answer(ae.Details["requestId"], true); err == nil {
		t.Fatal("an expired prompt was accepted")
	}
}

func TestFingerprintFormat(t *testing.T) {
	// sha256("") in the OpenSSH fingerprint form.
	if fp := Fingerprint(nil); fp != "SHA256:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU" {
		t.Fatal(fp)
	}
}

func TestExpiredCertificatePromptIsSweptByLaterCheck(t *testing.T) {
	ct := New(t.TempDir())
	now := time.Now()
	ct.now = func() time.Time { return now }
	old := ct.Check("rdp", "h", "win.lan", 3389, []byte("c")).(*apperr.Error).Details["requestId"]
	now = now.Add(pendingTTL + time.Second)
	ct.Check("rdp", "h", "win.lan", 3389, []byte("c"))
	if _, ok := ct.pending[old]; ok || len(ct.pending) != 1 {
		t.Fatalf("pending = %d entries, expired one kept = %v", len(ct.pending), ok)
	}
}

func TestPinsAreKeptPerKindAndRDPKeepsItsFile(t *testing.T) {
	dir := t.TempDir()
	ct := New(dir)
	ae := ct.Check("rdp", "h", "srv.lan", 990, []byte("c")).(*apperr.Error)
	if err := ct.Answer(ae.Details["requestId"], true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "rdp-certs.json")); err != nil {
		t.Fatalf("RDP pins moved out of rdp-certs.json, so existing pins would be lost: %v", err)
	}
	err := ct.Check("ftp", "h", "srv.lan", 990, []byte("c"))
	if apperr.CodeOf(err) != apperr.HostKeyUnknown || err.(*apperr.Error).Details["kind"] != "ftpCert" {
		t.Fatalf("an RDP pin vouched for FTPS: %v", err)
	}
}

// tlsServer serves TLS with cert until the test ends.
func tlsServer(t *testing.T, cert tls.Certificate) int {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.(*tls.Conn).Handshake()
			c.Close()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// dialer is a try function that counts the attempts that got past the
// handshake: those are the ones that would have signed in.
func dialer(port int, signedIn *int) func(*tls.Config) error {
	return func(cfg *tls.Config) error {
		c, err := tls.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), cfg)
		if err != nil {
			return err
		}
		*signedIn++
		return c.Close()
	}
}

func TestConnectAsksForAnUnknownCertificateBeforeSigningIn(t *testing.T) {
	ct := New(t.TempDir())
	cert := tlstest.Cert(t)
	port := tlsServer(t, cert)
	var signedIn int

	_, err := ct.Connect("ftp", "h", "127.0.0.1", port, dialer(port, &signedIn))
	ae, ok := err.(*apperr.Error)
	if !ok || ae.Code != apperr.HostKeyUnknown || ae.Details["kind"] != "ftpCert" || ae.Details["fingerprint"] != Fingerprint(cert.Certificate[0]) {
		t.Fatalf("err = %v", err)
	}
	if signedIn != 0 {
		t.Fatal("signed in to a server whose certificate is not trusted")
	}
	if err := ct.Answer(ae.Details["requestId"], false); err != nil {
		t.Fatal(err)
	}
	if _, err := ct.Connect("ftp", "h", "127.0.0.1", port, dialer(port, &signedIn)); apperr.CodeOf(err) != apperr.HostKeyUnknown || signedIn != 0 {
		t.Fatalf("after declining: %v, signed in %d times", err, signedIn)
	}

	ae = func() *apperr.Error {
		_, err := ct.Connect("ftp", "h", "127.0.0.1", port, dialer(port, &signedIn))
		return err.(*apperr.Error)
	}()
	if err := ct.Answer(ae.Details["requestId"], true); err != nil {
		t.Fatal(err)
	}
	cfg, err := ct.Connect("ftp", "h", "127.0.0.1", port, dialer(port, &signedIn))
	if err != nil || signedIn != 1 {
		t.Fatalf("pinned certificate: %v, signed in %d times", err, signedIn)
	}
	// The returned config accepts only the pinned certificate.
	other := tlsServer(t, tlstest.Cert(t))
	if err := dialer(other, &signedIn)(cfg); err == nil || signedIn != 1 {
		t.Fatalf("the pinned config accepted another certificate: %v", err)
	}
}

func TestConnectRefusesAChangedCertificateWithBothFingerprints(t *testing.T) {
	ct := New(t.TempDir())
	old, cur := tlstest.Cert(t), tlstest.Cert(t)
	port := tlsServer(t, old)
	var signedIn int
	_, err := ct.Connect("webdav", "h", "127.0.0.1", port, dialer(port, &signedIn))
	if err := ct.Answer(err.(*apperr.Error).Details["requestId"], true); err != nil {
		t.Fatal(err)
	}

	port2 := tlsServer(t, cur)
	// The same host and port as the pin, reached on the new server's port.
	try := dialer(port2, &signedIn)
	_, err = ct.Connect("webdav", "h", "127.0.0.1", port, try)
	ae, ok := err.(*apperr.Error)
	if !ok || ae.Code != apperr.HostKeyChanged || ae.Details["expected"] != Fingerprint(old.Certificate[0]) || ae.Details["fingerprint"] != Fingerprint(cur.Certificate[0]) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(ae.Message, "WebDAV") || signedIn != 0 {
		t.Fatalf("message %q, signed in %d times", ae.Message, signedIn)
	}
}

// flattening is a driver that turns every TLS error into text, as
// go-mssqldb, mongo-driver and go-ora do: Connect would find no leaf.
func flattening(try func(*tls.Config) error) func(*tls.Config) error {
	return func(cfg *tls.Config) error {
		if err := try(cfg); err != nil {
			return errors.New("driver: " + err.Error())
		}
		return nil
	}
}

// Capture finds the unverified leaf inside the TLS config, so a driver that
// flattens errors still raises the prompt, and nothing signs in before the
// certificate is trusted (KTD25).
func TestCaptureAsksThroughADriverThatFlattensErrors(t *testing.T) {
	ct := New(t.TempDir())
	cert := tlstest.Cert(t)
	port := tlsServer(t, cert)
	var signedIn int
	try := flattening(dialer(port, &signedIn))

	_, err := ct.Capture("postgres", "h", "127.0.0.1", port, try)
	ae, ok := err.(*apperr.Error)
	if !ok || ae.Code != apperr.HostKeyUnknown || ae.Details["kind"] != "postgresCert" || ae.Details["fingerprint"] != Fingerprint(cert.Certificate[0]) {
		t.Fatalf("err = %v", err)
	}
	if signedIn != 0 || !strings.Contains(ae.Message, "PostgreSQL") {
		t.Fatalf("message %q, signed in %d times", ae.Message, signedIn)
	}
	if err := ct.Answer(ae.Details["requestId"], true); err != nil {
		t.Fatal(err)
	}
	cfg, err := ct.Capture("postgres", "h", "127.0.0.1", port, try)
	if err != nil || signedIn != 1 {
		t.Fatalf("pinned certificate: %v, signed in %d times", err, signedIn)
	}
	// The returned config still checks the pin on every later connection.
	other := tlsServer(t, tlstest.Cert(t))
	if err := dialer(other, &signedIn)(cfg); err == nil || signedIn != 1 {
		t.Fatalf("the pinned config accepted another certificate: %v", err)
	}
}

func TestCaptureRefusesAChangedCertificateWithBothFingerprints(t *testing.T) {
	ct := New(t.TempDir())
	old, cur := tlstest.Cert(t), tlstest.Cert(t)
	port := tlsServer(t, old)
	var signedIn int
	_, err := ct.Capture("postgres", "h", "127.0.0.1", port, flattening(dialer(port, &signedIn)))
	if err := ct.Answer(err.(*apperr.Error).Details["requestId"], true); err != nil {
		t.Fatal(err)
	}
	_, err = ct.Capture("postgres", "h", "127.0.0.1", port, flattening(dialer(tlsServer(t, cur), &signedIn)))
	ae, ok := err.(*apperr.Error)
	if !ok || ae.Code != apperr.HostKeyChanged || ae.Details["expected"] != Fingerprint(old.Certificate[0]) || ae.Details["fingerprint"] != Fingerprint(cur.Certificate[0]) || signedIn != 0 {
		t.Fatalf("err = %v, signed in %d times", err, signedIn)
	}
}

// pinAndConnect raises the prompt through connect, accepts it, and connects
// again, returning that second attempt's result.
func pinAndConnect(t *testing.T, ct *Store, connect func() error) error {
	t.Helper()
	ae, ok := connect().(*apperr.Error)
	if !ok || ae.Code != apperr.HostKeyUnknown {
		t.Fatalf("no prompt: %v", ae)
	}
	if err := ct.Answer(ae.Details["requestId"], true); err != nil {
		t.Fatal(err)
	}
	return connect()
}

// A pin trusts those exact certificate bytes, so a server whose own
// certificate names only its container, as SQL Server's and MySQL's
// self-made ones do, still connects by address once pinned.
func TestPinnedCertificateNeedNotNameTheHost(t *testing.T) {
	cert := tlstest.CertFor(t, []string{"db-container"}, nil)
	port := tlsServer(t, cert)
	for name, connect := range map[string]func(*Store, *int) error{
		"Connect": func(ct *Store, n *int) error {
			_, err := ct.Connect("ftp", "h", "127.0.0.1", port, dialer(port, n))
			return err
		},
		"Capture": func(ct *Store, n *int) error {
			_, err := ct.Capture("postgres", "h", "127.0.0.1", port, flattening(dialer(port, n)))
			return err
		},
	} {
		ct := New(t.TempDir())
		var signedIn int
		if err := pinAndConnect(t, ct, func() error { return connect(ct, &signedIn) }); err != nil || signedIn != 1 {
			t.Errorf("%s: pinned certificate refused: %v, signed in %d times", name, err, signedIn)
		}
	}
}

// A pinned certificate that has since expired is refused before sign-in.
func TestExpiredPinnedCertificateIsRefused(t *testing.T) {
	cert := tlstest.Cert(t)
	port := tlsServer(t, cert)
	for name, connect := range map[string]func(*Store, *int) error{
		"Connect": func(ct *Store, n *int) error {
			_, err := ct.Connect("ftp", "h", "127.0.0.1", port, dialer(port, n))
			return err
		},
		"Capture": func(ct *Store, n *int) error {
			_, err := ct.Capture("postgres", "h", "127.0.0.1", port, flattening(dialer(port, n)))
			return err
		},
	} {
		ct := New(t.TempDir())
		var signedIn int
		if err := pinAndConnect(t, ct, func() error { return connect(ct, &signedIn) }); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		ct.now = func() time.Time { return cert.Leaf.NotAfter.Add(time.Minute) }
		err := connect(ct, &signedIn)
		if err == nil || !strings.Contains(err.Error(), "has expired") || signedIn != 1 {
			t.Errorf("%s: expired pin: %v, signed in %d times", name, err, signedIn)
		}
	}
}

// A pin is removed only with the refused host's name typed and OS
// verification passed; a mistyped name or a failed verification keeps it.
func TestRemovePinNeedsTypedNameAndVerification(t *testing.T) {
	ct := New(t.TempDir())
	ae := ct.Check("postgres", "h", "db.lan", 5432, []byte("c")).(*apperr.Error)
	if err := ct.Answer(ae.Details["requestId"], true); err != nil {
		t.Fatal(err)
	}
	id := ct.RemovalRequest("postgres", "h", "db.lan", 5432)
	pass := func(context.Context, string) error { return nil }
	fail := func(context.Context, string) error { return apperr.New(apperr.VerifyFailed, "cancelled") }
	if err := ct.RemovePin(context.Background(), id, "db.lan.", pass); err == nil {
		t.Fatal("a mistyped host name removed the pin")
	}
	if err := ct.RemovePin(context.Background(), id, "db.lan", fail); apperr.CodeOf(err) != apperr.VerifyFailed {
		t.Fatalf("failed verification: %v", err)
	}
	if err := ct.Check("postgres", "h", "db.lan", 5432, []byte("c")); err != nil {
		t.Fatalf("the pin is gone: %v", err)
	}
	if err := ct.RemovePin(context.Background(), id, "db.lan", pass); err != nil {
		t.Fatal(err)
	}
	if apperr.CodeOf(ct.Check("postgres", "h", "db.lan", 5432, []byte("c"))) != apperr.HostKeyUnknown {
		t.Fatal("the pin was not removed")
	}
	if err := ct.RemovePin(context.Background(), id, "db.lan", pass); err == nil {
		t.Fatal("a used removal request worked twice")
	}
}

// A kubeconfig's CA is the only trust anchor (KTD28): a server it signed
// verifies, under the TLS server name when one is set, and any other is
// refused outright, with no pin prompt.
func TestAnchoredTrustsOnlyTheGivenCA(t *testing.T) {
	cert := tlstest.Cert(t)
	port := tlsServer(t, cert)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	signedIn := 0
	cfg, err := Anchored(ca, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := dialer(port, &signedIn)(cfg); err != nil || signedIn != 1 {
		t.Fatalf("the CA's own server was refused: %v", err)
	}
	if cfg, err = Anchored(ca, "localhost"); err != nil || dialer(port, &signedIn)(cfg) != nil {
		t.Fatalf("the TLS server name is the name checked: %v", err)
	}
	other := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlstest.Cert(t).Certificate[0]})
	cfg, _ = Anchored(other, "127.0.0.1")
	err = dialer(port, &signedIn)(cfg)
	if err == nil || signedIn != 2 || apperr.CodeOf(err) != "" {
		t.Fatalf("a server another CA signed must fail without a prompt: %v", err)
	}
	if _, err := Anchored([]byte("not a certificate"), "x"); err == nil {
		t.Fatal("CA data with no certificate must be refused")
	}
}
