// Package routing plans the route changes that send each destination
// through the right VPN (R25 to R28, KTD9). Build is a pure function of the
// profiles, the labeled tunnels and the live route table: it returns only
// what differs from the table, so a plan that has applied plans nothing, and
// re-applying on every route event is safe. internal/app runs the plan
// through hostbay-netd.
package routing

import (
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"

	"hostbay/internal/netd"
	"hostbay/internal/store"
	"hostbay/internal/vpn/detect"
)

// Routing modes, as store.VPN.RoutingMode holds them. A profile whose
// routes include a default route (the profile form's "Full tunnel") is a
// full tunnel in the default mode too.
const (
	PerDestination = ""      // each network to its own VPN, or to the local network
	Full           = "full"  // everything through this VPN: 0.0.0.0/1 and 128.0.0.0/1
	Split          = "split" // only this VPN's networks through it
)

// Change kinds, as the preview shows them.
const (
	KindAdd    = "add"
	KindDelete = "delete"
	KindNarrow = "narrow" // a greedy route replaced by the subnets kept
	KindPin    = "pin"    // an app-scoped profile's gateway kept on the local network
	KindMTU    = "mtu"
)

// Local names the local network as a destination's target.
const Local = "local"

// halves are the routes a full tunnel installs: together they beat the
// default route without replacing it.
var halves = []netip.Prefix{netip.MustParsePrefix("0.0.0.0/1"), netip.MustParsePrefix("128.0.0.0/1")}

// Profile is what planning knows of one VPN profile.
type Profile struct {
	ID, Name string
	Tier     string // store.VPNTier*; app-scoped profiles add no system routes
	Mode     string
	Routes   []string
	MTU      int
	// Narrow is the user's keep list for each greedy route on this VPN's
	// tunnel, re-applied whenever the vendor pushes the route again.
	Narrow map[string][]string
	// Domains are the sites whose addresses go through this VPN as host
	// routes, as last resolved.
	Domains []Domain
}

// Domain is a listed site and the addresses it last resolved to. Err is
// why the last lookup failed; Addrs then holds what it returned before.
type Domain struct {
	Name  string
	Addrs []netip.Addr
	Err   string
}

// Endpoint is a connected app-scoped profile's gateway, which a full or
// per-destination plan must not capture into a tunnel.
type Endpoint struct {
	Profile, Name string
	Addr          netip.Addr
}

// Input is everything a plan depends on.
type Input struct {
	Profiles  []Profile
	Labels    []detect.Labeled
	Local     []string        // destinations sent to the local network
	Routes    []netd.SysRoute // the live route table
	Tunnels   map[string]bool // interface name: is a tunnel
	MTUs      map[string]int  // interface name: its MTU now
	Endpoints []Endpoint
	// Added is the domain routes Hostbay added: one the plan no longer wants
	// is deleted, since the domain moved off its address.
	Added []Change
}

// Change is one step of a plan.
type Change struct {
	Kind    string `json:"kind"`
	Op      string `json:"op,omitempty"` // netd.Add or netd.Delete; "" for KindMTU
	Dst     string `json:"dst,omitempty"`
	Iface   string `json:"iface,omitempty"`
	Gateway bool   `json:"gateway,omitempty"` // through the local default gateway
	MTU     int    `json:"mtu,omitempty"`
	Profile string `json:"profile,omitempty"` // the profile it is for; "" for the local network or no profile
	Domain  string `json:"domain,omitempty"`  // the site whose address this host route carries
	Reason  string `json:"reason"`
}

// Route is the change as hostbay-netd takes it.
func (c Change) Route() netd.Route { return netd.Route{Dst: c.Dst, Iface: c.Iface, Gateway: c.Gateway} }

// Skip is a destination the plan does not route, and why.
type Skip struct {
	Dst     string `json:"dst"`
	Profile string `json:"profile,omitempty"`
	Reason  string `json:"reason"`
}

