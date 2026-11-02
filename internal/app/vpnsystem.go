package app

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"hostbay/internal/apperr"
	"hostbay/internal/netd"
	"hostbay/internal/store"
	"hostbay/internal/vpn/openvpn"
	"hostbay/internal/vpn/osvpn"
	"hostbay/internal/vpn/proto"
)

// System-wide profiles (U12) run through hostbay-netd: OpenVPN, WireGuard,
// and on Windows the OpenConnect types, which have no unprivileged mode
// there. IKEv2 and L2TP ("ipsec") run through the operating system's own
// VPN service. Their states reach the profile through report, as an
// app-scoped helper's do, so connecting, reconnecting and disconnecting
// work the same.

// vpnGOOS is the system whose rules vpnTier follows; tests change it.
var vpnGOOS = runtime.GOOS

// vpnTier is the tier v runs in: its own, except that an OpenConnect type
// on Windows runs system-wide (R2).
func vpnTier(v store.VPN) string {
	if v.Tier == store.VPNTierApp && vpnGOOS == "windows" && proto.IsOpenConnect(v.Type) {
		return store.VPNTierSystem
	}
	return v.Tier
}

// tunnelClient is the part of *netd.Client that runs system-wide tunnels.
type tunnelClient interface {
	Tunnel(ctx context.Context, m netd.Msg, on func(netd.Msg)) (netd.Msg, error)
	StopTunnel(ctx context.Context, h string) error
	Done() <-chan struct{}
}

// osVPN drives the operating system's VPN service; tests use a fake.
type osVPN struct {
	connect, disconnect func(ctx context.Context, name string) error
	state               func(ctx context.Context, name string) (osvpn.Status, error)
}

var systemOSVPN = osVPN{connect: osvpn.Connect, disconnect: osvpn.Disconnect, state: osvpn.State}

var (
	// osVPNPoll is how often an OS VPN service's state is read.
	osVPNPoll = time.Second
	// tunnelStartWait bounds a start request to the helper.
	tunnelStartWait = 30 * time.Second
	// openvpnConnectWait is how long a system-wide OpenVPN may take to
	// connect before Hostbay gives up on it.
	openvpnConnectWait = 90 * time.Second
	// osVPNCallWait bounds each state and disconnect call to the OS VPN
	// service, so a hung one cannot hold up disconnect, delete or quit.
	osVPNCallWait = 10 * time.Second
)

// systemRun is one system-wide run: Close ends it without waiting, and Done
// is closed once it has ended.
type systemRun interface {
	Close()
	Done() <-chan struct{}
}

// launchSystem starts v's system-wide run.
func (s *VPNService) launchSystem(id string, r *vpnRun, a *vpnAttempt, v *store.VPN) (<-chan struct{}, error) {
	if err := checkSaved(v); err != nil {
		return nil, err
	}
	sec, err := s.openSecrets(v)
	if err != nil {
		return nil, err
	}
	rep := func(m proto.Msg) { s.report(id, r, a, m) }
	up := func(iface string, addrs []string) {
		s.mu.Lock()
		a.iface = iface
		s.mu.Unlock()
		rep(proto.Msg{T: proto.State, State: proto.Connected, Address: addrs, Routes: v.Routes})
	}
	var run systemRun
	if v.Type == proto.TypeIPsec {
		run, err = s.startOSVPN(v, r.stop, up, rep)
	} else {
		run, err = s.startTunnel(v, sec, up, rep)
	}
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	a.sys = run
	stopped := s.runs[id] != r
	s.mu.Unlock()
	if stopped {
		run.Close()
	}
	return run.Done(), nil
}

// ended reports a run's end: dropped when it had been up, so it may
// reconnect, failed otherwise.
func ended(rep func(proto.Msg), wasUp bool, reason string) {
	state := proto.Failed
	if wasUp {
		state = proto.Dropped
	}
	rep(proto.Msg{T: proto.State, State: state, Reason: reason})
}

// netdTunnel is a tunnel hostbay-netd runs.
type netdTunnel struct {
	c      tunnelClient
	handle string
	up     func(iface string, addrs []string)
	rep    func(proto.Msg)

	mu    sync.Mutex
	wasUp bool
	mgmt  net.Conn // OpenVPN's management interface, once dialed

	closing, done chan struct{}
	closeOnce     sync.Once
	endOnce       sync.Once
	codes         chan string // the one-time code a static-challenge profile waits for
}

// Answer gives the one-time code a signing-in OpenVPN profile waits for.
func (t *netdTunnel) Answer(code string) error {
	select {
	case t.codes <- code:
		return nil
	default:
		return errors.New("this VPN profile is not waiting for a code")
	}
}

// askCode reports that the profile needs a one-time code and waits for it.
func (t *netdTunnel) askCode() (string, error) {
	t.rep(proto.Msg{T: proto.Need, Need: proto.NeedOTP})
	select {
	case code := <-t.codes:
		t.rep(proto.Msg{T: proto.State, State: proto.Connecting})
		return code, nil
	case <-t.closing:
		return "", errors.New("disconnected")
	}
}

