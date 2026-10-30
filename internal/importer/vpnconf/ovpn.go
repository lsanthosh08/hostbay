package vpnconf

import (
	"net/netip"
	"strconv"
	"strings"

	"hostbay/internal/importer"
	"hostbay/internal/store"
)

// ovpnKept are the directives the helper's OpenVPN allowlist accepts that
// have no typed profile field; they are kept as written. Everything else
// that no typed field holds is dropped and reported: script hooks (up,
// down, script-security, plugin, ...), file writers (log, status,
// writepid, ...) and any directive Hostbay does not know (R43).
var ovpnKept = map[string]bool{
	"client": true, "dev": true, "dev-type": true, "proto": true, "nobind": true, "persist-key": true, "persist-tun": true,
	"resolv-retry": true, "remote-random": true, "connect-retry": true, "connect-retry-max": true, "server-poll-timeout": true, "float": true,
	"remote-cert-tls": true, "remote-cert-ku": true, "remote-cert-eku": true, "verify-x509-name": true, "ns-cert-type": true, "tls-client": true,
	"tls-version-min": true, "tls-version-max": true, "tls-cipher": true, "tls-ciphersuites": true, "tls-groups": true,
	"cipher": true, "data-ciphers": true, "data-ciphers-fallback": true, "ncp-ciphers": true, "ncp-disable": true, "auth": true, "key-direction": true,
	"comp-lzo": true, "compress": true, "allow-compression": true, "auth-nocache": true, "auth-retry": true, "reneg-sec": true,
	"verb": true, "mute": true, "mute-replay-warnings": true, "mssfix": true, "fragment": true, "explicit-exit-notify": true,
	"keepalive": true, "ping": true, "ping-restart": true, "pull": true, "pull-filter": true, "route-nopull": true, "route-metric": true,
	"block-outside-dns": true, "sndbuf": true, "rcvbuf": true, "topology": true,
}

// ovpnBlocks are the certificate and key material kept, inline or read
// from a file beside the configuration.
var ovpnBlocks = map[string]bool{"ca": true, "cert": true, "key": true, "tls-auth": true, "tls-crypt": true, "tls-crypt-v2": true, "extra-certs": true}

// ovpn parses an OpenVPN configuration into a system-wide profile. dir is
// the folder file references are read from.
// shortcut: arguments split on spaces, so a quoted argument with spaces is
// dropped as unknown; parse OpenVPN quoting if real profiles use it.
func ovpn(b *importer.Builder, name, text, dir string) {
	p := newProfile(name, "openvpn", store.VPNTierSystem)
	p.Port = 1194
	var sec store.VPNSecrets
	var kept, ignored, routes []string
	full, userpass, cert, remotes := false, false, false, 0

	block := func(tag, content string) {
		switch tag {
		case "key":
			sec.PrivateKey = content
		case "auth-user-pass": // OpenVPN 2.6: user and password inline
			lines := strings.SplitN(content, "\n", 3)
			p.User, userpass = strings.TrimSpace(lines[0]), true
			if len(lines) > 1 {
				sec.Password = strings.TrimSpace(lines[1])
			}
		default:
			if !ovpnBlocks[tag] {
				ignored = append(ignored, "<"+tag+">") // its content can hold anything, such as a proxy password
				return
			}
			cert = cert || tag == "cert"
			kept = append(kept, "<"+tag+">\n"+content+"\n</"+tag+">")
		}
	}

	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(strings.TrimRight(lines[i], "\r"))
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if strings.HasPrefix(line, "<") && strings.HasSuffix(line, ">") && !strings.HasPrefix(line, "</") {
			tag := strings.ToLower(line[1 : len(line)-1])
			var body []string
			for i++; i < len(lines) && !strings.EqualFold(strings.TrimSpace(lines[i]), "</"+tag+">"); i++ {
				body = append(body, strings.TrimRight(lines[i], "\r"))
			}
			block(tag, strings.Join(body, "\n"))
			continue
		}
		f := strings.Fields(line)
		d, args := strings.ToLower(f[0]), f[1:]
		ok := true
		switch {
		case d == "remote" && len(args) > 0 && remotes == 0:
			remotes++
			p.Gateway = args[0]
			if len(args) > 1 {
				p.Port, ok = port(args[1])
			}
			if len(args) > 2 {
				kept = append(kept, "proto "+args[2])
			}
		case d == "port" && len(args) == 1:
			p.Port, ok = port(args[0])
		case d == "route" && len(args) > 0:
			var r string
			if r, ok = maskRoute(args); ok {
				routes = append(routes, r)
			}
		case d == "route-ipv6" && len(args) > 0:
			var r string
			if r, ok = network(args[0]); ok {
				routes = append(routes, r)
			}
		case d == "redirect-gateway":
			full = true
		case d == "dhcp-option" && len(args) == 2:
			switch strings.ToUpper(args[0]) {
			case "DNS", "DNS6":
				p.DNSServers = append(p.DNSServers, args[1])
			case "DOMAIN", "DOMAIN-SEARCH", "ADAPTER_DOMAIN_SUFFIX":
				p.DNSDomains = append(p.DNSDomains, args[1])
			default:
				ok = false
			}
		case d == "tun-mtu" && len(args) == 1:
			p.MTU, _ = strconv.Atoi(args[0])
		case d == "auth-user-pass":
			// A credentials file is never read: the password is asked for.
			userpass = true
		case ovpnBlocks[d] && len(args) > 0:
			if args[0] == "[inline]" {
				continue // the block follows
			}
			content, err := readRef(dir, args[0])
			if ok = err == nil; ok {
				block(d, content)
				if d == "tls-auth" && len(args) > 1 {
					kept = append(kept, "key-direction "+args[1])
				}
			}
		case ovpnKept[d]:
			kept = append(kept, line)
		default:
			ok = false
		}
		if !ok {
			ignored = append(ignored, clip(line))
		}
	}
	if p.Gateway == "" {
		b.Skip(name, "it names no server (no remote line)")
		return
	}
	if full {
		routes = append(append([]string{}, fullTunnel...), routes...)
	}
	p.Routes = routes
	switch {
	case userpass:
		p.Auth = "password"
	case cert || sec.PrivateKey != "":
		p.Auth = "certificate"
	}
	sec.OpenVPN = strings.Join(kept, "\n")
	b.AddVPN(importer.VPN{Profile: p, Secrets: sec, Ignored: ignored})
}

func port(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	return n, err == nil && n >= 1 && n <= 65535
}

// maskRoute reads "route network [netmask ...]" as a CIDR; a network
// given by name or a non-contiguous mask is not a route Hostbay keeps.
func maskRoute(args []string) (string, bool) {
	a, err := netip.ParseAddr(args[0])
	if err != nil || !a.Is4() {
		return "", false
	}
	bits := 32
	if len(args) > 1 {
		m, err := netip.ParseAddr(args[1])
		if err != nil || !m.Is4() {
			return "", false
		}
		v := m.As4()
		n := uint32(v[0])<<24 | uint32(v[1])<<16 | uint32(v[2])<<8 | uint32(v[3])
		ones := 0
		for n&0x80000000 != 0 {
			ones, n = ones+1, n<<1
		}
		if n != 0 {
			return "", false
		}
		bits = ones
	}
	return netip.PrefixFrom(a, bits).Masked().String(), true
}
