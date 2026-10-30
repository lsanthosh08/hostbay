// Package wg is hostbay-vpn's WireGuard engine (KTD3): wireguard-go over a
// gVisor netstack in the helper's own process, so it changes no system
// route, DNS setting or interface (R14). Only cmd/hostbay-vpn imports it,
// which keeps WireGuard and gVisor out of the main binary.
package wg

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"
	"golang.zx2c4.com/wireguard/tun/netstack"

	"hostbay/internal/vpn/proto"
	"hostbay/internal/vpn/stackdial"
)

var (
	// connectTimeout bounds the wait for the first handshake.
	connectTimeout = 30 * time.Second
	// dropAfter is how old the last handshake may get on a connected tunnel.
	// WireGuard rekeys every 2 minutes while traffic flows, and the 25 s
	// keepalive keeps it flowing, so this long without one means the peer is
	// gone (and its keys have expired: REJECT_AFTER_TIME is 180 s too).
	dropAfter = 180 * time.Second
	// pollEvery is how often the handshake time is read.
	pollEvery = time.Second
)

const (
	keepalive  = 25 // seconds, always: a silent tunnel would look dropped
	defaultMTU = 1420
)

type config struct {
	privateKey string // hex, as UAPI wants it
	addresses  []netip.Prefix
	dns        []netip.Addr
	search     []string
	mtu        int
	peers      []peer
}

type peer struct {
	publicKey, presharedKey string // hex
	endpoint                string // host:port as configured
	allowedIPs              []netip.Prefix
}

// Engine is one WireGuard tunnel. New prepares it, Run brings it up, and Dial
// reaches hosts through it.
type Engine struct {
	cfg *config
	tun tun.Device
	net *netstack.Net
}

// New parses a wg-quick .conf. secrets["privateKey"] and
// secrets["presharedKey"], when set, replace the .conf's keys, since Hostbay
// keeps them sealed apart from it. No error names a key's value.
func New(conf string, secrets map[string]string) (*Engine, error) {
	cfg, err := parse(conf, secrets)
	if err != nil {
		return nil, err
	}
	addrs := make([]netip.Addr, len(cfg.addresses))
	for i, p := range cfg.addresses {
		addrs[i] = p.Addr()
	}
	t, n, err := netstack.CreateNetTUN(addrs, cfg.dns, cfg.mtu)
	if err != nil {
		return nil, err
	}
	return &Engine{cfg: cfg, tun: t, net: n}, nil
}

// NewSystem is New for a system-wide tunnel (U12): the privileged helper
// gives create the tunnel's MTU and gets back a real TUN, whose addresses
// it sets from Addresses. Dial is not used: the system routes to the TUN.
func NewSystem(conf string, secrets map[string]string, create func(mtu int) (tun.Device, error)) (*Engine, error) {
	cfg, err := parse(conf, secrets)
	if err != nil {
		return nil, err
	}
	t, err := create(cfg.mtu)
	if err != nil {
		return nil, err
	}
	return &Engine{cfg: cfg, tun: t}, nil
}

// Addresses are the tunnel's own addresses.
func (e *Engine) Addresses() []netip.Prefix { return e.cfg.addresses }