// startTunnel asks hostbay-netd to start v's tunnel. OpenVPN's states and
// sign-in then come from its management interface, the others' from the
// helper's tunnel events.
func (s *VPNService) startTunnel(v *store.VPN, sec store.VPNSecrets, up func(string, []string), rep func(proto.Msg)) (systemRun, error) {
	var c tunnelClient
	if s.tunnels != nil {
		c = s.tunnels()
	}
	if c == nil {
		// VPNFailed, not HelperMissing, whose UI speaks of Remote Desktop.
		return nil, apperr.New(apperr.VPNFailed, fmt.Sprintf("VPN %s connects system-wide through Hostbay's network helper, which is not running; install it in Settings, Routing", v.Name)).
			With("vpnId", v.ID).With("reason", "helper")
	}
	m := netd.Msg{Tunnel: rand.Text()[:16]}
	switch {
	case v.Type == proto.TypeOpenVPN:
		m.T, m.OpenVPN = netd.OpenVPNStart, &openvpn.Profile{Remote: v.Gateway, Port: v.Port, Routes: v.Routes, MTU: v.MTU,
			UserPass: v.Auth == "password", Extra: sec.OpenVPN, Key: sec.PrivateKey}
	case v.Type == proto.TypeWireGuard:
		m.T, m.WireGuard = netd.WireGuardStart, &netd.WireGuard{Conf: wireGuardConfig(v), PrivateKey: sec.PrivateKey, PresharedKey: sec.PresharedKey}
	case proto.IsOpenConnect(v.Type) && v.Auth != "certificate":
		b, _ := json.Marshal(openConnectConfig{Gateway: openConnectGateway(v.Gateway, v.Port), User: v.User, ServerCert: v.PeerKey, AuthGroup: v.AuthGroup})
		m.T, m.OpenConnect = netd.OpenConnectStart, &netd.OpenConnect{Type: v.Type, Config: string(b), Password: sec.Password}
	default:
		return nil, apperr.New(apperr.VPNFailed, fmt.Sprintf("Hostbay cannot connect %s profiles system-wide", v.Type)).
			With("vpnId", v.ID).With("reason", "unsupported")
	}
	t := &netdTunnel{c: c, handle: m.Tunnel, up: up, rep: rep, closing: make(chan struct{}), done: make(chan struct{}), codes: make(chan string, 1)}
	rep(proto.Msg{T: proto.State, State: proto.Connecting})
	ctx, cancel := context.WithTimeout(context.Background(), tunnelStartWait)
	defer cancel()
	reply, err := c.Tunnel(ctx, m, t.event)
	if err != nil {
		return nil, err
	}
	go func() { // the helper stops every tunnel when its connection ends
		select {
		case <-c.Done():
			t.end("Hostbay's network helper stopped.")
		case <-t.done:
		}
	}()
	if m.OpenVPN != nil {
		go t.manage(reply, v.User, sec.Password)
	}
	return t, nil
}

// event applies the helper's tunnel events.
func (t *netdTunnel) event(m netd.Msg) {
	switch m.State {
	case netd.TunnelUp:
		t.mu.Lock()
		t.wasUp = true
		t.mu.Unlock()
		t.up(m.Iface, m.Address)
	case netd.TunnelEnded:
		t.end(m.Reason)
	}
}

// manage follows OpenVPN through its management interface and gives it the
// user name and password, which never pass through the helper.
func (t *netdTunnel) manage(r netd.Msg, user, pass string) {
	network, err := mgmtNetwork(vpnGOOS, r.Mgmt)
	if err != nil {
		t.end(err.Error())
		return
	}
	conn, err := net.DialTimeout(network, r.Mgmt, 10*time.Second)
	if err != nil {
		t.end("Hostbay could not reach OpenVPN: " + err.Error())
		return
	}
	t.mu.Lock()
	t.mgmt = conn
	t.mu.Unlock()
	reason := "OpenVPN stopped."
	// A server that never answers (a wrong address, say) leaves OpenVPN
	// retrying for ever; the profile would stay "connecting".
	giveUp := time.AfterFunc(openvpnConnectWait, func() {
		t.end("OpenVPN did not connect in " + openvpnConnectWait.String() + ". Check the server address and your network.")
	})
	defer giveUp.Stop()
	err = openvpn.ManageOTP(conn, user, pass, r.MgmtPass, t.askCode, func(e openvpn.Event) {
		switch e.State {
		case openvpn.Connected:
			giveUp.Stop()
			t.mu.Lock()
			t.wasUp = true
			t.mu.Unlock()
			addrs := []string{}
			if a, err := netip.ParseAddr(e.Local); err == nil {
				addrs = append(addrs, netip.PrefixFrom(a, a.BitLen()).String())
			}
			t.up(ifaceWith(e.Local), addrs)
		case openvpn.Connecting:
			t.rep(proto.Msg{T: proto.State, State: proto.Connecting})
		case openvpn.Failed:
			reason = e.Reason
		case openvpn.Exiting:
			// It only repeats the cause as a raw token ("auth-failure").
			if reason == "OpenVPN stopped." {
				reason = e.Reason
			}
		}
	})
	conn.Close()
	if err != nil {
		reason = err.Error()
	}
	t.end(reason)
}

