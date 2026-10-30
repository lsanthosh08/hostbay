package routing

import (
	"net/netip"
	"slices"
	"strings"
	"testing"

	"hostbay/internal/netd"
	"hostbay/internal/vpn/detect"
)

var lanGW = netip.MustParseAddr("192.168.1.1")

func route(dst, iface string) netd.SysRoute {
	return netd.SysRoute{Dst: netip.MustParsePrefix(dst), Iface: iface}
}

func viaLAN(dst string) netd.SysRoute {
	return netd.SysRoute{Dst: netip.MustParsePrefix(dst), Iface: "en0", Gateway: lanGW}
}

func labeled(iface, profile string) detect.Labeled {
	return detect.Labeled{Tunnel: detect.Tunnel{Iface: iface, Local: "10.255.0.2"}, Profile: profile}
}

// input is a Mac on a LAN with Harmony SASE on utun4 and the SSL VPN on
// utun6, both MTUs already at 1280.
func input(ps ...Profile) Input {
	return Input{
		Profiles: ps,
		Labels:   []detect.Labeled{labeled("utun4", "harmony"), labeled("utun6", "ssl")},
		Routes:   []netd.SysRoute{viaLAN("0.0.0.0/0")},
		Tunnels:  map[string]bool{"utun4": true, "utun6": true},
		MTUs:     map[string]int{"utun4": 1280, "utun6": 1280},
	}
}

func harmony(over func(*Profile)) Profile {
	p := Profile{ID: "harmony", Name: "Harmony SASE", Tier: "external", MTU: 1280}
	if over != nil {
		over(&p)
	}
	return p
}

// applyTo is what the route table holds after a plan applies: the system's
// view, kept apart from the planner's own bookkeeping.
func applyTo(routes []netd.SysRoute, p Plan) []netd.SysRoute {
	out := slices.Clone(routes)
	for _, c := range p.Changes {
		if c.Kind == KindMTU {
			continue
		}
		r := netd.SysRoute{Dst: netip.MustParsePrefix(c.Dst), Iface: c.Iface}
		if c.Gateway {
			r.Iface, r.Gateway = "en0", lanGW
		}
		if c.Op == netd.Delete {
			out = slices.DeleteFunc(out, func(x netd.SysRoute) bool { return x.Dst == r.Dst && (c.Gateway || x.Iface == r.Iface) })
		} else {
			out = append(out, r)
		}
	}
	return out
}

// outOf is the interface longest-prefix match sends addr out of.
func outOf(routes []netd.SysRoute, addr string) string {
	a, best, iface := netip.MustParseAddr(addr), -1, ""
	for _, r := range routes {
		if r.Dst.Contains(a) && r.Dst.Bits() > best {
			best, iface = r.Dst.Bits(), r.Iface
		}
	}
	return iface
}

func summary(p Plan) []string {
	var out []string
	for _, c := range p.Changes {
		s := c.Kind + " " + c.Op + " " + c.Dst
		switch {
		case c.Gateway:
			s += " via gateway"
		case c.Kind == KindMTU:
			s = "mtu " + c.Iface
		default:
			s += " on " + c.Iface
		}
		out = append(out, s)
	}
	return out
}

func TestPerDestinationRoutesEachNetworkToItsVPNOrLocal(t *testing.T) {
	in := input(harmony(func(p *Profile) { p.Routes = []string{"10.0.0.0/8"} }))
	in.Local = []string{"1.2.3.4"}
	p := Build(in)
	want := []string{"add add 10.0.0.0/8 on utun4", "add add 1.2.3.4/32 via gateway"}
	if got := summary(p); !slices.Equal(got, want) {
		t.Fatalf("plan %q, want %q", got, want)
	}
	if p.Changes[0].Profile != "harmony" || p.Changes[0].Reason == "" || p.Changes[1].Profile != "" {
		t.Fatalf("changes %+v", p.Changes)
	}
	if again := Build(withRoutes(in, applyTo(in.Routes, p))); len(again.Changes) != 0 {
		t.Fatalf("an applied plan plans again: %q", summary(again))
	}
}

