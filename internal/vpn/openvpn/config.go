// Package openvpn builds the configuration hostbay-netd runs OpenVPN with,
// and speaks the OpenVPN management interface for Hostbay (U12, KTD8).
//
// The helper runs OpenVPN as root, so the configuration holds only what
// Build writes from typed fields plus the profile's own directives that pass
// an allowlist: nothing that names a file to write, a program or plugin to
// run, or a second management interface. A synced or restored profile can
// carry any text (store.VPNSecrets.OpenVPN), so Build checks every line in
// the helper, never trusting what import already checked.
package openvpn

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

// Profile is what Hostbay sends hostbay-netd to start OpenVPN.
type Profile struct {
	Remote string   `json:"remote"`
	Port   int      `json:"port,omitempty"` // 1194 when 0
	Routes []string `json:"routes,omitempty"`
	MTU    int      `json:"mtu,omitempty"`
	// UserPass asks for a user name and password, which Hostbay gives the
	// management interface: they never reach the helper or a file.
	UserPass bool `json:"userPass,omitempty"`
	// Extra is the profile's directives with no typed field and its inline
	// certificates (store.VPNSecrets.OpenVPN), one directive or <block> per
	// line.
	Extra string `json:"extra,omitempty"`
	// Key is the client certificate's private key, PEM.
	Key string `json:"key,omitempty"`
}

// Management is where the configuration puts the management interface: a
// Unix socket that only User may use, or on Windows a loopback port with a
// password file.
type Management struct {
	Socket, User   string
	Addr, PassFile string
}

// directives are the ones a profile may carry. It covers every directive
// the .ovpn import keeps (internal/importer/vpnconf): none names a file or
// a program. dev and dev-type are accepted for tun only, since Build writes
// its own; every other directive, script-security and the file writers
// (log, status, writepid ...) included, refuses the profile.
var directives = map[string]bool{
	"client": true, "dev": true, "dev-type": true, "proto": true, "nobind": true, "persist-key": true, "persist-tun": true,
	"resolv-retry": true, "remote-random": true, "connect-retry": true, "connect-retry-max": true, "server-poll-timeout": true, "float": true,
	"remote-cert-tls": true, "remote-cert-ku": true, "remote-cert-eku": true, "verify-x509-name": true, "ns-cert-type": true, "tls-client": true,
	"tls-version-min": true, "tls-version-max": true, "tls-cipher": true, "tls-ciphersuites": true, "tls-groups": true,
	"cipher": true, "data-ciphers": true, "data-ciphers-fallback": true, "ncp-ciphers": true, "ncp-disable": true, "auth": true, "key-direction": true,
	"comp-lzo": true, "compress": true, "allow-compression": true, "auth-nocache": true, "auth-retry": true, "reneg-sec": true,
	"verb": true, "mute": true, "mute-replay-warnings": true, "mssfix": true, "fragment": true, "explicit-exit-notify": true,
	"keepalive": true, "ping": true, "ping-restart": true, "pull": true, "pull-filter": true, "route-nopull": true, "route-metric": true,
	"block-outside-dns": true, "sndbuf": true, "rcvbuf": true, "topology": true, "peer-fingerprint": true, "static-challenge": true,
}

// blocks are the inline certificate and key blocks a profile may carry; a
// file reference in their place is refused.
var blocks = map[string]bool{"ca": true, "cert": true, "tls-auth": true, "tls-crypt": true, "tls-crypt-v2": true, "extra-certs": true}

// maxVerb keeps OpenVPN's log quiet enough that no packet or key material
// reaches it.
const maxVerb = 4

