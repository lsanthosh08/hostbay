package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"slices"
	"strconv"
	"testing"
	"time"

	"hostbay/internal/apperr"
	"hostbay/internal/remotedesktop"
	"hostbay/internal/remotedesktop/rfbtest"
	"hostbay/internal/remotedesktop/spicetest"
	"hostbay/internal/sftp/sftptest"
	"hostbay/internal/sshx/proxytest"
	"hostbay/internal/tlstrust"
	"hostbay/internal/tlstrust/tlstest"
)

func newVncService(t *testing.T, knownHosts ...string) (*VncService, *HostService) {
	t.Helper()
	c, s, dir := connWithKnownHosts(t, knownHosts...)
	return NewVncService(c.creds, c, tlstrust.New(dir)), s
}

func vncHost(srv *rfbtest.Server, security, pw string) Host {
	return Host{Alias: "desk", Hostname: srv.Host, Port: srv.Port, Kind: new("vnc"), TLS: &security, Password: &pw}
}

func countOf(log []string, e string) int {
	n := 0
	for _, l := range log {
		if l == e {
			n++
		}
	}
	return n
}

// A VNC host is reached through its proxy only; a proxy that is down
// fails the connect and the server sees no direct connection (KTD22).
func TestVncConnectGoesThroughTheProxyOnly(t *testing.T) {
	srv := rfbtest.New(t, "pw", 2)
	v, s := newVncService(t)
	p := proxytest.SOCKS5(t, "", "")
	px, err := s.SaveProxy(Proxy{Name: "office", Type: "socks5", Host: p.Host, Port: p.Port})
	if err != nil {
		t.Fatal(err)
	}
	form := vncHost(srv, "none", "pw")
	form.ProxyID = &px.ID
	h, err := s.SaveHost(form)
	if err != nil {
		t.Fatal(err)
	}
	id, err := v.VncConnect(context.Background(), h.ID)
	if err != nil {
		t.Fatal(err)
	}
	v.VncClose(id)
	if !slices.Contains(p.Targets(), srv.Addr()) {
		t.Fatalf("proxy carried %v, want %s", p.Targets(), srv.Addr())
	}

	down, port := proxytest.Down(t)
	dead, err := s.SaveProxy(Proxy{Name: "dead", Type: "http", Host: down, Port: port})
	if err != nil {
		t.Fatal(err)
	}
	h.ProxyID = &dead.ID
	if _, err := s.SaveHost(h); err != nil {
		t.Fatal(err)
	}
	before := srv.Conns()
	if _, err := v.VncConnect(context.Background(), h.ID); apperr.CodeOf(err) != apperr.ProxyFailed {
		t.Fatalf("err = %v, want ProxyFailed", err)
	}
	if srv.Conns() != before {
		t.Fatal("the server saw a direct connection")
	}
}

// A VNC host behind a jump host is dialled from the jump host, by a name
// only the jump host can resolve.
func TestVncConnectGoesThroughTheJumpHost(t *testing.T) {
	srv := rfbtest.New(t, "pw", 2)
	jump := sftptest.New(t, "jump-pw")
	jump.Resolve("vnc.internal", srv.Addr())
	v, s := newVncService(t, jump.KnownHostsLine())
	jhost, jport, _ := net.SplitHostPort(jump.Addr)
	jp, _ := strconv.Atoi(jport)
	auth, jpw := "password", "jump-pw"
	j, err := s.SaveHost(Host{Alias: "bastion", Hostname: jhost, Port: jp, User: "u", AuthType: &auth, Password: &jpw})
	if err != nil {
		t.Fatal(err)
	}
	form := vncHost(srv, "none", "pw")
	form.Hostname, form.JumpHostID = "vnc.internal", &j.ID
	h, err := s.SaveHost(form)
	if err != nil {
		t.Fatal(err)
	}
	id, err := v.VncConnect(context.Background(), h.ID)
	if err != nil {
		t.Fatal(err)
	}
	v.VncClose(id)
	if want := net.JoinHostPort("vnc.internal", strconv.Itoa(srv.Port)); !slices.Contains(jump.Dialed(), want) {
		t.Fatalf("jump host dialled %v, want %s", jump.Dialed(), want)
	}
}

