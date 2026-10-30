// Package openconnect is hostbay-vpn's engine for the VPN types the
// openconnect program speaks (KTD3): AnyConnect, the SSL VPN (PPP over TLS),
// GlobalProtect, Pulse, Network Connect, F5 and Array. hostbay-vpn runs
// openconnect in its no-admin mode (--script-tun), takes the tunnel's packet
// socket from the short-lived "hostbay-vpn tun-fd" child openconnect starts,
// and puts a gVisor stack on it (internal/vpn/tunfd), so the tunnel serves
// Hostbay only (R14). Only cmd/hostbay-vpn imports it.
package openconnect

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"hostbay/internal/vpn/proto"
)

// browserTypes can sign in in the user's browser (R20): openconnect's
// --external-browser serves only AnyConnect's external-browser SSO mode.
var browserTypes = map[string]bool{proto.TypeAnyConnect: true}

// minMajor.minMinor is the oldest openconnect accepted. The fortinet, f5
// and array protocols arrived in 8.20 and --external-browser in 9.00, but
// 9.10 fixed Array packets split across TLS frames and F5's JSON-only login
// forms, 9.11 GlobalProtect ESP, and 9.12 raised the stdin line limit
// --passwd-on-stdin reads through. 9.12 is what Homebrew and current
// distributions ship.
const minMajor, minMinor = 9, 12

const defaultMTU = 1300 // when the gateway names none; small, so SSH never stalls (R28)

// config is a profile's non-secret settings, as Hostbay sends them in the
// start message's Config. It is a fixed set of fields: a synced profile must
// never carry openconnect options, since --script runs a program.
type config struct {
	Gateway    string `json:"gateway"`              // host, host:port, or host/path as openconnect takes it
	User       string `json:"user,omitempty"`       // sign-in name
	ServerCert string `json:"serverCert,omitempty"` // the pinned gateway, "pin-sha256:..." (R21)
	Browser    bool   `json:"browser,omitempty"`    // sign in in the user's browser (R20)
}

// launch is one prepared openconnect run.
type launch struct {
	typ      string
	cfg      config
	password string
}

var pinRE = regexp.MustCompile(`pin-sha256:[A-Za-z0-9+/]{43}=`)

// prepare checks a start message's type, Config and Secrets
// (secrets["password"]). No error names the password.
func prepare(typ, conf string, secrets map[string]string) (launch, error) {
	l := launch{typ: typ, password: secrets["password"]}
	if !proto.IsOpenConnect(typ) {
		return l, fmt.Errorf("OpenConnect cannot run VPN type %q.", typ)
	}
	dec := json.NewDecoder(strings.NewReader(conf))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&l.cfg); err != nil {
		return l, fmt.Errorf("The VPN profile's settings are not valid: %v", err)
	}
	c := l.cfg
	switch {
	case c.Gateway == "" || strings.HasPrefix(c.Gateway, "-") || strings.ContainsFunc(c.Gateway, unsafe):
		return l, fmt.Errorf("Gateway %q is not a host name or address.", c.Gateway)
	case strings.ContainsFunc(c.User, func(r rune) bool { return r < ' ' || r == 0x7f }):
		return l, errors.New("The user name has a control character.")
	case c.ServerCert != "" && pinRE.FindString(c.ServerCert) != c.ServerCert:
		return l, fmt.Errorf("The pinned gateway certificate %q is not a pin-sha256 fingerprint.", c.ServerCert)
	case c.Browser && !browserTypes[typ]:
		return l, errors.New("This VPN type has no browser sign-in.")
	case strings.ContainsAny(l.password, "\r\n"):
		// openconnect reads one line; the rest would answer its next prompt.
		return l, errors.New("The password has a line break.")
	}
	return l, nil
}

func unsafe(r rune) bool { return r <= ' ' || r == 0x7f }

// args is openconnect's command line. self is hostbay-vpn's absolute path,
// which openconnect runs through /bin/sh -c as the tun script, and execs
// as the browser. No secret is ever an argument: the password goes on
// stdin.
func (l launch) args(self string) []string {
	a := l.signIn([]string{"--protocol=" + proto.OpenConnectTypes[l.typ], "--non-inter", "--script-tun", "--script=" + shellQuote(self) + " tun-fd"})
	if l.cfg.Browser {
		a = append(a, "--external-browser="+self)
	}
	return append(a, "--", l.cfg.Gateway)
}

// SystemArgs is openconnect's command line for a system-wide run under
// hostbay-netd on Windows (U12): the tunnel on the Wintun adapter iface,
// configured by script, a command line Windows runs without a shell. The
// start message is checked as for an app-scoped run; browser sign-in is
// refused, since the helper runs as SYSTEM with no browser. The password,
// if any, goes on stdin.
func SystemArgs(typ, conf string, secrets map[string]string, iface, script string) ([]string, error) {
	l, err := prepare(typ, conf, secrets)
	if err != nil {
		return nil, err
	}
	if l.cfg.Browser {
		return nil, errors.New("A system-wide VPN cannot sign in in the browser.")
	}
	a := l.signIn([]string{"--protocol=" + proto.OpenConnectTypes[l.typ], "--non-inter", "--interface=" + iface, "--script=" + script})
	return append(a, "--", l.cfg.Gateway), nil
}

