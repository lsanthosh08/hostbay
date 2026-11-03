// Package pac writes the proxy auto-config Hostbay sets as the system's
// automatic proxy: each connected app-scoped VPN's sites and networks go to
// that VPN's loopback SOCKS5 listener, and everything else goes direct.
//
// The script resolves no names, since a lookup on every request would slow
// every page, so a network matches only a request to an IPv4 address. A match
// returns only "SOCKS5 127.0.0.1:<port>": browsers read a bare "SOCKS" as
// SOCKS v4, and a "; DIRECT" fallback would send a VPN's site around it.
package pac

import (
	"cmp"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"hostbay/internal/store"
)

// Profile is one connected VPN's share of the script.
type Profile struct {
	ID, Name string
	Port     int      // its SOCKS5 listener on 127.0.0.1
	Domains  []string // each with its subdomains
	Networks []string // CIDRs: its own routes and those its tunnel pushed
}

// Skip is an entry the script leaves out, and why.
type Skip struct {
	Entry, Profile, Reason string
}

// Result is the script and what went into it per profile ID.
type Result struct {
	Script   string // "" when nothing goes through a VPN
	Domains  map[string]int
	Networks map[string]int
	Skipped  []Skip
}

type rule struct {
	profile string
	port    int
	domain  string
	net     netip.Prefix
}

// Build writes the script for ps. An entry two profiles both list goes to
// the first; any other overlap goes to the most specific entry.
func Build(ps []Profile) Result {
	r := Result{Domains: map[string]int{}, Networks: map[string]int{}, Skipped: []Skip{}}
	var domains, nets []rule
	owner := map[string]string{} // domain or network: its profile's name
	claim := func(key string, p Profile) bool {
		if first, ok := owner[key]; ok {
			r.Skipped = append(r.Skipped, Skip{key, p.ID, "already goes through " + first})
			return false
		}
		owner[key] = p.Name
		return true
	}
	for _, p := range ps {
		for _, d := range p.Domains {
			c, ok := store.CleanDomain(d)
			if !ok || c != d {
				r.Skipped = append(r.Skipped, Skip{d, p.ID, "not a domain name"})
				continue
			}
			if claim(c, p) {
				domains = append(domains, rule{profile: p.ID, port: p.Port, domain: c})
				r.Domains[p.ID]++
			}
		}
		for _, s := range p.Networks {
			n, err := netip.ParsePrefix(s)
			if err != nil {
				r.Skipped = append(r.Skipped, Skip{s, p.ID, "not a network"})
				continue
			}
			// IPv6 hosts never match, and a default route or its halves would
			// pull every LAN address typed as an IP into the VPN.
			if !n.Addr().Is4() || n.Bits() < 2 {
				continue
			}
			if n = n.Masked(); claim(n.String(), p) {
				nets = append(nets, rule{profile: p.ID, port: p.Port, net: n})
				r.Networks[p.ID]++
			}
		}
	}
	if len(domains)+len(nets) == 0 {
		return r
	}
	// Most specific first, so the first match is the one that wins.
	slices.SortStableFunc(domains, func(a, b rule) int { return cmp.Compare(len(b.domain), len(a.domain)) })
	slices.SortStableFunc(nets, func(a, b rule) int { return cmp.Compare(b.net.Bits(), a.net.Bits()) })

	var w strings.Builder
	w.WriteString("function FindProxyForURL(url, host) {\n")
	w.WriteString("  var h = host.toLowerCase();\n")
	w.WriteString("  if (h.charAt(h.length - 1) === \".\") h = h.substring(0, h.length - 1);\n")
	for _, d := range domains {
		fmt.Fprintf(&w, "  if (h === %q || dnsDomainIs(h, %q)) return \"SOCKS5 127.0.0.1:%d\";\n", d.domain, "."+d.domain, d.port)
	}
	if len(nets) > 0 {
		w.WriteString("  if (/^\\d+\\.\\d+\\.\\d+\\.\\d+$/.test(h)) {\n")
		for _, n := range nets {
			var mask [4]byte
			for i := range n.net.Bits() {
				mask[i/8] |= 0x80 >> (i % 8)
			}
			fmt.Fprintf(&w, "    if (isInNet(h, %q, %q)) return \"SOCKS5 127.0.0.1:%d\";\n", n.net.Addr(), netip.AddrFrom4(mask), n.port)
		}
		w.WriteString("  }\n")
	}
	w.WriteString("  return \"DIRECT\";\n}\n")
	r.Script = w.String()
	return r
}