func parse(conf string, secrets map[string]string) (*config, error) {
	cfg := &config{mtu: defaultMTU}
	section := ""
	sc := bufio.NewScanner(strings.NewReader(conf))
	for n := 1; sc.Scan(); n++ {
		line, _, _ := strings.Cut(sc.Text(), "#")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(line[1 : len(line)-1])
			if section == "peer" {
				cfg.peers = append(cfg.peers, peer{})
			}
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("Line %d of the WireGuard profile is not \"key = value\".", n)
		}
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
		var err error
		switch section + "." + k {
		case "interface.privatekey":
			cfg.privateKey, err = hexKey("PrivateKey", v)
		case "interface.address":
			cfg.addresses, err = prefixes("Address", v)
		case "interface.dns":
			// wg-quick's rule: addresses are servers, anything else a search domain.
			for _, s := range list(v) {
				if a, perr := netip.ParseAddr(s); perr == nil {
					cfg.dns = append(cfg.dns, a)
				} else {
					cfg.search = append(cfg.search, s)
				}
			}
		case "interface.mtu":
			cfg.mtu, err = strconv.Atoi(v)
			if err != nil || cfg.mtu < 576 || cfg.mtu > 65535 {
				err = fmt.Errorf("MTU %q is not a usable MTU.", v)
			}
		case "peer.publickey":
			cfg.peers[len(cfg.peers)-1].publicKey, err = hexKey("PublicKey", v)
		case "peer.presharedkey":
			cfg.peers[len(cfg.peers)-1].presharedKey, err = hexKey("PresharedKey", v)
		case "peer.endpoint":
			cfg.peers[len(cfg.peers)-1].endpoint = v
		case "peer.allowedips":
			cfg.peers[len(cfg.peers)-1].allowedIPs, err = prefixes("AllowedIPs", v)
		}
		// Anything else (ListenPort, PostUp, Table, PersistentKeepalive...) is
		// for wg-quick or fixed here, and nothing in it runs.
		if err != nil {
			return nil, err
		}
	}

	var err error
	if s := secrets["privateKey"]; s != "" {
		if cfg.privateKey, err = hexKey("The private key", s); err != nil {
			return nil, err
		}
	}
	if s := secrets["presharedKey"]; s != "" {
		psk, err := hexKey("The pre-shared key", s)
		if err != nil {
			return nil, err
		}
		// shortcut: one PSK for all peers, add per-peer secrets when a multi-peer profile needs them.
		for i := range cfg.peers {
			cfg.peers[i].presharedKey = psk
		}
	}

	switch {
	case cfg.privateKey == "":
		return nil, errors.New("The WireGuard profile has no private key.")
	case len(cfg.addresses) == 0:
		return nil, errors.New("The WireGuard profile has no Address.")
	case len(cfg.peers) == 0:
		return nil, errors.New("The WireGuard profile has no [Peer].")
	}
	for i, p := range cfg.peers {
		if p.publicKey == "" || p.endpoint == "" || len(p.allowedIPs) == 0 {
			return nil, fmt.Errorf("Peer %d of the WireGuard profile needs a PublicKey, an Endpoint and AllowedIPs.", i+1)
		}
		if _, _, err := net.SplitHostPort(p.endpoint); err != nil {
			return nil, fmt.Errorf("Endpoint %q is not host:port.", p.endpoint)
		}
	}
	return cfg, nil
}

// hexKey turns a base64 WireGuard key into hex. The error names the field,
// never the value.
func hexKey(field, b64 string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(b) != 32 {
		return "", fmt.Errorf("%s is not a WireGuard key.", field)
	}
	return hex.EncodeToString(b), nil
}

func prefixes(field, v string) ([]netip.Prefix, error) {
	var ps []netip.Prefix
	for _, s := range list(v) {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, fmt.Errorf("%s %q is not an address with a prefix length.", field, s)
		}
		ps = append(ps, p)
	}
	return ps, nil
}

func list(v string) []string {
	var out []string
	for s := range strings.SplitSeq(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// uapi is the device configuration, with each endpoint resolved by the
// system resolver: the endpoint is outside the tunnel, and UAPI takes only
// IP:port.
func (e *Engine) uapi(ctx context.Context) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "private_key=%s\nreplace_peers=true\n", e.cfg.privateKey)
	for _, p := range e.cfg.peers {
		host, port, _ := net.SplitHostPort(p.endpoint)
		ip, err := netip.ParseAddr(host)
		if err != nil {
			ips, lerr := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if lerr != nil || len(ips) == 0 {
				return "", fmt.Errorf("Cannot resolve the WireGuard endpoint %s.", p.endpoint)
			}
			ip = ips[0].Unmap()
		}
		fmt.Fprintf(&b, "public_key=%s\n", p.publicKey)
		if p.presharedKey != "" {
			fmt.Fprintf(&b, "preshared_key=%s\n", p.presharedKey)
		}
		fmt.Fprintf(&b, "endpoint=%s\npersistent_keepalive_interval=%d\nreplace_allowed_ips=true\n",
			net.JoinHostPort(ip.String(), port), keepalive)
		for _, a := range p.allowedIPs {
			fmt.Fprintf(&b, "allowed_ip=%s\n", a)
		}
	}
	return b.String(), nil
}

