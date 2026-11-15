package tunnel

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"golang.zx2c4.com/wireguard/tun"

	"hostbay/internal/vpn/openconnect"
)

func tunName(string) string { return "utun" } // the kernel numbers it

// setAddrs puts addrs on the point-to-point utun, as wg-quick does.
func setAddrs(_ tun.Device, name string, addrs []netip.Prefix) error {
	for _, a := range addrs {
		args := []string{name, "inet6", a.String(), "alias"}
		if a.Addr().Is4() {
			args = []string{name, "inet", a.String(), a.Addr().String(), "alias"}
		}
		if err := command("/sbin/ifconfig", args...); err != nil {
			return err
		}
	}
	return command("/sbin/ifconfig", name, "up")
}

var utunName = regexp.MustCompile(`^utun[0-9]+$`)

// ocTarget is the utun openconnect runs on and its script, the helper
// itself with that utun's name. openconnect runs the script through
// /bin/sh -c, so the script is built only from values the helper owns. A
// profile with no pinned certificate trusts macOS's root-owned CA bundle,
// not the user-writable one compiled into openconnect's libraries. taken
// are the utuns given to tunnels still signing in, which the system does
// not list yet.
func ocTarget(dir, _ string, taken []string) (iface, script, caFile string, err error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return "", "", "", fmt.Errorf("listing interfaces: %w", err)
	}
	names := taken
	for _, i := range ifs {
		names = append(names, i.Name)
	}
	iface = freeUtun(names)
	return iface, openconnect.ShellQuote(filepath.Join(dir, "hostbay-netd")) + " oc-script " + iface, "/etc/ssl/cert.pem", nil
}

// freeUtun is the lowest utun no interface in names has, above the units
// the system and other VPN clients usually take first.
func freeUtun(names []string) string {
	for n := 10; ; n++ {
		if u := fmt.Sprintf("utun%d", n); !slices.Contains(names, u) {
			return u
		}
	}
}

// createAdapter: openconnect creates the utun itself, and it goes when
// openconnect exits.
func createAdapter(string) (io.Closer, error) { return io.NopCloser(nil), nil }

// Script is "hostbay-netd oc-script <utun>", openconnect's script: on
// connect it puts the tunnel's addresses on the utun the helper chose
// (parseScript). Routes and MTU come from Hostbay; nothing else is changed.
func Script(args []string) error {
	iface, addrs, err := parseScript(args, os.Getenv)
	if err != nil || iface == "" {
		return err
	}
	return setAddrs(nil, iface, addrs)
}

// parseScript reads openconnect's environment for Script (see
// vpnc-script). It acts only on a connect, only for the utun named as its
// one argument, and only with the tunnel's own addresses, each as a host
// prefix whatever netmask the gateway sent, and each one a tunnel can
// carry: no loopback, unspecified, multicast, broadcast or link-local
// address. Anything else is an error, and nothing is configured.
func parseScript(args []string, getenv func(string) string) (string, []netip.Prefix, error) {
	if getenv("reason") != "connect" {
		return "", nil, nil
	}
	if len(args) != 1 || !utunName.MatchString(args[0]) || getenv("TUNDEV") != args[0] {
		return "", nil, fmt.Errorf("openconnect named a tunnel other than the helper's: %q", getenv("TUNDEV"))
	}
	var addrs []netip.Prefix
	for i, v := range []string{getenv("INTERNAL_IP4_ADDRESS"), cmp.Or(getenv("INTERNAL_IP6_ADDRESS"), getenv("INTERNAL_IP6_NETMASK"))} {
		if v == "" {
			continue
		}
		s, _, _ := strings.Cut(v, "/")
		a, err := netip.ParseAddr(s)
		if err != nil || a.Is4() != (i == 0) || a.Is4In6() || a.Zone() != "" || !a.IsGlobalUnicast() {
			return "", nil, fmt.Errorf("openconnect gave the tunnel an address it cannot carry: %q", v)
		}
		addrs = append(addrs, netip.PrefixFrom(a, a.BitLen()))
	}
	if len(addrs) == 0 {
		return "", nil, errors.New("openconnect gave the tunnel no address")
	}
	return args[0], addrs, nil
}
