package app

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"hostbay/internal/apperr"
	"hostbay/internal/netd"
	"hostbay/internal/store"
	"hostbay/internal/vpn/osvpn"
	"hostbay/internal/vpn/vpntest"
)

// fakeTunnels is hostbay-netd's tunnel side: it records each start and stop
// and answers starts with reply.
type fakeTunnels struct {
	mu     sync.Mutex
	starts []netd.Msg
	stops  []string
	on     func(netd.Msg)
	done   chan struct{}
	reply  func(netd.Msg) (netd.Msg, error)
}

func useTunnels(s *VPNService) *fakeTunnels {
	f := &fakeTunnels{done: make(chan struct{})}
	s.tunnels = func() tunnelClient { return f }
	return f
}

func (f *fakeTunnels) Tunnel(_ context.Context, m netd.Msg, on func(netd.Msg)) (netd.Msg, error) {
	f.mu.Lock()
	f.starts, f.on = append(f.starts, m), on
	reply := f.reply
	f.mu.Unlock()
	if reply != nil {
		return reply(m)
	}
	return netd.Msg{}, nil
}

// StopTunnel ends the tunnel as the helper does: with its ended event.
func (f *fakeTunnels) StopTunnel(_ context.Context, h string) error {
	f.mu.Lock()
	f.stops = append(f.stops, h)
	on := f.on
	f.mu.Unlock()
	on(netd.Msg{Tunnel: h, State: netd.TunnelEnded})
	return nil
}

func (f *fakeTunnels) Done() <-chan struct{} { return f.done }

func (f *fakeTunnels) event(m netd.Msg) {
	f.mu.Lock()
	on := f.on
	f.mu.Unlock()
	on(m)
}

func (f *fakeTunnels) stopped() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.stops)
}

func loopback(t *testing.T) string {
	t.Helper()
	ifs, _ := net.Interfaces()
	i := slices.IndexFunc(ifs, func(i net.Interface) bool { return i.Flags&net.FlagLoopback != 0 })
	if i < 0 {
		t.Skip("no loopback interface")
	}
	return ifs[i].Name
}

// fakeManagement plays OpenVPN's management interface on a Unix socket:
// it releases on hold, asks for the password, reports connected at local,
// and records every command after that.
func fakeManagement(t *testing.T, local string) (netd.Msg, chan string, chan []string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "mg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "m.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	later, creds := make(chan string, 8), make(chan []string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		sc := bufio.NewScanner(c)
		read := func() string { sc.Scan(); return sc.Text() }
		say := func(s string) { c.Write([]byte(s + "\r\n")) }
		if read() != "state on" {
			return
		}
		say(">HOLD:Waiting for hold release:0")
		if read() != "hold release" {
			return
		}
		say(">PASSWORD:Need 'Auth' username/password")
		creds <- []string{read(), read()}
		say(">STATE:1700000001,CONNECTED,SUCCESS," + local + ",203.0.113.9,1194,,")
		for sc.Scan() {
			later <- sc.Text()
			if sc.Text() == "signal SIGTERM" {
				return
			}
		}
	}()
	return netd.Msg{Mgmt: sock}, later, creds
}