// VeNCrypt asks before trusting an unknown certificate and refuses a
// changed one, and the password is sent in neither case (KTD18).
func TestVncVeNCryptPinsTheCertificate(t *testing.T) {
	srv := rfbtest.New(t, "pw", 19)
	srv.Set(func(c *rfbtest.Config) { c.SubTypes, c.Cert = []uint32{261}, tlstest.Cert(t) })
	v, s := newVncService(t)
	h, err := s.SaveHost(vncHost(srv, "", "pw")) // unset is VeNCrypt
	if err != nil {
		t.Fatal(err)
	}
	_, err = v.VncConnect(context.Background(), h.ID)
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != apperr.HostKeyUnknown || ae.Details["kind"] != "vncCert" {
		t.Fatalf("err = %v, want a vncCert trust prompt", err)
	}
	if n := countOf(srv.Log(), "auth ok") + countOf(srv.Log(), "auth failed"); n != 0 {
		t.Fatal("a sign-in was attempted before the certificate was trusted")
	}
	if err := v.VncTrustCert(ae.Details["requestId"], true); err != nil {
		t.Fatal(err)
	}
	id, err := v.VncConnect(context.Background(), h.ID)
	if err != nil {
		t.Fatal(err)
	}
	v.VncClose(id)

	srv.Set(func(c *rfbtest.Config) { c.Cert = tlstest.Cert(t) })
	_, err = v.VncConnect(context.Background(), h.ID)
	if !errors.As(err, &ae) || ae.Code != apperr.HostKeyChanged {
		t.Fatalf("err = %v, want HostKeyChanged", err)
	}
	if n := countOf(srv.Log(), "auth ok") + countOf(srv.Log(), "auth failed"); n != 1 {
		t.Fatalf("%d sign-ins, want only the one after trusting", n)
	}
}