// Greedy is a broad vendor route that would swallow another VPN's networks,
// offered for narrowing (R27), with the profile's own subnets inside it as
// the suggested keep list.
type Greedy struct {
	Dst     string   `json:"dst"`
	Iface   string   `json:"iface"`
	Profile string   `json:"profile"`
	Keep    []string `json:"keep"`
}

// Plan is the ordered changes, pins first so an app-scoped tunnel never
// loses its gateway mid-apply, then deletes, adds and MTUs.
type Plan struct {
	Changes   []Change `json:"changes"`
	Skipped   []Skip   `json:"skipped"`
	Greedy    []Greedy `json:"greedy"`
	Unlabeled []string `json:"unlabeled"` // tunnel interfaces no profile owns, to pin
}

// want is a destination's target before diffing against the table.
type want struct {
	dst     netip.Prefix
	iface   string // "" with gateway
	gateway bool
	kind    string
	profile string
	domain  string
	reason  string
}

// Build plans the changes that make the table route as the profiles say.
func Build(in Input) Plan {
	p := Plan{Changes: []Change{}, Skipped: []Skip{}, Greedy: []Greedy{}, Unlabeled: []string{}}
	ifaceOf, owner := map[string]string{}, map[string]Profile{}
	for _, l := range in.Labels {
		if l.Profile == "" {
			p.Unlabeled = append(p.Unlabeled, l.Iface)
			continue
		}
		ifaceOf[l.Profile] = l.Iface
	}
	var (
		wants     []want
		dels      []Change
		fullIface string
		fullName  string
		perDest   bool
	)
	skip := func(dst, profile, reason string) { p.Skipped = append(p.Skipped, Skip{dst, profile, reason}) }

	for _, pr := range in.Profiles {
		if pr.Tier == store.VPNTierApp {
			continue
		}
		mode, nets, bad := modeOf(pr)
		for _, b := range bad {
			skip(b, pr.ID, fmt.Sprintf("%q is not a network", b))
		}
		iface := ifaceOf[pr.ID]
		if iface == "" {
			reason := fmt.Sprintf("%s is not connected", pr.Name)
			if len(p.Unlabeled) > 0 {
				reason = fmt.Sprintf("Hostbay cannot tell which tunnel is %s's; pin its interface", pr.Name)
			}
			if mode == Full {
				skip("0.0.0.0/0", pr.ID, reason)
			}
			for _, n := range nets {
				skip(n.String(), pr.ID, reason)
			}
			for _, d := range pr.Domains {
				skip(d.Name, pr.ID, reason)
			}
			continue
		}
		wants = append(wants, domainWants(pr, iface, in.Endpoints, skip)...)
		owner[iface] = pr
		switch mode {
		case Full:
			if fullIface != "" {
				skip("0.0.0.0/0", pr.ID, fmt.Sprintf("%s is already the full tunnel", fullName))
				continue
			}
			fullIface, fullName = iface, pr.Name
			for _, h := range halves {
				wants = append(wants, want{dst: h, iface: iface, kind: KindAdd, profile: pr.ID, reason: "everything through " + pr.Name})
			}
			continue
		case PerDestination:
			perDest = true
		}
		for _, n := range nets {
			wants = append(wants, want{dst: n, iface: iface, kind: KindAdd, profile: pr.ID, reason: fmt.Sprintf("%s through %s", n, pr.Name)})
		}
		for _, g := range slices.Sorted(maps.Keys(pr.Narrow)) {
			keep := pr.Narrow[g]
			gp, err := netip.ParsePrefix(g)
			if err != nil || !slices.ContainsFunc(in.Routes, func(r netd.SysRoute) bool { return r.Dst == gp && r.Iface == iface }) {
				continue
			}
			cs, err := Narrow(g, iface, keep)
			if err != nil {
				skip(g, pr.ID, err.Error())
				continue
			}
			cs[0].Profile = pr.ID
			dels = append(dels, cs[0])
			for _, c := range cs[1:] {
				wants = append(wants, want{dst: netip.MustParsePrefix(c.Dst), iface: iface, kind: KindNarrow, profile: pr.ID, reason: c.Reason})
			}
		}
	}
	for _, d := range in.Local {
		n, err := ParseDst(d)
		if err != nil {
			skip(d, "", fmt.Sprintf("%q is not a network", d))
			continue
		}
		wants = append(wants, want{dst: n, gateway: true, kind: KindAdd, reason: fmt.Sprintf("%s through the local network", n)})
	}
	wants = dedupe(wants, skip)

	// Domain routes Hostbay added that no domain wants any more.
	for _, a := range in.Added {
		if a.Domain == "" || slices.ContainsFunc(wants, func(w want) bool { return w.dst.String() == a.Dst && w.iface == a.Iface }) ||
			!slices.ContainsFunc(in.Routes, func(r netd.SysRoute) bool { return r.Dst.String() == a.Dst && r.Iface == a.Iface }) {
			continue
		}
		dels = append(dels, Change{Kind: KindDelete, Op: netd.Delete, Dst: a.Dst, Iface: a.Iface, Profile: a.Profile, Domain: a.Domain,
			Reason: fmt.Sprintf("%s no longer resolves to %s", a.Domain, strings.TrimSuffix(a.Dst, "/32"))})
	}

	// Greedy vendor routes on a per-destination or split VPN's tunnel.
	for _, r := range in.Routes {
		pr, ok := owner[r.Iface]
		if !ok || r.Iface == fullIface || r.Dst.Bits() < 2 || r.Dst.Bits() > 8 || !r.Dst.Addr().Is4() {
			continue
		}
		if _, narrowed := pr.Narrow[r.Dst.String()]; narrowed || slices.ContainsFunc(wants, func(w want) bool { return w.dst == r.Dst && w.iface == r.Iface }) {
			continue
		}
		g := Greedy{Dst: r.Dst.String(), Iface: r.Iface, Profile: pr.ID, Keep: []string{}}
		for _, w := range wants {
			if w.profile == pr.ID && w.dst.Bits() > r.Dst.Bits() && r.Dst.Contains(w.dst.Addr()) {
				g.Keep = append(g.Keep, w.dst.String())
			}
		}
		p.Greedy = append(p.Greedy, g)
	}

	// Half-internet routes a VPN's full-tunnel setting installed (R27); never
	// the local gateway's, nor the chosen full tunnel's own.
	for _, r := range in.Routes {
		if !slices.Contains(halves, r.Dst) || !in.Tunnels[r.Iface] || r.Iface == fullIface {
			continue
		}
		pr, owned := owner[r.Iface]
		if mode, _, _ := modeOf(pr); perDest || owned && mode == Split {
			dels = append(dels, Change{Kind: KindDelete, Op: netd.Delete, Dst: r.Dst.String(), Iface: r.Iface, Profile: pr.ID,
				Reason: "half-internet route a VPN's full-tunnel setting installed"})
		}
	}

	// Diff each target against the table: a route already there is left
	// alone; one on another tunnel moves.
	var adds []Change
	for _, w := range wants {
		gw, hasGW := netd.DefaultGateway(in.Routes, in.Tunnels, w.dst)
		if w.gateway && !hasGW {
			skip(w.dst.String(), w.profile, "no local gateway")
			continue
		}
		at := w.iface
		if w.gateway {
			at = gw.Iface
		}
		if slices.ContainsFunc(in.Routes, func(r netd.SysRoute) bool { return r.Dst == w.dst && r.Iface == at }) {
			continue
		}
		for _, r := range in.Routes {
			if r.Dst == w.dst && in.Tunnels[r.Iface] && !slices.ContainsFunc(dels, func(c Change) bool { return c.Dst == r.Dst.String() && c.Iface == r.Iface }) {
				dels = append(dels, Change{Kind: KindDelete, Op: netd.Delete, Dst: r.Dst.String(), Iface: r.Iface, Profile: w.profile,
					Reason: fmt.Sprintf("%s moves off %s", r.Dst, r.Iface)})
			}
		}
		adds = append(adds, Change{Kind: w.kind, Op: netd.Add, Dst: w.dst.String(), Iface: w.iface, Gateway: w.gateway, Profile: w.profile, Domain: w.domain, Reason: w.reason})
	}

	// Pin each app-scoped gateway the table would send into a tunnel once
	// this plan applies (KTD9).
	after := slices.DeleteFunc(slices.Clone(in.Routes), func(r netd.SysRoute) bool {
		return slices.ContainsFunc(dels, func(c Change) bool { return c.Dst == r.Dst.String() && c.Iface == r.Iface })
	})
	for _, c := range adds {
		r := netd.SysRoute{Dst: netip.MustParsePrefix(c.Dst), Iface: c.Iface}
		if c.Gateway {
			gw, _ := netd.DefaultGateway(in.Routes, in.Tunnels, r.Dst)
			r.Iface = gw.Iface
		}
		after = append(after, r)
	}
	var pins []Change
	for _, e := range in.Endpoints {
		host := netip.PrefixFrom(e.Addr, e.Addr.BitLen())
		if !in.Tunnels[lookup(after, e.Addr)] {
			continue
		}
		if _, ok := netd.DefaultGateway(in.Routes, in.Tunnels, host); !ok {
			skip(host.String(), e.Profile, "no local gateway to keep "+e.Name+"'s gateway on")
			continue
		}
		pins = append(pins, Change{Kind: KindPin, Op: netd.Add, Dst: host.String(), Gateway: true, Profile: e.Profile,
			Reason: fmt.Sprintf("keeps %s's gateway %s on the local network", e.Name, e.Addr)})
	}

	// MTU on each tunnel this plan routes through (R28).
	var mtus []Change
	for _, iface := range slices.Sorted(maps.Keys(owner)) {
		pr := owner[iface]
		used := slices.ContainsFunc(wants, func(w want) bool { return w.iface == iface })
		if used && pr.MTU > 0 && in.MTUs[iface] != pr.MTU {
			mtus = append(mtus, Change{Kind: KindMTU, Iface: iface, MTU: pr.MTU, Profile: pr.ID, Reason: fmt.Sprintf("%s's tunnel MTU", pr.Name)})
		}
	}
	p.Changes = slices.Concat(pins, dels, adds, mtus)
	if p.Changes == nil {
		p.Changes = []Change{}
	}
	return p
}

