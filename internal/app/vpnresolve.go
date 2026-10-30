package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"hostbay/internal/apperr"
	"hostbay/internal/sshx"
	"hostbay/internal/store"
)

// vpnConnectWait bounds how long a connect waits for its host's VPN profile
// to come up, a one-time code typed in included.
const vpnConnectWait = 2 * time.Minute

// LinkVPNs lets every dial reach hosts through VPN profiles (R15, KTD5).
func LinkVPNs(c *HostCredentials, v *VPNService) { c.vpns = v }

// dialVia resolves what a dial to h goes through (KTD5): the VPN profile
// vpnID names, else h's proxy, else the one connected app-scoped profile
// that reaches h's address (R17), else nothing, a direct dial. h is reached
// directly, never through a jump host: a chain's outermost host, whose
// vpnID is the one a host behind it names, or else h's own. Nothing is
// connected here; the UI's dials call vpnUp first, an agent's do not (KTD6).
func (c *HostCredentials) dialVia(d *store.Data, h *store.Host, vpnID string) (*sshx.Proxy, error) {
	if vpnID == "" {
		vpnID = store.Deref(h.VPNID)
	}
	switch {
	case vpnID != "" && h.Proxied():
		return nil, fmt.Errorf("%s is reached through a VPN profile and also names a proxy; choose one of them", h.Alias)
	case vpnID == store.HeldVPNID:
		return nil, apperr.New(apperr.VPNFailed, "This host's VPN profile was set on another computer and waits for your confirmation in Sync settings").
			With("reason", "held")
	case vpnID != "":
		v := d.VPN(vpnID)
		if v == nil {
			return nil, apperr.New(apperr.VPNFailed, "This host's VPN profile no longer exists; choose another in the host's settings").
				With("vpnId", vpnID).With("reason", "missing")
		}
		return c.vpnProxy(v, h.Hostname)
	case h.Proxied():
		return c.proxyFor(d, h)
	}
	return c.routed(d, h.Hostname)
}

// vpnProxy is the dial path through v, of any tier.
func (c *HostCredentials) vpnProxy(v *store.VPN, host string) (*sshx.Proxy, error) {
	switch vpnTier(*v) {
	case store.VPNTierSystem:
		// Checked at each dial, once vpnUp has had its chance to connect it.
		id, name := v.ID, v.Name
		return &sshx.Proxy{Type: "vpn", Host: name, VPN: id, Route: func(ctx context.Context, addr string) error {
			iface, err := c.vpns.systemIface(id, name)
			if err != nil {
				return err
			}
			return systemRoute(ctx, id, name, iface, addr)
		}}, nil
	case store.VPNTierExternal:
		// Pinned to the vendor tunnel's interface, labeled at each dial (KTD7).
		id, name, dns := v.ID, v.Name, slices.Clone(v.DNSServers)
		return &sshx.Proxy{Type: "vpn", Host: name, VPN: id, Iface: func(ctx context.Context) (string, []string, error) {
			iface, err := c.vpns.iface(ctx, id, name)
			return iface, dns, err
		}}, nil
	}
	id, name := v.ID, v.Name
	return &sshx.Proxy{Type: "vpn", Host: name, VPN: id, Socks: func() (string, string, string, error) {
		addr, user, pass, err := c.vpns.socks(id)
		if err != nil {
			err = apperr.New(apperr.VPNDown, fmt.Sprintf("VPN %s is not connected; connect it, then try again", name)).With("vpnId", id)
		}
		return addr, user, pass, err
	}}, nil
}

// systemRoute checks that the system sends addr's traffic through
// system-wide profile id's tunnel interface, so a dial with no proxy goes
// through it (KTD5): a UDP socket connected to addr, which sends nothing,
// takes the source address the route table picks, and that must be one of
// iface's. A name resolves through the system's resolver, as the dial's
// does.
func systemRoute(ctx context.Context, id, name, iface, addr string) error {
	fail := func(reason, format string, args ...any) error {
		return apperr.New(apperr.VPNFailed, fmt.Sprintf(format, args...)).With("vpnId", id).With("reason", reason)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fail("route", "%q is not host:port", addr)
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return fail("resolve", "VPN %s: cannot resolve %s: %v", name, host, err)
	}
	ifi, err := net.InterfaceByName(iface)
	var own []net.Addr
	if err == nil {
		own, err = ifi.Addrs()
	}
	if err != nil {
		return fail("route", "VPN %s: its interface %s is gone", name, iface)
	}
	conn, err := net.Dial("udp", net.JoinHostPort(ips[0].Unmap().String(), port))
	if err != nil {
		return fail("route", "VPN %s: this computer has no route to %s: %v", name, host, err)
	}
	local := conn.LocalAddr().(*net.UDPAddr).AddrPort().Addr().Unmap()
	conn.Close()
	for _, a := range own {
		if n, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok && ip.Unmap() == local {
				return nil
			}
		}
	}
	return fail("route", "VPN %s is connected, but this computer does not send %s through it; check the profile's routes", name, host)
}