// mgmtNetwork is the network to reach OpenVPN's management interface at
// addr, in the only shape the helper answers with on goos: a loopback TCP
// port on Windows, an absolute socket path elsewhere. The helper's reply
// chooses where the user name and password go, so anything else is refused.
// shortcut: the socket path is not pinned to the helper's run folder, whose
// location netd does not export; pin it if netd ever exports its folder.
func mgmtNetwork(goos, addr string) (string, error) {
	if goos == "windows" {
		if ap, err := netip.ParseAddrPort(addr); err == nil && ap.Addr().IsLoopback() && ap.Port() != 0 {
			return "tcp", nil
		}
	} else if filepath.IsAbs(addr) && filepath.Clean(addr) == addr && strings.HasSuffix(addr, ".sock") {
		return "unix", nil
	}
	return "", fmt.Errorf("Hostbay's network helper named an OpenVPN management address it does not use (%q), so Hostbay refused it", addr)
}

// ifaceWith is the interface that holds address a, which OpenVPN reports
// for its tunnel instead of the interface's name.
func ifaceWith(a string) string {
	ip, err := netip.ParseAddr(a)
	if err != nil {
		return ""
	}
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		addrs, _ := i.Addrs()
		for _, ad := range addrs {
			if n, ok := ad.(*net.IPNet); ok {
				if p, ok := netip.AddrFromSlice(n.IP); ok && p.Unmap() == ip {
					return i.Name
				}
			}
		}
	}
	return ""
}

// end reports the run's end once.
func (t *netdTunnel) end(reason string) {
	t.endOnce.Do(func() {
		t.mu.Lock()
		up := t.wasUp
		t.mu.Unlock()
		select {
		case <-t.closing: // disconnected: nothing to report
		default:
			ended(t.rep, up, reason)
		}
		close(t.done)
	})
}

// Close asks OpenVPN to exit through its management interface, so it
// removes its routes itself, then has the helper stop the tunnel. It
// returns at once: it is called with the service's lock held.
func (t *netdTunnel) Close() {
	t.closeOnce.Do(func() {
		close(t.closing)
		go func() {
			t.mu.Lock()
			if t.mgmt != nil {
				t.mgmt.SetWriteDeadline(time.Now().Add(time.Second))
				t.mgmt.Write([]byte("signal SIGTERM\n"))
			}
			t.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			t.c.StopTunnel(ctx, t.handle)
			t.end("")
		}()
	})
}

func (t *netdTunnel) Done() <-chan struct{} { return t.done }

// osTunnel is an IKEv2 or L2TP profile run by the system's VPN service,
// under the name the profile's client field gives, else its own name.
type osTunnel struct {
	stop, done chan struct{}
	once       sync.Once
}

// startOSVPN connects v through the OS VPN service; the connect gives up
// after vpnConnectWait, or once stop closes.
func (s *VPNService) startOSVPN(v *store.VPN, stop <-chan struct{}, up func(string, []string), rep func(proto.Msg)) (systemRun, error) {
	name := v.Client
	if name == "" {
		name = v.Name
	}
	rep(proto.Msg{T: proto.State, State: proto.Connecting})
	ctx, cancel := context.WithTimeout(context.Background(), vpnConnectWait)
	defer cancel()
	go func() {
		select {
		case <-stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	if err := s.osvpn.connect(ctx, name); err != nil {
		return nil, err
	}
	disconnect := func() {
		ctx, cancel := context.WithTimeout(context.Background(), osVPNCallWait)
		defer cancel()
		s.osvpn.disconnect(ctx, name)
	}
	state := func() (osvpn.Status, error) {
		ctx, cancel := context.WithTimeout(context.Background(), osVPNCallWait)
		defer cancel()
		return s.osvpn.state(ctx, name)
	}
	t := &osTunnel{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(t.done)
		started, wasUp := time.Now(), false
		unreadable := 0 // reads in a row that failed while up; one slow scutil or nmcli is no drop
		for {
			select {
			case <-t.stop:
				disconnect()
				return
			case <-time.After(osVPNPoll):
			}
			st, err := state()
			switch {
			case err == nil && st.Connected && !wasUp:
				wasUp = true
				up(st.Iface, nil)
			case err == nil && st.Connected:
				unreadable = 0
			case wasUp && err != nil && unreadable < 2:
				unreadable++
			case wasUp:
				ended(rep, true, fmt.Sprintf("The system disconnected VPN %s.", name))
				return
			case err != nil || time.Since(started) > vpnConnectWait:
				disconnect()
				reason := fmt.Sprintf("VPN %s did not connect in the system's VPN settings.", name)
				if err != nil {
					reason = err.Error()
				}
				ended(rep, false, reason)
				return
			}
		}
	}()
	return t, nil
}

func (t *osTunnel) Close()                { t.once.Do(func() { close(t.stop) }) }
func (t *osTunnel) Done() <-chan struct{} { return t.done }