// domainWants is a host route on iface for each address pr's domains
// resolved to. Only global unicast IPv4 addresses qualify: whoever answers
// DNS for a domain must not route loopback, link-local or an unspecified
// address, nor a VPN's own gateway, into a tunnel.
func domainWants(pr Profile, iface string, endpoints []Endpoint, skip func(dst, profile, reason string)) []want {
	var out []want
	for _, d := range pr.Domains {
		if d.Err != "" {
			skip(d.Name, pr.ID, fmt.Sprintf("%s could not be resolved: %s", d.Name, d.Err))
		}
		for _, a := range d.Addrs {
			a = a.Unmap()
			if !a.Is4() {
				continue
			}
			host := netip.PrefixFrom(a, 32)
			switch {
			case !a.IsGlobalUnicast() || a.IsLoopback():
				skip(host.String(), pr.ID, fmt.Sprintf("%s resolved to %s, which is not routable", d.Name, a))
			case slices.ContainsFunc(endpoints, func(e Endpoint) bool { return e.Addr == a }):
				skip(host.String(), pr.ID, fmt.Sprintf("%s resolved to a VPN gateway, %s", d.Name, a))
			default:
				out = append(out, want{dst: host, iface: iface, kind: KindAdd, profile: pr.ID, domain: d.Name,
					reason: fmt.Sprintf("%s (%s) through %s", a, d.Name, pr.Name)})
			}
		}
	}
	return out
}