var (
	hostName = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
	userName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,63}$`)
)

// Build writes the configuration, or refuses the profile naming the line
// at fault.
func Build(p Profile, m Management) (string, error) {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	w("client")
	blockText, err := extras(p.Extra, w)
	if err != nil {
		return "", err
	}

	// Typed lines last: for a directive given twice OpenVPN keeps the last.
	if p.Remote == "" || !hostName.MatchString(p.Remote) && !isAddr(p.Remote) {
		return "", fmt.Errorf("%q is not a server name or address", p.Remote)
	}
	port := p.Port
	if port == 0 {
		port = 1194
	}
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("port %d is not between 1 and 65535", p.Port)
	}
	w("dev tun")
	w("remote %s %d", p.Remote, port)
	if p.MTU != 0 {
		if p.MTU < 576 || p.MTU > 9000 {
			return "", fmt.Errorf("MTU %d is not between 576 and 9000", p.MTU)
		}
		w("tun-mtu %d", p.MTU)
	}
	if err := management(m, w); err != nil {
		return "", err
	}
	if p.UserPass {
		w("auth-user-pass") // no file: OpenVPN asks the management interface
	}
	w("auth-retry none") // a refused password ends the run; it is sent once
	if err := routes(p.Routes, w); err != nil {
		return "", err
	}
	b.WriteString(blockText)
	if p.Key != "" {
		if err := blockBody(p.Key); err != nil {
			return "", fmt.Errorf("the private key: %w", err)
		}
		w("<key>\n%s\n</key>", strings.TrimSpace(p.Key))
	}
	// Without one of these OpenVPN stops at its options, and the run ends
	// before it opens the management socket with nothing to say why.
	if out := b.String(); !strings.Contains(out, "\n<ca>\n") && !strings.Contains(out, "\npeer-fingerprint ") {
		return "", errors.New("the profile has no CA certificate or peer fingerprint, so OpenVPN cannot check the server; import the .ovpn file from your server")
	}
	return b.String(), nil
}

// extras checks the profile's directives and writes them with w, returning
// its inline blocks to write at the end.
func extras(text string, w func(string, ...any)) (string, error) {
	var bs strings.Builder
	ls := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for i := 0; i < len(ls); i++ {
		line := strings.TrimSpace(ls[i])
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if tag, ok := strings.CutPrefix(line, "<"); ok {
			tag, ok = strings.CutSuffix(tag, ">")
			if !ok || !blocks[tag] {
				return "", fmt.Errorf("line %d of the profile is a block Hostbay does not allow%s", i+1, named(tag))
			}
			var body []string
			for i++; i < len(ls) && strings.TrimSpace(ls[i]) != "</"+tag+">"; i++ {
				body = append(body, strings.TrimRight(ls[i], "\r"))
			}
			if i == len(ls) {
				return "", fmt.Errorf("the profile's <%s> block has no end", tag)
			}
			text := strings.Join(body, "\n")
			if err := blockBody(text); err != nil {
				return "", fmt.Errorf("the profile's <%s> block: %w", tag, err)
			}
			fmt.Fprintf(&bs, "<%s>\n%s\n</%s>\n", tag, text, tag)
			continue
		}
		if strings.ContainsFunc(line, func(r rune) bool { return r < ' ' || r == 0x7f || r == '\\' }) {
			return "", fmt.Errorf("line %d of the profile has a backslash or control character", i+1)
		}
		f := strings.Fields(line)
		d := strings.ToLower(f[0])
		if !directives[d] {
			return "", fmt.Errorf("line %d of the profile is a directive Hostbay does not allow%s", i+1, named(d))
		}
		switch d {
		case "dev", "dev-type":
			if len(f) != 2 || !strings.HasPrefix(f[1], "tun") {
				return "", fmt.Errorf("line %d of the profile: Hostbay runs OpenVPN on a tun device only", i+1)
			}
			continue // Build writes dev tun itself
		case "verb":
			if n, err := strconv.Atoi(strings.Join(f[1:], " ")); err != nil || n < 0 || n > maxVerb {
				return "", fmt.Errorf("line %d of the profile: verb is at most %d", i+1, maxVerb)
			}
		}
		w("%s", line)
	}
	return bs.String(), nil
}

// blockBody refuses a block's text if any line could end the block early
// and start a directive of its own.
func blockBody(s string) error {
	if strings.TrimSpace(s) == "" {
		return errors.New("it is empty")
	}
	if strings.ContainsAny(s, "<\x00") || strings.ContainsFunc(s, func(r rune) bool { return r < ' ' && r != '\n' && r != '\r' && r != '\t' }) {
		return errors.New("it holds something other than certificate or key text")
	}
	return nil
}

func management(m Management, w func(string, ...any)) error {
	quotable := func(s string) bool {
		return s != "" && !strings.ContainsAny(s, "\"\\") && !strings.ContainsFunc(s, func(r rune) bool { return r < ' ' })
	}
	if m.Socket != "" {
		if !quotable(m.Socket) || !userName.MatchString(m.User) {
			return errors.New("the management socket or its user is not valid")
		}
		w("management \"%s\" unix", m.Socket)
		w("management-client-user %s", m.User)
	} else {
		ap, err := netip.ParseAddrPort(m.Addr)
		pf := strings.ReplaceAll(m.PassFile, `\`, "/") // OpenVPN reads a backslash as an escape
		if err != nil || !ap.Addr().IsLoopback() || !quotable(pf) {
			return errors.New("the management address or its password file is not valid")
		}
		w("management %s %d \"%s\"", ap.Addr(), ap.Port(), pf)
	}
	w("management-hold")
	w("management-query-passwords")
	return nil
}

// routes writes the profile's networks; a default route is OpenVPN's
// redirect-gateway def1, which adds the two half routes and keeps the
// server reachable through the local gateway.
func routes(rs []string, w func(string, ...any)) error {
	var redirect []string
	for _, r := range rs {
		p, err := netip.ParsePrefix(r)
		if err != nil {
			return fmt.Errorf("%q is not a network", r)
		}
		p = p.Masked()
		switch {
		case p.Bits() == 0 && p.Addr().Is4():
			redirect = append([]string{"def1"}, redirect...)
		case p.Bits() == 0:
			redirect = append(redirect, "ipv6")
		case p.Addr().Is4():
			w("route %s %s", p.Addr(), net.IP(net.CIDRMask(p.Bits(), 32)))
		default:
			w("route-ipv6 %s", p)
		}
	}
	if len(redirect) == 1 && redirect[0] == "ipv6" {
		redirect = append(redirect, "!ipv4")
	}
	if len(redirect) > 0 {
		w("redirect-gateway %s", strings.Join(redirect, " "))
	}
	return nil
}

func isAddr(s string) bool {
	a, err := netip.ParseAddr(s)
	return err == nil && a.Zone() == ""
}

var directiveName = regexp.MustCompile(`^-{0,2}[a-z]{1,24}(-[a-z0-9]{1,24}){0,3}$`)

// named is ": name" for an error, only when name looks like a directive: a
// line of a key whose block lost its open tag must never reach a message,
// which the UI and MCP clients show.
func named(name string) string {
	if !directiveName.MatchString(name) {
		return ""
	}
	return fmt.Sprintf(" (%s)", name)
}