// routed is the one connected app-scoped profile that reaches host (R17):
// an IP address inside its routes, other than default and half-internet
// routes, or a name inside its DNS domains. A name is never resolved here.
// Two or more is VPNAmbiguous, naming them.
func (c *HostCredentials) routed(d *store.Data, host string) (*sshx.Proxy, error) {
	ip, err := netip.ParseAddr(host)
	isIP := err == nil
	name := strings.ToLower(strings.TrimSuffix(host, "."))
	var hits []*store.VPN
	for _, st := range c.vpns.VPNStates() {
		v := d.VPN(st.ID)
		if st.Status != VPNConnected || v == nil || vpnTier(*v) != store.VPNTierApp {
			continue
		}
		if isIP && slices.ContainsFunc(st.Routes, func(r string) bool {
			p, err := netip.ParsePrefix(r)
			return err == nil && p.Bits() > 1 && p.Contains(ip.Unmap())
		}) || !isIP && slices.ContainsFunc(v.DNSDomains, func(dom string) bool {
			dom = strings.ToLower(strings.Trim(dom, "."))
			return name == dom || strings.HasSuffix(name, "."+dom)
		}) {
			hits = append(hits, v)
		}
	}
	switch len(hits) {
	case 0:
		return nil, nil
	case 1:
		return c.vpnProxy(hits[0], host)
	}
	slices.SortFunc(hits, func(a, b *store.VPN) int { return strings.Compare(a.Name, b.Name) })
	var names, ids []string
	for _, v := range hits {
		names, ids = append(names, v.Name), append(ids, v.ID)
	}
	return nil, apperr.New(apperr.VPNAmbiguous, fmt.Sprintf("%s is reached through more than one connected VPN profile (%s); choose one in the host's settings",
		host, strings.Join(names, ", "))).With("vpnIds", strings.Join(ids, ","))
}

// vpnUp connects the VPN profile p dials through when it is down, and waits
// until it is up (R15): the UI's dials call it, an agent's never do (KTD6).
func (c *HostCredentials) vpnUp(ctx context.Context, p *sshx.Proxy) error {
	if p == nil || p.Type != "vpn" || p.Iface != nil { // an external profile's own app connects it
		return nil
	}
	if c.vpns.connected(p.VPN) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, vpnConnectWait)
	defer cancel()
	st, err := c.vpns.ConnectVPN(ctx, p.VPN)
	var ae *apperr.Error
	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return apperr.New(apperr.VPNFailed, fmt.Sprintf("VPN %s did not connect in time", p.Host)).With("vpnId", p.VPN).With("reason", "timeout")
	case errors.As(err, &ae) && ae.Code != apperr.VPNFailed:
		return ae.With("vpnId", p.VPN) // a missing helper, say, keeps its own code
	}
	reason := st.Reason
	if reason == "" {
		reason = err.Error()
	}
	return apperr.New(apperr.VPNFailed, fmt.Sprintf("VPN %s could not connect: %s", p.Host, reason)).With("vpnId", p.VPN).With("reason", st.Status)
}

// dialUp is dialVia for h alone, with its VPN profile connected first: the
// UI's direct dials.
func (c *HostCredentials) dialUp(ctx context.Context, d *store.Data, h *store.Host) (*sshx.Proxy, error) {
	p, err := c.dialVia(d, h, "")
	if err != nil {
		return nil, err
	}
	return p, c.vpnUp(ctx, p)
}

// vpnKey names the revision of the VPN profile p dials through in a
// connection key, so editing the profile opens a fresh connection (KTD5).
func vpnKey(d *store.Data, p *sshx.Proxy) string {
	if p == nil || p.Type != "vpn" {
		return ""
	}
	return "~vpn:" + p.VPN + "@" + strconv.FormatInt(d.VPN(p.VPN).Revision, 10)
}