// dedupe drops repeated targets and skips every claim on a destination two
// profiles (or a profile and the local network) both claim, so the plan
// never moves a route back and forth between them.
func dedupe(ws []want, skip func(dst, profile, reason string)) []want {
	var out []want
	conflict := map[netip.Prefix]bool{}
	for i, w := range ws {
		for _, o := range ws[:i] {
			if o.dst == w.dst && (o.iface != w.iface || o.gateway != w.gateway) {
				conflict[w.dst] = true
			}
		}
	}
	for i, w := range ws {
		if conflict[w.dst] {
			skip(w.dst.String(), w.profile, fmt.Sprintf("more than one target routes %s; keep it in one profile", w.dst))
			continue
		}
		if !slices.ContainsFunc(ws[:i], func(o want) bool { return o.dst == w.dst }) {
			out = append(out, w)
		}
	}
	return out
}

// Narrow replaces greedy route on iface with the keep list on the same
// interface (R27). An empty keep list is refused: stripping the route
// blindly would cut the VPN off from everything it carried.
func Narrow(greedy, iface string, keep []string) ([]Change, error) {
	if len(keep) == 0 {
		return nil, errors.New("no subnets to keep; refusing to remove the broad route blindly")
	}
	g, err := ParseDst(greedy)
	if err != nil {
		return nil, fmt.Errorf("%q is not a network", greedy)
	}
	cs := []Change{{Kind: KindNarrow, Op: netd.Delete, Dst: g.String(), Iface: iface, Reason: fmt.Sprintf("narrows the broad %s", g)}}
	for _, k := range keep {
		n, err := ParseDst(k)
		if err != nil {
			return nil, fmt.Errorf("%q is not a network", k)
		}
		cs = append(cs, Change{Kind: KindNarrow, Op: netd.Add, Dst: n.String(), Iface: iface, Reason: fmt.Sprintf("keeps %s of %s", n, g)})
	}
	return cs, nil
}

