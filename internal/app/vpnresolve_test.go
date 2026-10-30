package app

import (
	"context"
	"errors"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"hostbay/internal/apperr"
	"hostbay/internal/sshx/proxytest"
	"hostbay/internal/store"
	"hostbay/internal/tlstrust"
	"hostbay/internal/vpn/detect"
	"hostbay/internal/vpn/socks"
	"hostbay/internal/vpn/vpntest"
)

// vpnConn is a ConnService whose VPN profiles start the fake helper in mode.
func vpnConn(t *testing.T, mode string, known ...string) (*ConnService, *HostService, *vpntest.Fake) {
	t.Helper()
	c, s, _ := connWithKnownHosts(t, known...)
	f := vpntest.Install(t, mode)
	c.creds.vpns.helper = func() (string, error) { return f.Path, nil }
	t.Cleanup(func() { c.creds.vpns.ServiceShutdown() })
	return c, s, f
}

// saveProfile saves an app-scoped WireGuard profile.
func saveProfile(t *testing.T, v *VPNService, name string, edit func(*VPN)) VPN {
	t.Helper()
	key := testWGKey
	p := VPN{Name: name, Type: "wireguard", Tier: "app", Gateway: name + ".example.test", Address: []string{"10.9.0.2/32"},
		PeerKey: testPeer, PrivateKey: &key}
	if edit != nil {
		edit(&p)
	}
	out, err := v.SaveVPN(p)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// vpnEndpoint stands in for a connected helper's SOCKS5 endpoint: it records
// every address it is asked for and carries each one to `to`.
type vpnEndpoint struct {
	port int
	mu   sync.Mutex
	seen []string
}

func newVPNEndpoint(t *testing.T, to string) *vpnEndpoint {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	e := &vpnEndpoint{port: ln.Addr().(*net.TCPAddr).Port}
	go (&socks.Server{User: "vu", Pass: "vp", Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
		e.mu.Lock()
		e.seen = append(e.seen, addr)
		e.mu.Unlock()
		return (&net.Dialer{}).DialContext(ctx, network, to)
	}}).Serve(ln)
	return e
}

func (e *vpnEndpoint) asked() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.seen)
}

// up marks profile id connected on e with routes, as the helper's connected
// message would.
func up(v *VPNService, id string, e *vpnEndpoint, routes ...string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.runs[id] = &vpnRun{stop: make(chan struct{}), att: &vpnAttempt{user: "vu", pass: "vp", port: e.port}}
	v.set(VPNState{ID: id, Status: VPNConnected, Routes: routes})
}

func vpnCode(t *testing.T, err error, code apperr.Code, vpnID string) *apperr.Error {
	t.Helper()
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != code || (vpnID != "" && ae.Details["vpnId"] != vpnID) {
		t.Fatalf("err = %v (%+v), want %s for %s", err, ae, code, vpnID)
	}
	return ae
}

// AE1, R16: a host whose profile cannot connect, was deleted or waits for
// confirmation fails with VPNFailed naming the profile, and its server
// never sees a direct connection.
func TestVPNHostThatCannotConnectNeverDialsDirectly(t *testing.T) {
	srv := newCountingServer(t)
	c, s, _ := vpnConn(t, vpntest.Down, srv.knownLine())
	p := saveProfile(t, c.creds.vpns, "office", nil)
	form := srv.form("pw")
	form.VPNID = &p.ID
	h, err := s.SaveHost(form)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Connect(context.Background(), h.ID)
	vpnCode(t, err, apperr.VPNFailed, p.ID)
	if !strings.Contains(err.Error(), "VPN office could not connect") || !strings.Contains(err.Error(), "gateway did not answer") {
		t.Fatalf("err = %v, want the profile and the reason", err)
	}
	form.ID = h.ID
	if r, err := c.TestConnection(context.Background(), form); err != nil || r.Step != "vpn" {
		t.Fatalf("test connection = %+v, %v", r, err)
	}

	if err := c.creds.vpns.DeleteVPN(p.ID); err != nil {
		t.Fatal(err)
	}
	_, err = c.Connect(context.Background(), h.ID)
	if ae := vpnCode(t, err, apperr.VPNFailed, p.ID); ae.Details["reason"] != "missing" {
		t.Fatalf("deleted profile: %+v", ae)
	}
	if err := c.creds.st.Edit(func(d *store.Data) error {
		return d.EditHost(h.ID, func(h *store.Host) { h.VPNID = new(store.HeldVPNID) })
	}); err != nil {
		t.Fatal(err)
	}
	_, err = c.Connect(context.Background(), h.ID)
	if ae := vpnCode(t, err, apperr.VPNFailed, ""); ae.Details["reason"] != "held" {
		t.Fatalf("held profile: %+v", ae)
	}
	if n := srv.conns.Load(); n != 0 {
		t.Fatalf("the server saw %d direct connections", n)
	}
}