// A refused password asks for the password, once, naming the host.
func TestVncWrongPasswordAsksOnce(t *testing.T) {
	srv := rfbtest.New(t, "right", 2)
	v, s := newVncService(t)
	h, err := s.SaveHost(vncHost(srv, "none", "wrong"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = v.VncConnect(context.Background(), h.ID)
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != apperr.SecretNeeded || ae.Details["hostId"] != h.ID {
		t.Fatalf("err = %v, want SecretNeeded naming the host", err)
	}
	if n := srv.Conns(); n != 1 {
		t.Fatalf("the server saw %d connections, want 1", n)
	}
}

// The stream relays only the session it names, once; the page going away
// hangs up on the server.
func TestVncStreamRelaysOnlyItsSession(t *testing.T) {
	srv := rfbtest.New(t, "pw", 2)
	v, s := newVncService(t)
	h, err := s.SaveHost(vncHost(srv, "none", "pw"))
	if err != nil {
		t.Fatal(err)
	}
	id, err := v.VncConnect(context.Background(), h.ID)
	if err != nil {
		t.Fatal(err)
	}

	stranger := rfbtest.NewPage()
	stranger.Feed([]byte("not-a-session"))
	v.serve(stranger)
	if len(stranger.Sent()) != 0 {
		t.Fatalf("an unknown ID got %q", stranger.Sent())
	}

	page := rfbtest.NewPage()
	page.Feed([]byte(id))
	done := make(chan struct{})
	go func() { v.serve(page); close(done) }()
	if got := page.Read(t, 0, 12); string(got) != "RFB 003.008\n" {
		t.Fatalf("greeting %q", got)
	}
	second := rfbtest.NewPage()
	second.Feed([]byte(id))
	ended := make(chan struct{})
	go func() { v.serve(second); close(ended) }()
	select {
	case <-ended:
	case <-time.After(time.Second):
		second.Close()
		t.Fatal("a second stream took over the session")
	}
	if len(second.Sent()) != 0 {
		t.Fatal("a second stream got the session's bytes")
	}

	page.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the relay outlived the page")
	}
	waitUntil(t, "the server to see the hang-up", func() bool { return slices.Contains(srv.Log(), "closed") })
}

func newSpiceService(t *testing.T, knownHosts ...string) (*SpiceService, *HostService) {
	t.Helper()
	c, s, dir := connWithKnownHosts(t, knownHosts...)
	return NewSpiceService(c.creds, c, tlstrust.New(dir)), s
}

func spiceHost(srv *spicetest.Server, security, pw string) Host {
	return Host{Alias: "vm", Hostname: srv.Host, Port: srv.Port, Kind: new("spice"), TLS: &security, Password: &pw}
}

// spiceLink is the page's link message for a channel: the main one's,
// retargeted, with no channel caps.
func spiceLink(typ, id byte) []byte {
	b := bytes.Clone(remotedesktop.SpiceMainLink)
	b[20], b[21] = typ, id
	copy(b[38:], []byte{0, 0, 0, 0})
	return b
}

// startChannel opens a channel stream the way spice-html5 does: the
// spiceId, then the link message. done closes when the stream ends.
func startChannel(t *testing.T, sp *SpiceService, id string, link []byte) (page *rfbtest.Page, done chan struct{}) {
	page = rfbtest.NewPage()
	page.Feed([]byte(id))
	page.Feed(link)
	done = make(chan struct{})
	go func() { sp.serve(page); close(done) }()
	t.Cleanup(page.Close)
	return page, done
}

// linked answers the link reply with an empty-password ticket, as
// spice-html5 does with no password, and checks the link succeeded.
func linked(t *testing.T, page *rfbtest.Page) {
	t.Helper()
	n := 16 + int(page.Read(t, 0, 16)[12])
	page.Read(t, 16, n)
	page.Feed(make([]byte, 132))
	if got := page.Read(t, n, n+4); !bytes.Equal(got, []byte{0, 0, 0, 0}) {
		t.Fatalf("link result %v", got)
	}
}

// openChannel opens a channel stream and waits until it is linked.
func openChannel(t *testing.T, sp *SpiceService, id string, link []byte) (*rfbtest.Page, chan struct{}) {
	t.Helper()
	page, done := startChannel(t, sp, id, link)
	linked(t, page)
	return page, done
}

// refused runs a stream and reports whether it ended having sent nothing.
func refused(sp *SpiceService, frames ...[]byte) bool {
	page := rfbtest.NewPage()
	for _, f := range frames {
		page.Feed(f)
	}
	done := make(chan struct{})
	go func() { sp.serve(page); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		page.Close()
		return false
	}
	return len(page.Sent()) == 0
}

// Every SPICE channel goes through the host's proxy only; a proxy that is
// down fails the connect and the server sees no direct connection (KTD22).
func TestSpiceConnectGoesThroughTheProxyOnly(t *testing.T) {
	srv := spicetest.New(t, "pw")
	sp, s := newSpiceService(t)
	p := proxytest.SOCKS5(t, "", "")
	px, err := s.SaveProxy(Proxy{Name: "office", Type: "socks5", Host: p.Host, Port: p.Port})
	if err != nil {
		t.Fatal(err)
	}
	form := spiceHost(srv, "none", "pw")
	form.ProxyID = &px.ID
	h, err := s.SaveHost(form)
	if err != nil {
		t.Fatal(err)
	}
	id, err := sp.SpiceConnect(context.Background(), h.ID)
	if err != nil {
		t.Fatal(err)
	}
	openChannel(t, sp, id, remotedesktop.SpiceMainLink)
	openChannel(t, sp, id, spiceLink(2, 0))
	sp.SpiceClose(id)
	if n := countOf(p.Targets(), srv.Addr()); n != 2 {
		t.Fatalf("proxy carried %v, want the main and display channels to %s", p.Targets(), srv.Addr())
	}

	down, port := proxytest.Down(t)
	dead, err := s.SaveProxy(Proxy{Name: "dead", Type: "http", Host: down, Port: port})
	if err != nil {
		t.Fatal(err)
	}
	h.ProxyID = &dead.ID
	if _, err := s.SaveHost(h); err != nil {
		t.Fatal(err)
	}
	before := srv.Conns()
	if _, err := sp.SpiceConnect(context.Background(), h.ID); apperr.CodeOf(err) != apperr.ProxyFailed {
		t.Fatalf("err = %v, want ProxyFailed", err)
	}
	if srv.Conns() != before {
		t.Fatal("the server saw a direct connection")
	}
}

// A SPICE host behind a jump host is dialled from the jump host, for the
// main channel and every later one.
func TestSpiceConnectGoesThroughTheJumpHost(t *testing.T) {
	srv := spicetest.New(t, "pw")
	jump := sftptest.New(t, "jump-pw")
	jump.Resolve("vm.internal", srv.Addr())
	sp, s := newSpiceService(t, jump.KnownHostsLine())
	jhost, jport, _ := net.SplitHostPort(jump.Addr)
	jp, _ := strconv.Atoi(jport)
	auth, jpw := "password", "jump-pw"
	j, err := s.SaveHost(Host{Alias: "bastion", Hostname: jhost, Port: jp, User: "u", AuthType: &auth, Password: &jpw})
	if err != nil {
		t.Fatal(err)
	}
	form := spiceHost(srv, "none", "pw")
	form.Hostname, form.JumpHostID = "vm.internal", &j.ID
	h, err := s.SaveHost(form)
	if err != nil {
		t.Fatal(err)
	}
	id, err := sp.SpiceConnect(context.Background(), h.ID)
	if err != nil {
		t.Fatal(err)
	}
	openChannel(t, sp, id, remotedesktop.SpiceMainLink)
	openChannel(t, sp, id, spiceLink(3, 0))
	sp.SpiceClose(id)
	if n := countOf(jump.Dialed(), net.JoinHostPort("vm.internal", strconv.Itoa(srv.Port))); n != 2 {
		t.Fatalf("jump host dialled %v, want vm.internal twice", jump.Dialed())
	}
}

// A TLS port asks before trusting an unknown certificate and refuses a
// changed one, and no ticket is sent in either case; a plain port is
// refused for a host set to TLS, with no fallback (KTD18).
func TestSpiceTLSPinsTheCertificate(t *testing.T) {
	srv := spicetest.New(t, "pw")
	srv.Set(func(c *spicetest.Config) { c.Cert = tlstest.Cert(t) })
	sp, s := newSpiceService(t)
	h, err := s.SaveHost(spiceHost(srv, "", "pw")) // unset is a TLS port
	if err != nil {
		t.Fatal(err)
	}
	_, err = sp.SpiceConnect(context.Background(), h.ID)
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != apperr.HostKeyUnknown || ae.Details["kind"] != "spiceCert" {
		t.Fatalf("err = %v, want a spiceCert trust prompt", err)
	}
	if n := countOf(srv.Log(), "ticket ok") + countOf(srv.Log(), "ticket wrong"); n != 0 {
		t.Fatal("a ticket was sent before the certificate was trusted")
	}
	if err := sp.SpiceTrustCert(ae.Details["requestId"], true); err != nil {
		t.Fatal(err)
	}
	id, err := sp.SpiceConnect(context.Background(), h.ID)
	if err != nil {
		t.Fatal(err)
	}
	openChannel(t, sp, id, remotedesktop.SpiceMainLink)
	openChannel(t, sp, id, spiceLink(2, 0)) // later channels use the pinned certificate
	sp.SpiceClose(id)

	srv.Set(func(c *spicetest.Config) { c.Cert = tlstest.Cert(t) })
	_, err = sp.SpiceConnect(context.Background(), h.ID)
	if !errors.As(err, &ae) || ae.Code != apperr.HostKeyChanged {
		t.Fatalf("err = %v, want HostKeyChanged", err)
	}
	if n := countOf(srv.Log(), "ticket ok") + countOf(srv.Log(), "ticket wrong"); n != 2 {
		t.Fatalf("%d tickets, want only the two channels after trusting", n)
	}

	srv.Set(func(c *spicetest.Config) { c.Cert = tls.Certificate{} })
	if _, err := sp.SpiceConnect(context.Background(), h.ID); err == nil {
		t.Fatal("a plain SPICE port was accepted for a host set to TLS")
	}
	if n := countOf(srv.Log(), "ticket ok") + countOf(srv.Log(), "ticket wrong"); n != 2 {
		t.Fatal("a ticket went to the plain port")
	}
}

// A refused password asks for the password, once, naming the host.
func TestSpiceWrongPasswordAsksOnce(t *testing.T) {
	srv := spicetest.New(t, "right")
	sp, s := newSpiceService(t)
	h, err := s.SaveHost(spiceHost(srv, "none", "wrong"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = sp.SpiceConnect(context.Background(), h.ID)
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != apperr.SecretNeeded || ae.Details["hostId"] != h.ID {
		t.Fatalf("err = %v, want SecretNeeded naming the host", err)
	}
	if n := srv.Conns(); n != 1 {
		t.Fatalf("the server saw %d connections, want 1", n)
	}
}

// One session carries its main channel and then several channels at once,
// each signed in by Go; unknown IDs, channels before main and channels
// linked twice get nothing; no byte of the password reaches the page; and
// the main stream closing ends every channel (KTD19).
func TestSpiceStreamCarriesTheSessionsChannels(t *testing.T) {
	const pw = "Spice-Session-4"
	srv := spicetest.New(t, pw)
	sp, s := newSpiceService(t)
	h, err := s.SaveHost(spiceHost(srv, "none", pw))
	if err != nil {
		t.Fatal(err)
	}
	id, err := sp.SpiceConnect(context.Background(), h.ID)
	if err != nil {
		t.Fatal(err)
	}

	if !refused(sp, []byte("not-a-session"), remotedesktop.SpiceMainLink) {
		t.Fatal("an unknown ID got the session")
	}
	if !refused(sp, []byte(id), spiceLink(2, 0)) {
		t.Fatal("a channel linked before the main channel")
	}
	altered := bytes.Clone(remotedesktop.SpiceMainLink)
	altered[38] = 0
	if !refused(sp, []byte(id), altered) {
		t.Fatal("a main link unlike Go's got the session")
	}

	main, mainDone := openChannel(t, sp, id, remotedesktop.SpiceMainLink)
	if !refused(sp, []byte(id), remotedesktop.SpiceMainLink) {
		t.Fatal("a second main stream took over the session")
	}
	// Display, inputs and cursor link at once, each dialling and signing in
	// on its own.
	var pages []*rfbtest.Page
	var dones []chan struct{}
	for _, typ := range []byte{2, 3, 4} {
		p, d := startChannel(t, sp, id, spiceLink(typ, 0))
		pages, dones = append(pages, p), append(dones, d)
	}
	for _, p := range pages {
		linked(t, p)
	}
	if !refused(sp, []byte(id), spiceLink(2, 0)) {
		t.Fatal("a channel linked twice")
	}
	for i, p := range pages {
		n := len(p.Sent())
		p.Feed([]byte("ping"))
		if got := p.Read(t, n, n+4); string(got) != "ping" {
			t.Fatalf("channel %d echoed %q", i, got)
		}
		if bytes.Contains(p.Sent(), []byte(pw)) {
			t.Fatalf("channel %d: the password reached the page", i)
		}
	}
	if bytes.Contains(main.Sent(), []byte(pw)) {
		t.Fatal("the password reached the page")
	}
	if n := countOf(srv.Log(), "ticket ok"); n != 4 {
		t.Fatalf("%d tickets accepted, want 4: %v", n, srv.Log())
	}

	main.Close()
	for _, d := range append(dones, mainDone) {
		select {
		case <-d:
		case <-time.After(5 * time.Second):
			t.Fatal("a channel outlived the main stream")
		}
	}
	for _, c := range []string{"closed 1:0", "closed 2:0", "closed 3:0", "closed 4:0"} {
		waitUntil(t, "the server to see "+c, func() bool { return slices.Contains(srv.Log(), c) })
	}
	if !refused(sp, []byte(id), spiceLink(5, 0)) {
		t.Fatal("an ended session took a channel")
	}
}
