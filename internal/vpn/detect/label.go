package detect

import (
	"net/netip"
	"slices"
	"strings"
)

// Profile is what labeling knows of one external profile.
type Profile struct {
	ID     string
	Names  []string // what the OS may call its VPN: the profile's name and its client's
	Shape  string   // ShapeFor its type
	Ranges []string // CIDRs its tunnel's addresses fall in: its addresses and routes
	Iface  string   // the interface the user pinned it to on this computer
}

// Labeled is a tunnel and the profile it belongs to, if any.
type Labeled struct {
	Tunnel
	Profile string `json:"profile,omitempty"` // profile ID; "" when unlabeled
	By      string `json:"by,omitempty"`      // "os", "pin", "range" or "shape"
}

// Label ties each tunnel to its profile, by the strongest signal first, as
// TunnelHub does (R24): the name the OS gives the tunnel's VPN, then a
// pinned interface, then the profile's address range, then the tunnel's
// shape. Every step labels only when exactly one candidate fits; a tunnel
// two profiles could own stays unlabeled, so no traffic follows a guess.
func Label(tuns []Tunnel, ps []Profile) []Labeled {
	out := make([]Labeled, len(tuns))
	for i, t := range tuns {
		out[i].Tunnel = t
	}
	owns := func(id string) bool { return slices.ContainsFunc(out, func(l Labeled) bool { return l.Profile == id }) }
	// claim labels each unlabeled tunnel that exactly one unclaimed profile
	// fits, when that profile fits no other unlabeled tunnel.
	claim := func(by string, fits func(t Tunnel, p Profile) bool) {
		hits := map[int][]int{} // tunnel -> profiles
		count := map[int]int{}  // profile -> tunnels
		for i := range out {
			for j, p := range ps {
				if out[i].Profile == "" && !owns(p.ID) && fits(out[i].Tunnel, p) {
					hits[i] = append(hits[i], j)
					count[j]++
				}
			}
		}
		for i, js := range hits {
			if len(js) == 1 && count[js[0]] == 1 {
				out[i].Profile, out[i].By = ps[js[0]].ID, by
			}
		}
	}
	claim("os", func(t Tunnel, p Profile) bool {
		return t.VPN != "" && slices.ContainsFunc(p.Names, func(n string) bool { return strings.EqualFold(n, t.VPN) })
	})
	claim("pin", func(t Tunnel, p Profile) bool { return p.Iface != "" && p.Iface == t.Iface })
	claim("range", func(t Tunnel, p Profile) bool {
		return slices.ContainsFunc(p.Ranges, func(r string) bool { return holds(r, t.Local) || holds(r, t.Peer) })
	})
	// Shape: only when one unclaimed profile and one unlabeled tunnel of a
	// shape remain; two VPNs of the same shape cannot be told apart.
	for _, shape := range []string{PeerLess, PointToPoint} {
		var free []string
		for _, p := range ps {
			if p.Shape == shape && !owns(p.ID) {
				free = append(free, p.ID)
			}
		}
		var open []int
		for i, l := range out {
			// A tunnel the OS names after a VPN belongs to that VPN, so its
			// shape says nothing about any other profile.
			if l.Profile == "" && l.Shape == shape && l.VPN == "" {
				open = append(open, i)
			}
		}
		if len(free) == 1 && len(open) == 1 {
			out[open[0]].Profile, out[open[0]].By = free[0], "shape"
		}
	}
	return out
}

// holds reports whether network r holds address a. Default and
// half-internet routes hold everything, so they tell nothing.
func holds(r, a string) bool {
	p, err := netip.ParsePrefix(r)
	ip, err2 := netip.ParseAddr(a)
	return err == nil && err2 == nil && p.Bits() > 1 && p.Contains(ip)
}
