// Package tunnelhub imports a TunnelHub vpn-config.json (R41): its VPNs as
// external profiles, its routes as per-destination routes on them, and its
// Chrome entries as browser mappings, which are reported until browser
// routing exists to save them.
package tunnelhub

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"

	"hostbay/internal/importer"
	"hostbay/internal/store"
)

type vpnDef struct {
	Display string `json:"display"`
	Match   string `json:"match"`
	Exclude string `json:"exclude"`
	Iface   string `json:"iface"`
	Peer    string `json:"peer"`
	Kind    string `json:"kind"`
	Socks   string `json:"socks"`
	MTU     int    `json:"mtu"`
}

type config struct {
	VPNs   map[string]vpnDef `json:"vpns"`
	Routes []struct {
		Dest string `json:"dest"`
		VPN  string `json:"vpn"`
	} `json:"routes"`
	Chrome []struct {
		Profile string `json:"profile"`
		VPN     string `json:"vpn"`
	} `json:"chrome"`
}

// Parse reads the vpn-config.json at path. It holds no secrets.
func Parse(path, _ string) (importer.Result, error) {
	raw, err := importer.ReadFile(path)
	if err != nil {
		return importer.Result{}, err
	}
	var c config
	if err := json.Unmarshal(raw, &c); err != nil {
		return importer.Result{}, fmt.Errorf("this is not a TunnelHub vpn-config.json: %w", err)
	}
	if len(c.VPNs) == 0 {
		return importer.Result{}, fmt.Errorf("this is not a TunnelHub vpn-config.json: it lists no VPNs")
	}
	b := importer.NewBuilder("TunnelHub")
	routes := map[string][]string{}
	for i, r := range c.Routes {
		p, err := netip.ParsePrefix(strings.TrimSpace(r.Dest))
		switch _, ok := c.VPNs[r.VPN]; {
		case err != nil:
			b.Skip(fmt.Sprintf("routes[%d]", i), fmt.Sprintf("%q is not a network such as 10.0.0.0/24", r.Dest))
		case !ok:
			b.Skip(fmt.Sprintf("routes[%d]", i), fmt.Sprintf("it names VPN %q, which the file does not define", r.VPN))
		default:
			routes[r.VPN] = append(routes[r.VPN], p.Masked().String())
		}
	}
	names := map[string]string{} // config key -> profile name
	for _, key := range slices.Sorted(maps.Keys(c.VPNs)) {
		v := c.VPNs[key]
		name := strings.TrimSpace(v.Display)
		if name == "" {
			name = key
		}
		p := store.VPN{Name: name, Type: typeOf(key, v), Tier: store.VPNTierExternal, Client: name, MTU: v.MTU, Routes: routes[key]}
		if b.AddVPN(importer.VPN{Profile: p}) {
			names[key] = name
		}
		// The detection hints have no Hostbay field yet; external detection
		// finds the tunnel on its own. Values are not repeated here.
		var hints []string
		for _, h := range []struct{ field, value string }{{"match", v.Match}, {"exclude", v.Exclude}, {"interface", v.Iface}, {"peer", v.Peer}, {"kind", v.Kind}, {"SOCKS endpoint", v.Socks}} {
			if h.value != "" {
				hints = append(hints, h.field)
			}
		}
		if len(hints) > 0 {
			b.Note(name, "its TunnelHub detection settings ("+strings.Join(hints, ", ")+") have no Hostbay setting, so they were not imported; Hostbay detects the tunnel itself")
		}
	}
	res := b.Result()
	for _, ch := range c.Chrome {
		if names[ch.VPN] == "" {
			res.Skipped = append(res.Skipped, importer.Skipped{File: "chrome " + ch.Profile, Reason: fmt.Sprintf("it names VPN %q, which was not imported", ch.VPN)})
			continue
		}
		res.Browsers = append(res.Browsers, importer.BrowserMapping{Profile: ch.Profile, VPN: names[ch.VPN]})
	}
	return res, nil
}

// typeOf picks the Hostbay type for a TunnelHub VPN: a tunnel without a
// peer address is the SSL VPN (TunnelHub's one structural check), and the
// key or name tells the rest.
func typeOf(key string, v vpnDef) string {
	s := strings.ToLower(key + " " + v.Display)
	switch {
	case v.Kind == "peerless":
		return "sslvpn"
	case strings.Contains(s, "wireguard"):
		return "wireguard"
	case strings.Contains(s, "harmony") || strings.Contains(s, "perimeter") || strings.Contains(s, "p81"):
		return "perimeter81"
	case strings.Contains(s, "anyconnect") || strings.Contains(s, "cisco"):
		return "anyconnect"
	case strings.Contains(s, "globalprotect"):
		return "globalprotect"
	}
	return "other"
}