func saveOpenVPN(t *testing.T, s *VPNService) VPN {
	t.Helper()
	pw := "s3cret-pw"
	v, err := s.SaveVPN(VPN{Name: "Office OVPN", Type: "openvpn", Tier: store.VPNTierSystem, Gateway: "vpn.example.test", Port: 1194,
		User: "alice", Auth: "password", Routes: []string{"10.20.0.0/16"}, Password: &pw})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// An OpenVPN profile starts through the helper, which never sees the
// password: Hostbay gives it to the management interface once, and the
// profile is connected, on the interface holding OpenVPN's address, when
// OpenVPN says CONNECTED. Disconnecting asks OpenVPN to exit, then stops
// the tunnel.
func TestOpenVPNConnectsThroughTheHelperAndManagement(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	f := useTunnels(e.svc)
	reply, later, creds := fakeManagement(t, "127.0.0.1")
	f.reply = func(netd.Msg) (netd.Msg, error) { return reply, nil }
	v := saveOpenVPN(t, e.svc)

	st, err := e.svc.ConnectVPN(context.Background(), v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Iface != loopback(t) || !slices.Equal(st.Address, []string{"127.0.0.1/32"}) || !slices.Equal(st.Routes, []string{"10.20.0.0/16"}) {
		t.Fatalf("state %+v", st)
	}
	if got := <-creds; !slices.Equal(got, []string{`username "Auth" "alice"`, `password "Auth" "s3cret-pw"`}) {
		t.Fatalf("credentials sent %q", got)
	}
	f.mu.Lock()
	start := f.starts[0]
	f.mu.Unlock()
	b, _ := json.Marshal(start)
	if start.T != netd.OpenVPNStart || start.OpenVPN.Remote != "vpn.example.test" || !start.OpenVPN.UserPass ||
		!slices.Equal(start.OpenVPN.Routes, []string{"10.20.0.0/16"}) || strings.Contains(string(b), "s3cret-pw") {
		t.Fatalf("start %s", b)
	}

	if err := e.svc.DisconnectVPN(v.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case cmd := <-later:
		if cmd != "signal SIGTERM" {
			t.Fatalf("command %q", cmd)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OpenVPN was not asked to exit")
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(f.stopped()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !slices.Equal(f.stopped(), []string{start.Tunnel}) || stateOf(e.svc, v.ID).Status != VPNDisconnected {
		t.Fatalf("stopped %v, state %+v", f.stopped(), stateOf(e.svc, v.ID))
	}
}

// Without the helper a system-wide profile cannot connect, and says so.
func TestSystemWideNeedsTheHelper(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	v := saveOpenVPN(t, e.svc)
	_, err := e.svc.ConnectVPN(context.Background(), v.ID)
	if ae := vpnCode(t, err, apperr.VPNFailed, v.ID); ae.Details["reason"] != "helper" || !strings.Contains(ae.Message, "network helper") {
		t.Fatalf("err %+v", ae)
	}
	if n := len(e.fake.Starts(t)); n != 0 {
		t.Fatalf("an app-scoped helper started %d times", n)
	}
}

// A system-wide WireGuard profile is up when the helper says its first
// handshake came, on the helper's interface, and stays up while external
// profiles are relabeled; it drops when the tunnel ends or the helper goes.
func TestSystemWireGuardFollowsTheHelpersEvents(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	f := useTunnels(e.svc)
	v := e.save(t, false)
	key := testWGKey
	v.Tier, v.PrivateKey = store.VPNTierSystem, &key // secrets are sealed to the tier
	if _, err := e.svc.SaveVPN(v); err != nil {
		t.Fatal(err)
	}
	go func() {
		e.waitFor(t, v.ID, VPNConnecting)
		for {
			f.mu.Lock()
			n := len(f.starts)
			f.mu.Unlock()
			if n > 0 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		f.event(netd.Msg{State: netd.TunnelUp, Iface: "utun9", Address: []string{"10.9.0.2/32"}})
	}()
	st, err := e.svc.ConnectVPN(context.Background(), v.ID)
	if err != nil || st.Iface != "utun9" {
		t.Fatalf("state %+v, %v", st, err)
	}
	f.mu.Lock()
	start := f.starts[0]
	f.mu.Unlock()
	if start.T != netd.WireGuardStart || start.WireGuard.PrivateKey != testWGKey || !strings.Contains(start.WireGuard.Conf, "AllowedIPs = 10.9.0.0/24") {
		t.Fatalf("start %+v", start)
	}
	e.svc.refresh(context.Background()) // detection must not take the system-wide tunnel away
	if st := stateOf(e.svc, v.ID); st.Status != VPNConnected || st.Iface != "utun9" {
		t.Fatalf("after a refresh: %+v", st)
	}
	close(f.done) // the helper connection ended, and with it the tunnel
	e.waitFor(t, v.ID, VPNDropped)
}

// IKEv2 and L2TP connect through the system's own VPN service under the
// profile's name, and disconnect through it.
func TestIPsecUsesTheSystemsVPNService(t *testing.T) {
	old := osVPNPoll
	osVPNPoll = 10 * time.Millisecond
	t.Cleanup(func() { osVPNPoll = old })
	e := newVPNEnv(t, vpntest.OK)
	var mu sync.Mutex
	var calls []string
	polls := 0
	e.svc.osvpn = osVPN{
		connect: func(_ context.Context, n string) error {
			mu.Lock()
			calls = append(calls, "up "+n)
			mu.Unlock()
			return nil
		},
		disconnect: func(_ context.Context, n string) error {
			mu.Lock()
			calls = append(calls, "down "+n)
			mu.Unlock()
			return nil
		},
		state: func(context.Context, string) (osvpn.Status, error) {
			mu.Lock()
			defer mu.Unlock()
			polls++
			return osvpn.Status{Connected: polls > 1, Iface: "ipsec0"}, nil
		},
	}
	v, err := e.svc.SaveVPN(VPN{Name: "Branch IKEv2", Type: "ipsec", Tier: store.VPNTierSystem, Gateway: "ike.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	st, err := e.svc.ConnectVPN(context.Background(), v.ID)
	if err != nil || st.Iface != "ipsec0" {
		t.Fatalf("state %+v, %v", st, err)
	}
	e.svc.DisconnectVPN(v.ID)
	e.svc.waitExited(v.ID)
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(calls, []string{"up Branch IKEv2", "down Branch IKEv2"}) {
		t.Fatalf("calls %q", calls)
	}
}

// On Windows an OpenConnect profile runs system-wide (R2): through the
// helper, on its adapter.
func TestWindowsOpenConnectRunsSystemWide(t *testing.T) {
	old := vpnGOOS
	vpnGOOS = "windows"
	t.Cleanup(func() { vpnGOOS = old })
	e := newVPNEnv(t, vpntest.OK)
	f := useTunnels(e.svc)
	f.reply = func(m netd.Msg) (netd.Msg, error) {
		go f.event(netd.Msg{Tunnel: m.Tunnel, State: netd.TunnelUp, Iface: "Hostbay ABCDEFGH", Address: []string{"10.1.2.3"}})
		return netd.Msg{Iface: "Hostbay ABCDEFGH"}, nil
	}
	pw := "pw"
	v, err := e.svc.SaveVPN(VPN{Name: "AC", Type: "anyconnect", Tier: store.VPNTierApp, Gateway: "ac.example.test", User: "bob", Password: &pw})
	if err != nil {
		t.Fatal(err)
	}
	st, err := e.svc.ConnectVPN(context.Background(), v.ID)
	if err != nil || st.Iface != "Hostbay ABCDEFGH" {
		t.Fatalf("state %+v, %v", st, err)
	}
	f.mu.Lock()
	start := f.starts[0]
	f.mu.Unlock()
	if start.T != netd.OpenConnectStart || start.OpenConnect.Type != "anyconnect" || start.OpenConnect.Password != "pw" ||
		!strings.Contains(start.OpenConnect.Config, `"gateway":"ac.example.test"`) || len(e.fake.Starts(t)) != 0 {
		t.Fatalf("start %+v", start)
	}
}

// A host on a down system-wide profile connects it first (vpnUp), then
// dials directly once the system routes the host through its tunnel.
func TestHostOnASystemWideVPNConnectsItAndDialsThroughItsRoute(t *testing.T) {
	srv := newCountingServer(t)
	c, s, _ := vpnConn(t, vpntest.OK, srv.knownLine())
	f := useTunnels(c.creds.vpns)
	reply, _, _ := fakeManagement(t, "127.0.0.1") // the tunnel's address is loopback's, which routes the server
	f.reply = func(netd.Msg) (netd.Msg, error) { return reply, nil }
	v := saveOpenVPN(t, c.creds.vpns)
	form := srv.form("pw")
	form.VPNID = &v.ID
	h, err := s.SaveHost(form)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Connect(context.Background(), h.ID); err != nil {
		t.Fatal(err)
	}
	if srv.conns.Load() != 1 || stateOf(c.creds.vpns, v.ID).Status != VPNConnected {
		t.Fatalf("server saw %d connections, state %+v", srv.conns.Load(), stateOf(c.creds.vpns, v.ID))
	}
}

// systemRoute passes only when the system's route to the host leaves
// through the profile's interface.
func TestSystemRouteChecksTheInterface(t *testing.T) {
	ctx := context.Background()
	lo := loopback(t)
	if err := systemRoute(ctx, "v1", "Office", lo, "127.0.0.1:22"); err != nil {
		t.Fatal(err)
	}
	ifs, _ := net.Interfaces()
	other := ""
	for _, i := range ifs {
		if addrs, _ := i.Addrs(); i.Flags&net.FlagLoopback == 0 && len(addrs) > 0 {
			other = i.Name
			break
		}
	}
	for iface, reason := range map[string]string{"hostbay-none0": "route", other: "route"} {
		if iface == "" {
			continue
		}
		err := systemRoute(ctx, "v1", "Office", iface, "127.0.0.1:22")
		if ae := vpnCode(t, err, apperr.VPNFailed, "v1"); ae.Details["reason"] != reason {
			t.Errorf("%s: %+v", iface, ae)
		}
	}
}

// tunnelHelper is the fake helper with its tunnel side.
type tunnelHelper struct {
	*fakeHelper
	*fakeTunnels
}

func (h tunnelHelper) Done() <-chan struct{} { return h.fakeHelper.Done() }

// R29, U12 step 5: once a system-wide profile with auto-apply connects,
// its routes go through its tunnel by way of the helper.
func TestSystemWideRoutesApplyOnConnect(t *testing.T) {
	e := newRoutingEnv(t)
	ft := &fakeTunnels{}
	e.nd.dial = func(context.Context) (netdClient, error) {
		e.f.reopen()
		return tunnelHelper{e.f, ft}, nil
	}
	ft.reply = func(m netd.Msg) (netd.Msg, error) {
		go ft.event(netd.Msg{Tunnel: m.Tunnel, State: netd.TunnelUp, Iface: "utun6", Address: []string{"10.30.0.2/32"}})
		return netd.Msg{Iface: "utun6"}, nil
	}
	e.start(t)
	e.settle(t, 1) // Harmony SASE's route
	v := e.vpn.save(t, false)
	key := testWGKey
	v.Tier, v.PrivateKey, v.Routes, v.AutoApply = store.VPNTierSystem, &key, []string{"10.30.0.0/16"}, true
	if _, err := e.vpn.svc.SaveVPN(v); err != nil {
		t.Fatal(err)
	}
	if _, err := e.vpn.svc.ConnectVPN(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	e.settle(t, 2)
	if !e.has("10.30.0.0/16", "utun6") {
		rs, _ := e.f.Routes()
		t.Fatalf("routes %v", rs)
	}
}
