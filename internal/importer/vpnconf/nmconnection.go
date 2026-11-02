package vpnconf

import (
	"errors"
	"fmt"
	"strings"

	"hostbay/internal/importer"
	"hostbay/internal/store"
)

// nmServices maps NetworkManager VPN plugins to Hostbay types. Internal
// only: a plugin's name can carry a vendor's name, which the SSL VPN type
// never shows, so no message repeats it.
var nmServices = map[string]struct{ typ, tier string }{
	"org.freedesktop.NetworkManager.openvpn":     {"openvpn", store.VPNTierSystem},
	"org.freedesktop.NetworkManager.vpnc":        {"ipsec", store.VPNTierSystem},
	"org.freedesktop.NetworkManager.l2tp":        {"ipsec", store.VPNTierSystem},
	"org.freedesktop.NetworkManager.fortisslvpn": {"sslvpn", store.VPNTierApp},
	"org.freedesktop.NetworkManager.openconnect": {"anyconnect", store.VPNTierApp},
}

// OpenConnect's protocol names, as NetworkManager stores them.
var ocProtocols = map[string]string{"anyconnect": "anyconnect", "nc": "nc", "gp": "globalprotect", "pulse": "pulse", "f5": "f5", "fortinet": "sslvpn", "array": "array"}

// nmconnection imports a NetworkManager keyfile connection: WireGuard, or
// a VPN plugin's connection, with its ipv4 and ipv6 routes and DNS.
func nmconnection(b *importer.Builder, fileName, text string) error {
	secs := parseINI(text)
	conn := find(secs, "connection")
	name := conn.get("id")
	if name == "" {
		name = fileName
	}
	var p store.VPN
	var sec store.VPNSecrets
	var ignored []string
	switch conn.get("type") {
	case "wireguard":
		p = newProfile(name, "wireguard", store.VPNTierApp)
		p.Port = 51820
		wg := find(secs, "wireguard")
		sec.PrivateKey = wg.get("private-key")
		for _, s := range secs {
			pub, ok := strings.CutPrefix(s.name, "wireguard-peer.")
			if !ok {
				continue
			}
			if p.PeerKey != "" {
				ignored = append(ignored, "[wireguard-peer] "+pub)
				continue
			}
			p.PeerKey, sec.PresharedKey = pub, s.get("preshared-key")
			p.Gateway, p.Port = hostPort(s.get("endpoint"), 51820)
			for _, r := range list(s.get("allowed-ips")) {
				if c, ok := network(r); ok {
					p.Routes = append(p.Routes, c)
				}
			}
		}
	case "vpn":
		vpn, vs := find(secs, "vpn"), find(secs, "vpn-secrets")
		svc, ok := nmServices[vpn.get("service-type")]
		if !ok {
			b.Skip(name, "its VPN plugin is not one Hostbay imports")
			return nil
		}
		p = newProfile(name, svc.typ, svc.tier)
		p.Auth = "password"
		used := map[string]bool{"service-type": true}
		take := func(k string) string { used[k] = true; return vpn.get(k) }
		switch svc.typ {
		case "openvpn":
			host, _, _ := strings.Cut(take("remote"), ",")
			p.Gateway, p.Port = hostPort(host, 1194)
			p.User = take("username")
			if take("connection-type") == "tls" {
				p.Auth = "certificate"
			}
			if take("proto-tcp") == "yes" {
				sec.OpenVPN = "proto tcp-client"
			}
			sec.Password = vs.get("password")
			// NetworkManager keeps its ca, cert and key as paths on that computer,
			// which are not read; say so here rather than at the first connect.
			if n := noCANote(sec.OpenVPN); n != "" {
				ignored = append(ignored, n)
			}
		case "ipsec":
			if g := take("IPSec gateway"); g != "" { // vpnc
				p.Gateway, p.User = g, take("Xauth username")
				sec.Password, sec.PresharedKey = vs.get("Xauth password"), vs.get("IPSec secret")
			} else { // l2tp
				p.Gateway, p.User = take("gateway"), take("user")
				sec.Password, sec.PresharedKey = vs.get("password"), vs.get("ipsec-psk")
			}
		default: // OpenConnect protocols and the SSL VPN
			if proto := take("protocol"); proto != "" {
				if p.Type, ok = ocProtocols[proto]; !ok {
					b.Skip(name, "its OpenConnect protocol is not one Hostbay imports")
					return nil
				}
			}
			gw := take("gateway")
			if i := strings.Index(gw, "://"); i >= 0 {
				gw = gw[i+3:]
			}
			gw, _, _ = strings.Cut(gw, "/")
			p.Gateway, p.Port = hostPort(gw, 443)
			p.User = take("user")
			if p.User == "" {
				p.User = take("username")
			}
			sec.Password = vs.get("password")
		}
		for _, k := range vpn.keys {
			// *-flags only say where NetworkManager keeps a secret.
			if !used[k.key] && !strings.HasSuffix(k.key, "-flags") {
				ignored = append(ignored, clip(k.line))
			}
		}
	default:
		return errors.New("this NetworkManager connection is not a VPN")
	}
	full := conn.get("type") != "wireguard"
	var dnsList []string
	for _, family := range []string{"ipv4", "ipv6"} {
		ip := find(secs, family)
		if ip.get("never-default") == "true" {
			full = false
		}
		dnsList = append(dnsList, list(ip.get("dns"))...)
		dnsList = append(dnsList, list(ip.get("dns-search"))...)
		for i := 1; ; i++ {
			r := ip.get(fmt.Sprintf("route%d", i))
			if r == "" {
				break
			}
			dest, _, _ := strings.Cut(r, ",")
			if c, ok := network(dest); ok {
				p.Routes = append(p.Routes, c)
			} else {
				ignored = append(ignored, clip(fmt.Sprintf("route%d=%s", i, r)))
			}
		}
		if conn.get("type") == "wireguard" {
			for i := 1; ; i++ {
				a := ip.get(fmt.Sprintf("address%d", i))
				if a == "" {
					break
				}
				a, _, _ = strings.Cut(a, ",")
				if c, ok := address(a); ok {
					p.Address = append(p.Address, c)
				}
			}
		}
	}
	if full {
		p.Routes = append(append([]string{}, fullTunnel...), p.Routes...)
	}
	p.DNSServers, p.DNSDomains = dns(dnsList)
	b.AddVPN(importer.VPN{Profile: p, Secrets: sec, Ignored: ignored})
	return nil
}
