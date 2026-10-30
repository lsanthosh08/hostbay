package vpnconf

import (
	"errors"
	"strconv"
	"strings"

	"hostbay/internal/importer"
	"hostbay/internal/store"
)

// wgEntry parses a WireGuard configuration into an app-scoped profile: the
// interface's addresses, key and DNS, and its peer's key, endpoint and
// allowed networks. Hook commands (PostUp and the like) are dropped.
func wgEntry(name, text string) (importer.VPN, error) {
	secs := parseINI(text)
	iface := find(secs, "Interface")
	if iface.name == "" {
		return importer.VPN{}, errors.New("this is not a WireGuard configuration: it has no [Interface]")
	}
	p := newProfile(name, "wireguard", store.VPNTierApp)
	p.Port = 51820
	var sec store.VPNSecrets
	var ignored, dnsList []string
	for _, k := range iface.keys {
		switch strings.ToLower(k.key) {
		case "privatekey":
			sec.PrivateKey = k.value
		case "address":
			for _, a := range list(k.value) {
				if c, ok := address(a); ok {
					p.Address = append(p.Address, c)
				} else {
					ignored = append(ignored, clip(k.line))
				}
			}
		case "dns":
			dnsList = append(dnsList, list(k.value)...)
		case "mtu":
			p.MTU, _ = strconv.Atoi(k.value)
		default:
			ignored = append(ignored, clip(k.line))
		}
	}
	p.DNSServers, p.DNSDomains = dns(dnsList)
	peers := 0
	for _, s := range secs {
		if !strings.EqualFold(s.name, "Peer") {
			continue
		}
		if peers++; peers > 1 {
			ignored = append(ignored, "[Peer] "+s.get("PublicKey"))
			continue
		}
		for _, k := range s.keys {
			switch strings.ToLower(k.key) {
			case "publickey":
				p.PeerKey = k.value
			case "presharedkey":
				sec.PresharedKey = k.value
			case "endpoint":
				p.Gateway, p.Port = hostPort(k.value, 51820)
			case "allowedips":
				for _, r := range list(k.value) {
					if c, ok := network(r); ok {
						p.Routes = append(p.Routes, c)
					} else {
						ignored = append(ignored, clip(k.line))
					}
				}
			default:
				ignored = append(ignored, clip(k.line))
			}
		}
	}
	return importer.VPN{Profile: p, Secrets: sec, Ignored: ignored}, nil
}
