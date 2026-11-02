package detect

import (
	"context"
	"strings"
)

// detect ports TunnelHub's detect_vpns: VPNs registered with the OS
// (scutil), apps and daemons that run their own tunnel, and every utun
// carrying an IPv4 address.
func detect(ctx context.Context) (Snapshot, error) {
	nc := run(ctx, "scutil", "--nc", "list")
	status := map[string]string{}
	for _, v := range ncServices(nc) {
		if v.Connected {
			status[v.Name] = run(ctx, "scutil", "--nc", "status", v.Name)
		}
	}
	return fromDarwin(nc, status, run(ctx, "ifconfig"), run(ctx, "ps", "axo", "command")), nil
}

// fromDarwin reads the tools' output: `scutil --nc list`, `scutil --nc
// status` per connected service, `ifconfig` and `ps axo command`.
func fromDarwin(nc string, status map[string]string, ifconfig, ps string) Snapshot {
	var s Snapshot
	owner := map[string]string{} // interface -> service the OS says owns it
	for _, v := range ncServices(nc) {
		if st := status[v.Name]; v.Connected && strings.HasPrefix(st, "Connected") {
			for _, l := range strings.Split(st, "\n") {
				if k, val, ok := strings.Cut(l, ":"); ok && strings.TrimSpace(k) == "InterfaceName" {
					v.Iface = strings.TrimSpace(val)
					owner[v.Iface] = v.Name
					break
				}
			}
		}
		s.VPNs = add(s.VPNs, v)
	}
	unexplained := 0
	for _, t := range utuns(ifconfig) {
		t.VPN = owner[t.Iface]
		if t.VPN == "" && t.Shape == PointToPoint {
			unexplained++
		}
		s.Tunnels = append(s.Tunnels, t)
	}
	agents, daemons := processVPNs(ps)
	// An agent shows only as a point-to-point utun nothing else explains
	// (Harmony SASE, Perimeter 81): with at least one such tunnel for each
	// agent running, they are all up.
	for _, v := range agents {
		v.Connected = unexplained >= len(agents)
		s.VPNs = add(s.VPNs, v)
	}
	for _, v := range daemons {
		s.VPNs = add(s.VPNs, v)
	}
	return s
}

// add appends v unless a VPN of its type, or of its name for type
// "other", is already there: a daemon and its app are one VPN.
func add(vs []VPN, v VPN) []VPN {
	for _, w := range vs {
		if v.Type != "other" && w.Type == v.Type || strings.EqualFold(w.Name, v.Name) {
			return vs
		}
	}
	return append(vs, v)
}

// ncServices reads `scutil --nc list` lines such as
// `* (Connected) UUID VPN (com.vendor.app) "Name" [VPN:com.vendor.app]`.
func ncServices(nc string) []VPN {
	var out []VPN
	for _, l := range strings.Split(nc, "\n") {
		_, rest, ok := strings.Cut(l, "VPN (")
		id, rest, ok2 := strings.Cut(rest, ")")
		name := strings.Split(rest, `"`)
		if !ok || !ok2 || len(name) < 3 || name[1] == "" {
			continue
		}
		typ := TypeFor(id)
		out = append(out, VPN{Name: name[1], Type: typ, Shape: ShapeFor(typ), Connected: strings.Contains(l, "(Connected)")})
	}
	return out
}

// utuns reads every utun with an IPv4 address out of `ifconfig`; IPv6-only
// and addressless ones carry nothing to dial through.
func utuns(ifconfig string) []Tunnel {
	var out []Tunnel
	iface, done := "", false
	for _, l := range strings.Split(ifconfig, "\n") {
		if l != "" && l[0] != '\t' && l[0] != ' ' {
			iface, _, _ = strings.Cut(l, ":")
			done = !strings.HasPrefix(iface, "utun")
			continue
		}
		f := strings.Fields(l)
		if done || len(f) < 2 || f[0] != "inet" {
			continue
		}
		t := Tunnel{Iface: iface, Local: f[1], Shape: PeerLess}
		if len(f) > 3 && f[2] == "-->" {
			t.Peer, t.Shape = f[3], PointToPoint
		}
		out, done = append(out, t), true
	}
	return out
}

// processVPNs finds VPNs by their running processes: agents, apps in
// /Applications that run their own tunnel, and daemons such as Cisco's
// vpnagentd or a VPN system extension that register nothing with the OS.
func processVPNs(ps string) (agents, daemons []VPN) {
	for _, line := range strings.Split(ps, "\n") {
		l := strings.ToLower(line)
		if strings.HasPrefix(l, "/system/") || strings.Contains(l, "com.apple.") {
			continue
		}
		// An app is a VPN agent by its own name, never by its arguments: Claude's
		// helper runs with "vpn" among them and is no VPN.
		if rest, ok := strings.CutPrefix(line, "/Applications/"); ok {
			if app, _, ok := strings.Cut(rest, ".app"); ok && !strings.Contains(app, "/") &&
				(TypeFor(app) != "other" || containsAny(strings.ToLower(app), "vpn", "sase", "wireguard")) {
				typ := TypeFor(app)
				agents = add(agents, VPN{Name: app, Type: typ, Shape: ShapeFor(typ), Client: "/Applications/" + app + ".app"})
				continue
			}
		}
		if !containsAny(l, "vpnagent", "tunnelprovider", "/vpn/", "anyconnect", "secureclient") &&
			!(strings.Contains(l, ".systemextension") && containsAny(l, "vpn", "tunnel")) {
			continue
		}
		if name := daemonName(line); name != "" {
			typ := TypeFor(name)
			daemons = add(daemons, VPN{Name: name, Type: typ, Shape: ShapeFor(typ)})
		}
	}
	return agents, daemons
}

// daemonName names a daemon by its install path: /opt/<vendor>/...,
// <Name>.framework, or the vendor in a reverse-DNS bundle id.
func daemonName(line string) string {
	path, _, _ := strings.Cut(line, " ")
	parts := strings.Split(path, "/")
	for i, p := range parts {
		switch {
		case p == "opt" && i+1 < len(parts):
			return parts[i+1]
		case strings.HasSuffix(p, ".framework"):
			return strings.TrimSuffix(p, ".framework")
		}
	}
	for _, p := range parts {
		if f := strings.Split(p, "."); len(f) >= 3 && (f[0] == "com" || f[0] == "net" || f[0] == "org" || f[0] == "io") {
			return f[1]
		}
	}
	return ""
}