// Run brings the tunnel up and reports connecting, then connected on the
// first handshake. It returns after reporting failed (no handshake within
// connectTimeout) or dropped (none for dropAfter), or when ctx ends.
func (e *Engine) Run(ctx context.Context, report func(proto.Msg)) {
	report(proto.Msg{T: proto.State, State: proto.Connecting})
	fail := func(reason string) { report(proto.Msg{T: proto.State, State: proto.Failed, Reason: reason}) }
	u, err := e.uapi(ctx)
	if err != nil {
		fail(err.Error())
		return
	}
	// wireguard-go's own logger writes to stdout, which is the protocol.
	logger := &device.Logger{
		Verbosef: device.DiscardLogf,
		Errorf:   log.New(os.Stderr, "hostbay-vpn: wireguard: ", log.LstdFlags).Printf,
	}
	dev := device.NewDevice(e.tun, conn.NewDefaultBind(), logger)
	defer dev.Close()
	if err := dev.IpcSet(u); err != nil {
		fail("WireGuard refused the profile: " + err.Error())
		return
	}
	if err := dev.Up(); err != nil {
		fail("Cannot start WireGuard: " + err.Error())
		return
	}

	started, connected := time.Now(), false
	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		last := lastHandshake(dev)
		switch {
		case !connected && !last.IsZero():
			connected = true
			report(proto.Msg{T: proto.State, State: proto.Connected, Address: strs(e.cfg.addresses), Routes: e.routes()})
		case !connected && time.Since(started) > connectTimeout:
			fail(fmt.Sprintf("No WireGuard handshake from %s within %v.", e.endpoints(), connectTimeout))
			return
		case connected && time.Since(last) > dropAfter:
			report(proto.Msg{T: proto.State, State: proto.Dropped,
				Reason: fmt.Sprintf("No WireGuard handshake from %s for %v.", e.endpoints(), dropAfter)})
			return
		}
	}
}

// lastHandshake is the newest handshake with any peer, or zero for none.
func lastHandshake(dev *device.Device) time.Time {
	get, err := dev.IpcGet()
	if err != nil {
		return time.Time{}
	}
	var newest time.Time
	var sec int64
	for line := range strings.Lines(get) {
		k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch k {
		case "last_handshake_time_sec":
			sec, _ = strconv.ParseInt(v, 10, 64)
		case "last_handshake_time_nsec":
			nsec, _ := strconv.ParseInt(v, 10, 64)
			if t := time.Unix(sec, nsec); (sec != 0 || nsec != 0) && t.After(newest) {
				newest = t
			}
		}
	}
	return newest
}

func (e *Engine) routes() []string {
	var r []string
	for _, p := range e.cfg.peers {
		r = append(r, strs(p.allowedIPs)...)
	}
	return r
}

func (e *Engine) endpoints() string {
	var s []string
	for _, p := range e.cfg.peers {
		s = append(s, p.endpoint)
	}
	return strings.Join(s, ", ")
}

func strs(ps []netip.Prefix) []string {
	s := make([]string, len(ps))
	for i, p := range ps {
		s[i] = p.String()
	}
	return s
}

// Dial opens a TCP connection, or a connected UDP socket for network "udp",
// through the tunnel. Names resolve through the tunnel's DNS servers with its
// search domains, never the system resolver.
func (e *Engine) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	return stackdial.Dialer{Net: e.net, Search: e.cfg.search}.Dial(ctx, network, address)
}