// R15: connecting from the UI brings a host's profile up first, then dials
// through it.
func TestConnectBringsTheHostsVPNUpFirst(t *testing.T) {
	srv := newCountingServer(t)
	c, s, f := vpnConn(t, vpntest.OK, srv.knownLine())
	helper := proxytest.SOCKS5(t, "", "")
	f.SetSocksPort(t, helper.Port)
	p := saveProfile(t, c.creds.vpns, "office", nil)
	form := srv.form("pw")
	form.VPNID = &p.ID
	h, err := s.SaveHost(form)
	if err != nil {
		t.Fatal(err)
	}
	id, err := c.Connect(context.Background(), h.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Disconnect(id)
	want := net.JoinHostPort(srv.host, strconv.Itoa(srv.port))
	if !slices.Equal(helper.Targets(), []string{want}) || len(f.Starts(t)) != 1 {
		t.Fatalf("helper carried %v after %d starts, want %s", helper.Targets(), len(f.Starts(t)), want)
	}

	// Reconnecting a dropped session brings its dropped VPN up again.
	c.creds.vpns.DisconnectVPN(p.ID)
	c.mgr.Lease(id).Client().Close()
	if err := c.Reconnect(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(helper.Targets(), []string{want, want}) || len(f.Starts(t)) != 2 {
		t.Fatalf("after the reconnect the helper carried %v after %d starts", helper.Targets(), len(f.Starts(t)))
	}
}

// KTD6, R37: an agent's command on a host whose profile is down fails with
// VPNDown naming the profile and does not start it.
func TestAgentDialNeverStartsTheVPN(t *testing.T) {
	srv := newCountingServer(t)
	c, s, f := vpnConn(t, vpntest.OK, srv.knownLine())
	p := saveProfile(t, c.creds.vpns, "office", nil)
	form := srv.form("pw")
	form.VPNID = &p.ID
	h, err := s.SaveHost(form)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.promptlessLease(context.Background(), h.ID)
	vpnCode(t, err, apperr.VPNDown, p.ID)
	if !strings.Contains(err.Error(), "office") || len(f.Starts(t)) != 0 || len(c.creds.vpns.VPNStates()) != 0 || srv.conns.Load() != 0 {
		t.Fatalf("err = %v, starts %d, states %v, direct %d", err, len(f.Starts(t)), c.creds.vpns.VPNStates(), srv.conns.Load())
	}
}

// AE2, R17: an unbound host inside the routes of exactly one connected
// app-scoped profile dials through it; inside two, it fails naming both.
func TestUnboundHostDialsThroughTheOneVPNThatRoutesIt(t *testing.T) {
	srv := newCountingServer(t)
	at := net.JoinHostPort(srv.host, strconv.Itoa(srv.port))
	c, s, _ := vpnConn(t, vpntest.OK, strings.Replace(srv.knownLine(), "["+srv.host+"]", "[10.1.2.3]", 1))
	a := saveProfile(t, c.creds.vpns, "alpha", nil)
	ea := newVPNEndpoint(t, at)
	up(c.creds.vpns, a.ID, ea, "10.1.0.0/16")
	form := srv.form("pw")
	form.Hostname = "10.1.2.3"
	h, err := s.SaveHost(form)
	if err != nil {
		t.Fatal(err)
	}
	id, err := c.Connect(context.Background(), h.ID)
	if err != nil {
		t.Fatal(err)
	}
	c.Disconnect(id)
	if want := "10.1.2.3:" + strconv.Itoa(srv.port); !slices.Equal(ea.asked(), []string{want}) {
		t.Fatalf("alpha carried %v, want %s", ea.asked(), want)
	}

	b := saveProfile(t, c.creds.vpns, "bravo", nil)
	eb := newVPNEndpoint(t, at)
	up(c.creds.vpns, b.ID, eb, "10.1.0.0/16")
	before := srv.conns.Load()
	_, err = c.Connect(context.Background(), h.ID)
	vpnCode(t, err, apperr.VPNAmbiguous, "")
	if !strings.Contains(err.Error(), "alpha") || !strings.Contains(err.Error(), "bravo") {
		t.Fatalf("err = %v, want both profiles named", err)
	}
	if len(ea.asked()) != 1 || len(eb.asked()) != 0 || srv.conns.Load() != before {
		t.Fatal("an ambiguous host was dialled")
	}
}

// R17 never matches default or half-internet routes, and matches a host
// name only by the profile's DNS domains.
func TestRouteMatchingSkipsDefaultRoutesAndOtherNames(t *testing.T) {
	c, s, _ := vpnConn(t, vpntest.OK)
	p := saveProfile(t, c.creds.vpns, "office", func(v *VPN) { v.DNSDomains = []string{"corp.example"} })
	up(c.creds.vpns, p.ID, newVPNEndpoint(t, "127.0.0.1:1"), "0.0.0.0/1", "128.0.0.0/1", "::/0")
	for name, want := range map[string]string{"10.1.2.3": "", "db.other.example": "", "corp.example.evil": "", "db.corp.example": p.ID, "DB.Corp.Example.": p.ID} {
		h, err := s.SaveHost(Host{Alias: name, Hostname: name, Port: 22, User: "u"})
		if err != nil {
			t.Fatal(err)
		}
		tg, err := c.target(h.ID)
		if err != nil {
			t.Fatal(err)
		}
		got := ""
		if px := tg.Hops[0].Proxy; px != nil {
			got = px.VPN
		}
		if got != want {
			t.Errorf("%s dials through %q, want %q", name, got, want)
		}
	}
}

// KTD5: a host names a VPN or a proxy, never both, at save and at connect.
func TestVPNAndProxyAreRefusedTogether(t *testing.T) {
	srv := newCountingServer(t)
	c, s, _ := vpnConn(t, vpntest.OK, srv.knownLine())
	p := saveProfile(t, c.creds.vpns, "office", nil)
	px, err := s.SaveProxy(Proxy{Name: "proxy", Type: "socks5", Host: "127.0.0.1", Port: 1})
	if err != nil {
		t.Fatal(err)
	}
	form := srv.form("pw")
	form.VPNID, form.ProxyID = &p.ID, &px.ID
	if _, err := s.SaveHost(form); err == nil {
		t.Fatal("a host with a VPN and a proxy was saved")
	}
	form.ProxyID = nil
	h, err := s.SaveHost(form)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.creds.st.Edit(func(d *store.Data) error {
		return d.EditHost(h.ID, func(h *store.Host) { h.ProxyID = &px.ID })
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Connect(context.Background(), h.ID); err == nil || !strings.Contains(err.Error(), "proxy") {
		t.Fatalf("err = %v", err)
	}
	if srv.conns.Load() != 0 || len(c.creds.vpns.VPNStates()) != 0 {
		t.Fatal("a host with a VPN and a proxy was dialled")
	}
}

// KTD5: a VPN carries a jump chain's first hop, whichever hop names it; a
// chain whose hops name different VPNs is refused at save and connect.
func TestJumpChainGoesThroughOneVPN(t *testing.T) {
	srv := newCountingServer(t)
	c, s, _ := vpnConn(t, vpntest.OK, srv.knownLine())
	p := saveProfile(t, c.creds.vpns, "office", nil)
	e := newVPNEndpoint(t, net.JoinHostPort(srv.host, strconv.Itoa(srv.port)))
	up(c.creds.vpns, p.ID, e)
	jump, err := s.SaveHost(srv.form("pw"))
	if err != nil {
		t.Fatal(err)
	}
	inner := Host{Alias: "inner", Hostname: "inner.lan", Port: 22, User: "u", AuthType: new("password"), Password: new("pw"), JumpHostID: &jump.ID, VPNID: &p.ID}
	h, err := s.SaveHost(inner)
	if err != nil {
		t.Fatal(err)
	}
	c.Connect(context.Background(), h.ID) // the bastion refuses the inner hop's channel
	if want := net.JoinHostPort(srv.host, strconv.Itoa(srv.port)); !slices.Equal(e.asked(), []string{want}) || srv.conns.Load() != 1 {
		t.Fatalf("the VPN carried %v; the bastion saw %d connections", e.asked(), srv.conns.Load())
	}

	other := saveProfile(t, c.creds.vpns, "lab", nil)
	jump.VPNID = &other.ID
	if _, err := s.SaveHost(jump); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Connect(context.Background(), h.ID); err == nil || !strings.Contains(err.Error(), "VPN") {
		t.Fatalf("err = %v, want the second VPN refused", err)
	}
	inner.ID = h.ID
	if _, err := s.SaveHost(inner); err == nil {
		t.Fatal("a host behind a jump host on another VPN was saved")
	}
	if len(e.asked()) != 1 || srv.conns.Load() != 1 {
		t.Fatal("a chain on two VPNs was dialled")
	}
}

// KTD5: editing a profile opens a fresh shared connection.
func TestEditingTheVPNChangesTheConnectionKey(t *testing.T) {
	c, s, _ := vpnConn(t, vpntest.OK)
	p := saveProfile(t, c.creds.vpns, "office", nil)
	h, err := s.SaveHost(Host{Alias: "web", Hostname: "web.lan", Port: 22, User: "u", VPNID: &p.ID})
	if err != nil {
		t.Fatal(err)
	}
	before, err := c.target(h.ID)
	if err != nil {
		t.Fatal(err)
	}
	p.MTU = 1400
	if _, err := c.creds.vpns.SaveVPN(p); err != nil {
		t.Fatal(err)
	}
	after, err := c.target(h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(before.Key, "~vpn:"+p.ID+"@") || before.Key == after.Key {
		t.Fatalf("keys %q and %q", before.Key, after.Key)
	}
}

// KTD2: a shared connection rebuilt after its VPN reconnected on a new port
// dials the new one.
func TestReconnectDialsTheVPNsNewPort(t *testing.T) {
	srv := newCountingServer(t)
	at := net.JoinHostPort(srv.host, strconv.Itoa(srv.port))
	c, s, _ := vpnConn(t, vpntest.OK, srv.knownLine())
	p := saveProfile(t, c.creds.vpns, "office", nil)
	first := newVPNEndpoint(t, at)
	up(c.creds.vpns, p.ID, first)
	form := srv.form("pw")
	form.VPNID = &p.ID
	h, err := s.SaveHost(form)
	if err != nil {
		t.Fatal(err)
	}
	id, err := c.Connect(context.Background(), h.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Disconnect(id)
	second := newVPNEndpoint(t, at)
	up(c.creds.vpns, p.ID, second)
	c.mgr.Lease(id).Client().Close()
	if err := c.Reconnect(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if len(first.asked()) != 1 || len(second.asked()) != 1 {
		t.Fatalf("first carried %v, second %v", first.asked(), second.asked())
	}
}

// KTD5: a host bound to a system-wide profile that cannot connect (no
// helper) or an external one that is down sends nothing.
func TestHostOnASystemWideVPNSendsNothing(t *testing.T) {
	srv := newCountingServer(t)
	c, s, f := vpnConn(t, vpntest.OK, srv.knownLine())
	// An external profile whose tunnel detection does not see is down.
	for tier, code := range map[string]apperr.Code{store.VPNTierSystem: apperr.VPNFailed, store.VPNTierExternal: apperr.VPNDown} {
		p := saveProfile(t, c.creds.vpns, tier, func(v *VPN) { v.Tier = tier })
		form := srv.form("pw")
		form.VPNID = &p.ID
		h, err := s.SaveHost(form)
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Connect(context.Background(), h.ID)
		vpnCode(t, err, code, p.ID)
	}
	if srv.conns.Load() != 0 || len(f.Starts(t)) != 0 {
		t.Fatal("a host on a system-wide or external VPN was dialled")
	}
}

// A host on an external profile dials with its socket pinned to the
// vendor tunnel's interface, looked up at each dial (KTD7): no tunnel is
// VPNDown, a tunnel whose interface is gone is VPNFailed, both naming the
// profile, and the server sees a connection only through the tunnel.
func TestExternalHostDialsThroughItsTunnelOnly(t *testing.T) {
	srv := newCountingServer(t)
	c, s, f := vpnConn(t, vpntest.OK, srv.knownLine())
	fd := &fakeDetect{}
	c.creds.vpns.detect = fd.detect
	p := saveProfile(t, c.creds.vpns, "Loop VPN", func(v *VPN) { v.Tier, v.Type, v.Gateway = store.VPNTierExternal, "other", "" })
	form := srv.form("pw")
	form.VPNID = &p.ID
	h, err := s.SaveHost(form)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, err = c.Connect(ctx, h.ID)
	vpnCode(t, err, apperr.VPNDown, p.ID)

	fd.set(detect.Snapshot{Tunnels: []detect.Tunnel{{Iface: "hostbay-none0", Local: "10.0.0.1", VPN: "Loop VPN"}}})
	_, err = c.Connect(ctx, h.ID)
	if ae := vpnCode(t, err, apperr.VPNFailed, p.ID); !strings.Contains(ae.Message, "Loop VPN") {
		t.Fatalf("message %q does not name the profile", ae.Message)
	}
	if srv.conns.Load() != 0 {
		t.Fatal("the server was dialled with no tunnel")
	}

	ifs, _ := net.Interfaces()
	lo := ifs[slices.IndexFunc(ifs, func(i net.Interface) bool { return i.Flags&net.FlagLoopback != 0 })].Name
	fd.set(detect.Snapshot{Tunnels: []detect.Tunnel{{Iface: lo, Local: "127.0.0.1", VPN: "Loop VPN"}}})
	if _, err := c.Connect(ctx, h.ID); err != nil {
		t.Fatal(err)
	}
	if srv.conns.Load() != 1 || len(f.Starts(t)) != 0 {
		t.Fatalf("server saw %d connections, helper started %d times", srv.conns.Load(), len(f.Starts(t)))
	}
}

// Raw TCP, Test Connection, RDP and file hosts dial through the host's VPN
// too (R15).
func TestEveryHostKindDialsThroughItsVPN(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
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
			c.Close()
		}
	}()
	c, s, _ := vpnConn(t, vpntest.OK)
	p := saveProfile(t, c.creds.vpns, "office", nil)
	e := newVPNEndpoint(t, ln.Addr().String())
	up(c.creds.vpns, p.ID, e)

	tcp := "tcp"
	h, err := s.SaveHost(Host{Alias: "dev", Hostname: "dev.lan", Port: 7000, User: "u", Kind: &tcp, VPNID: &p.ID})
	if err != nil {
		t.Fatal(err)
	}
	pty, err := c.PtyOpenTCP(context.Background(), "owner", h.ID)
	if err != nil {
		t.Fatal(err)
	}
	c.PtyClose(pty)
	if r, err := c.TestConnection(context.Background(), toUI(*mustHost(t, c, h.ID))); err != nil || r.Step != "ok" {
		t.Fatalf("test connection = %+v, %v", r, err)
	}

	rdpKind := "rdp"
	win, err := s.SaveHost(Host{Alias: "win", Hostname: "win.lan", User: "me", Kind: &rdpKind, VPNID: &p.ID})
	if err != nil {
		t.Fatal(err)
	}
	rs := NewRdpService(c.creds, c, tlstrust.New(t.TempDir()))
	dialHost, dialPort, release, err := rs.dialAddr(context.Background(), *mustHost(t, c, win.ID), 3389)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if dialHost != "127.0.0.1" {
		t.Fatalf("RDP dials %s directly", dialHost)
	}
	fc, err := net.Dial("tcp", net.JoinHostPort(dialHost, strconv.Itoa(dialPort)))
	if err != nil {
		t.Fatal(err)
	}
	fc.Read(make([]byte, 1))
	fc.Close()

	srv, _, _ := davHost(t, nil)
	_, davPort, _ := net.SplitHostPort(srv.Listener.Addr().String())
	ts := NewTransferService(NewFilesService(c.mgr), c.creds, tlstrust.New(t.TempDir()))
	e2 := newVPNEndpoint(t, srv.Listener.Addr().String())
	up(c.creds.vpns, p.ID, e2)
	kind, url, pw := "webdav", "http://dav.corp.example:"+davPort+"/", "pw"
	dav, err := s.SaveHost(Host{Alias: "dav", User: "u", Kind: &kind, URL: &url, Password: &pw, VPNID: &p.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.TransferConnect(context.Background(), dav.ID); err != nil {
		t.Fatal(err)
	}

	want := []string{"dev.lan:7000", "dev.lan:7000", "win.lan:3389"}
	if got := e.asked(); !slices.Equal(got, want) {
		t.Fatalf("the VPN carried %v, want %v", got, want)
	}
	if got := e2.asked(); len(got) == 0 || got[0] != "dav.corp.example:"+davPort {
		t.Fatalf("the file host's VPN carried %v", got)
	}
}

func mustHost(t *testing.T, c *ConnService, id string) *store.Host {
	t.Helper()
	h, err := c.host(id)
	if err != nil {
		t.Fatal(err)
	}
	return h
}