// signIn appends the user name, the password's stdin flag and the pinned
// gateway certificate to a.
func (l launch) signIn(a []string) []string {
	if l.cfg.User != "" {
		a = append(a, "--user="+l.cfg.User)
	}
	if l.password != "" {
		a = append(a, "--passwd-on-stdin")
	}
	if l.cfg.ServerCert != "" {
		// This also turns off system trust: only this certificate is accepted.
		a = append(a, "--servercert="+l.cfg.ServerCert)
	}
	return a
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

var versionRE = regexp.MustCompile(`version v?(\d+)\.(\d+)`)

// checkVersion refuses an openconnect, at path, whose --version output out
// is older than minMajor.minMinor.
func checkVersion(path, out string) error {
	m := versionRE.FindStringSubmatch(out)
	if m == nil {
		return fmt.Errorf("Cannot tell which version %s is; Hostbay needs OpenConnect %d.%02d or newer.", path, minMajor, minMinor)
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	if major < minMajor || major == minMajor && minor < minMinor {
		return fmt.Errorf("Hostbay needs OpenConnect %d.%02d or newer; %s is %d.%02d.", minMajor, minMinor, path, major, minor)
	}
	return nil
}

// findOpenConnect looks beside hostbay-vpn, where Hostbay's package puts
// it, then on Linux on PATH.
func findOpenConnect() (string, error) {
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "openconnect")
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	if runtime.GOOS == "linux" {
		if p, err := exec.LookPath("openconnect"); err == nil {
			return p, nil
		}
		return "", errors.New("OpenConnect is not installed: Hostbay found it neither beside hostbay-vpn nor on PATH.")
	}
	return "", errors.New("OpenConnect is not installed beside hostbay-vpn.")
}

// netConfig is the tunnel as openconnect describes it to its script.
type netConfig struct {
	addrs  []netip.Addr
	dns    []netip.Addr
	search []string
	mtu    int
	routes []string // CIDR
}

// scriptVar reports whether the tun-fd child passes an environment
// variable on: only the ones parseNet reads.
func scriptVar(name string) bool {
	for _, p := range []string{"INTERNAL_IP4_", "INTERNAL_IP6_", "CISCO_SPLIT_INC", "CISCO_IPV6_SPLIT_INC", "CISCO_DEF_DOMAIN"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// parseNet reads the variables openconnect sets for its script (see
// vpnc-script): the tunnel's addresses, DNS servers and search domains, MTU,
// and split-include routes, everything when there are none.
func parseNet(env map[string]string) (netConfig, error) {
	n := netConfig{mtu: defaultMTU}
	for _, k := range []string{"INTERNAL_IP4_ADDRESS", "INTERNAL_IP6_ADDRESS"} {
		if v := env[k]; v != "" {
			a, err := netip.ParseAddr(strings.Split(v, "/")[0])
			if err != nil {
				return n, fmt.Errorf("The VPN gave an address Hostbay cannot read: %q.", v)
			}
			n.addrs = append(n.addrs, a)
		}
	}
	if len(n.addrs) == 0 {
		return n, errors.New("The VPN gave Hostbay no address.")
	}
	for _, k := range []string{"INTERNAL_IP4_DNS", "INTERNAL_IP6_DNS"} {
		for _, s := range strings.Fields(env[k]) {
			if a, err := netip.ParseAddr(s); err == nil {
				n.dns = append(n.dns, a)
			}
		}
	}
	n.search = strings.Fields(strings.ReplaceAll(env["CISCO_DEF_DOMAIN"], ",", " "))
	if m, err := strconv.Atoi(env["INTERNAL_IP4_MTU"]); err == nil && m >= 576 && m <= 65535 {
		n.mtu = m
	}
	for _, k := range []string{"CISCO_SPLIT_INC", "CISCO_IPV6_SPLIT_INC"} {
		count, _ := strconv.Atoi(env[k])
		for i := range count {
			p, err := netip.ParsePrefix(env[fmt.Sprintf("%s_%d_ADDR", k, i)] + "/" + env[fmt.Sprintf("%s_%d_MASKLEN", k, i)])
			if err == nil {
				n.routes = append(n.routes, p.Masked().String())
			}
		}
	}
	if len(n.routes) == 0 {
		n.routes = []string{"0.0.0.0/0"}
		if n.addrs[len(n.addrs)-1].Is6() {
			n.routes = append(n.routes, "::/0")
		}
	}
	return n, nil
}

// address is the tunnel's own addresses as CIDR.
func (n netConfig) address() []string {
	s := make([]string, len(n.addrs))
	for i, a := range n.addrs {
		s[i] = netip.PrefixFrom(a, a.BitLen()).String()
	}
	return s
}