// Conflict checks whether profile p may connect system-wide while the
// connected profiles are up (R13): a second full tunnel, or a network
// another system-wide or external VPN already routes, is refused naming
// that VPN. App-scoped profiles use no system routes and never conflict.
func Conflict(connected []Profile, p Profile) error {
	if p.Tier == store.VPNTierApp {
		return nil
	}
	mode, nets, _ := modeOf(p)
	for _, c := range connected {
		if c.Tier == store.VPNTierApp || c.ID == p.ID {
			continue
		}
		cmode, cnets, _ := modeOf(c)
		if mode == Full && cmode == Full {
			return fmt.Errorf("%s is already a full tunnel; disconnect it first, or route %s per destination", c.Name, p.Name)
		}
		for _, n := range nets {
			if slices.Contains(cnets, n) {
				return fmt.Errorf("%s already routes %s", c.Name, n)
			}
		}
	}
	return nil
}

// modeOf is a profile's effective mode and its networks without default
// routes, and the routes that do not parse.
func modeOf(p Profile) (mode string, nets []netip.Prefix, bad []string) {
	mode = p.Mode
	if mode != Full && mode != Split {
		mode = PerDestination
	}
	for _, r := range p.Routes {
		n, err := ParseDst(r)
		switch {
		case err != nil:
			bad = append(bad, r)
		case n.Bits() == 0:
			if p.Mode == PerDestination {
				mode = Full
			}
		default:
			nets = append(nets, n)
		}
	}
	return mode, nets, bad
}

// ParseDst reads a CIDR or a bare address (a host route) in the canonical
// form hostbay-netd requires.
func ParseDst(s string) (netip.Prefix, error) {
	if a, err := netip.ParseAddr(s); err == nil {
		return netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()), nil
	}
	n, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return n.Masked(), nil
}

// lookup is the interface the longest matching route sends a out of.
func lookup(routes []netd.SysRoute, a netip.Addr) string {
	best, iface := -1, ""
	for _, r := range routes {
		if r.Dst.Contains(a) && r.Dst.Bits() > best {
			best, iface = r.Dst.Bits(), r.Iface
		}
	}
	return iface
}