func withRoutes(in Input, routes []netd.SysRoute) Input {
	in.Routes = routes
	return in
}

func TestPerDestinationStripsHalfRoutesOnVPNsOnly(t *testing.T) {
	in := input(harmony(func(p *Profile) { p.Routes = []string{"10.1.0.0/16"} }))
	in.Routes = append(in.Routes, route("0.0.0.0/1", "utun4"), route("128.0.0.0/1", "utun4"), viaLAN("0.0.0.0/1"), viaLAN("128.0.0.0/1"))
	got := summary(Build(in))
	want := []string{"delete delete 0.0.0.0/1 on utun4", "delete delete 128.0.0.0/1 on utun4", "add add 10.1.0.0/16 on utun4"}
	if !slices.Equal(got, want) {
		t.Fatalf("plan %q, want %q", got, want)
	}
}

func TestNarrowingIsOneDeleteAndAddsOnTheSameInterface(t *testing.T) {
	cs, err := Narrow("10.0.0.0/8", "utun4", []string{"10.1.0.0/16", "10.2.0.0/16"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"narrow delete 10.0.0.0/8 on utun4", "narrow add 10.1.0.0/16 on utun4", "narrow add 10.2.0.0/16 on utun4"}
	if got := summary(Plan{Changes: cs}); !slices.Equal(got, want) {
		t.Fatalf("narrow %q, want %q", got, want)
	}
	if _, err := Narrow("10.0.0.0/8", "utun4", nil); err == nil {
		t.Fatal("an empty keep list narrowed")
	}

	// F3: the saved keep list narrows Harmony's re-pushed 10/8 and the SSL
	// VPN gets 10.212/16.
	in := input(
		harmony(func(p *Profile) { p.Narrow = map[string][]string{"10.0.0.0/8": {"10.1.0.0/16", "10.2.0.0/16"}} }),
		Profile{ID: "ssl", Name: "SSL VPN", Tier: "external", MTU: 1280, Routes: []string{"10.212.0.0/16"}},
	)
	in.Routes = append(in.Routes, route("10.0.0.0/8", "utun4"))
	p := Build(in)
	want = append(want, "add add 10.212.0.0/16 on utun6")
	if got := summary(p); !slices.Equal(got, want) {
		t.Fatalf("F3 plan %q, want %q", got, want)
	}
	if again := Build(withRoutes(in, applyTo(in.Routes, p))); len(again.Changes) != 0 {
		t.Fatalf("a narrowed table plans again: %q", summary(again))
	}

	in.Profiles[0].Narrow["10.0.0.0/8"] = nil
	p = Build(in)
	if slices.ContainsFunc(p.Changes, func(c Change) bool { return c.Kind == KindNarrow }) ||
		!slices.ContainsFunc(p.Skipped, func(s Skip) bool { return s.Dst == "10.0.0.0/8" }) {
		t.Fatalf("an empty saved keep list narrowed: %+v", p)
	}
}

func TestGreedyRoutesAreOfferedForNarrowing(t *testing.T) {
	in := input(harmony(func(p *Profile) { p.Routes = []string{"10.1.0.0/16"} }))
	in.Routes = append(in.Routes, route("10.0.0.0/8", "utun4"), route("10.1.0.0/16", "utun4"))
	p := Build(in)
	if len(p.Greedy) != 1 || p.Greedy[0].Dst != "10.0.0.0/8" || p.Greedy[0].Iface != "utun4" || p.Greedy[0].Profile != "harmony" ||
		!slices.Equal(p.Greedy[0].Keep, []string{"10.1.0.0/16"}) {
		t.Fatalf("greedy offers %+v", p.Greedy)
	}
}

// AE3: a second full tunnel is refused naming the first; an app-scoped
// profile never conflicts, and the full-tunnel plan pins its gateway via
// the local gateway, before the halves, so its traffic keeps passing.
func TestAE3FullTunnelConflictAndAppScopedGatewayPin(t *testing.T) {
	first := harmony(func(p *Profile) { p.Tier, p.Mode = "system", Full })
	second := Profile{ID: "corp", Name: "Corp OpenVPN", Tier: "system", Routes: []string{"0.0.0.0/0"}}
	if err := Conflict([]Profile{first}, second); err == nil || !strings.Contains(err.Error(), "Harmony SASE") {
		t.Fatalf("second full tunnel: %v, want a refusal naming the first", err)
	}
	app := Profile{ID: "wg", Name: "Atlas", Tier: "app", Routes: []string{"0.0.0.0/0"}}
	if err := Conflict([]Profile{first}, app); err != nil {
		t.Fatalf("app-scoped profile conflicts: %v", err)
	}
	if err := Conflict([]Profile{app}, second); err != nil {
		t.Fatalf("a connected app-scoped profile blocks a full tunnel: %v", err)
	}

	in := input(first)
	in.Endpoints = []Endpoint{{Profile: "wg", Name: "Atlas", Addr: netip.MustParseAddr("203.0.113.7")}}
	p := Build(in)
	want := []string{"pin add 203.0.113.7/32 via gateway", "add add 0.0.0.0/1 on utun4", "add add 128.0.0.0/1 on utun4"}
	if got := summary(p); !slices.Equal(got, want) {
		t.Fatalf("plan %q, want %q", got, want)
	}
	after := applyTo(in.Routes, p)
	if outOf(after, "203.0.113.7") != "en0" || outOf(after, "8.8.8.8") != "utun4" {
		t.Fatalf("after apply: gateway via %s, internet via %s", outOf(after, "203.0.113.7"), outOf(after, "8.8.8.8"))
	}
	if again := Build(withRoutes(in, after)); len(again.Changes) != 0 {
		t.Fatalf("pinned table plans again: %q", summary(again))
	}

	// An endpoint no route captures needs no pin.
	in.Profiles[0].Mode = PerDestination
	in.Profiles[0].Routes = []string{"10.0.0.0/8"}
	if p := Build(in); slices.ContainsFunc(p.Changes, func(c Change) bool { return c.Kind == KindPin }) {
		t.Fatalf("uncaptured endpoint pinned: %q", summary(p))
	}
}

func TestUnlabeledVPNsDestinationsAreSkipped(t *testing.T) {
	in := input(harmony(func(p *Profile) { p.Routes = []string{"10.0.0.0/8"} }))
	in.Labels = []detect.Labeled{labeled("utun4", ""), labeled("utun6", "")}
	p := Build(in)
	if len(p.Changes) != 0 || len(p.Skipped) != 1 || p.Skipped[0].Dst != "10.0.0.0/8" || p.Skipped[0].Profile != "harmony" ||
		!strings.Contains(p.Skipped[0].Reason, "pin") {
		t.Fatalf("plan %+v", p)
	}
	if !slices.Equal(p.Unlabeled, []string{"utun4", "utun6"}) {
		t.Fatalf("unlabeled %v", p.Unlabeled)
	}
}

func TestTwoVPNsClaimingOneNetworkRouteNeither(t *testing.T) {
	in := input(
		harmony(func(p *Profile) { p.Routes = []string{"10.9.0.0/16"} }),
		Profile{ID: "ssl", Name: "SSL VPN", Tier: "external", MTU: 1280, Routes: []string{"10.9.0.0/16"}},
	)
	p := Build(in)
	if len(p.Changes) != 0 || len(p.Skipped) != 2 {
		t.Fatalf("plan %+v", p)
	}
}

func TestMTUIsSetOnUsedTunnels(t *testing.T) {
	in := input(harmony(func(p *Profile) { p.Routes = []string{"10.0.0.0/8"} }))
	in.MTUs["utun4"] = 1400
	p := Build(in)
	if got := summary(p); !slices.Equal(got, []string{"add add 10.0.0.0/8 on utun4", "mtu utun4"}) || p.Changes[1].MTU != 1280 {
		t.Fatalf("plan %q %+v", got, p.Changes)
	}
}
