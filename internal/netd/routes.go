package netd

import (
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
)

// SysRoute is a route as the system table holds it. Gateway is the next hop,
// or invalid for a route on the interface itself.
type SysRoute struct {
	Dst     netip.Prefix `json:"dst"`
	Iface   string       `json:"iface"`
	Gateway netip.Addr   `json:"gateway,omitzero"`
}

func (r SysRoute) String() string {
	if r.Gateway.IsValid() {
		return fmt.Sprintf("%s via %s on %s", r.Dst, r.Gateway, r.Iface)
	}
	return fmt.Sprintf("%s on %s", r.Dst, r.Iface)
}

// Iface is a network interface and whether it is a tunnel: the only kind a
// route may be added through by name, or a vendor route deleted from.
type Iface struct {
	Name   string
	Tunnel bool
}

// Table is the system route table. Each platform has one (routes_*.go);
// tests use a fake.
type Table interface {
	// Routes lists the main table's IPv4 and IPv6 routes.
	Routes() ([]SysRoute, error)
	Interfaces() ([]Iface, error)
	Add(SysRoute) error
	Delete(SysRoute) error
	SetMTU(iface string, mtu int) error
}

// SystemTable is this computer's route table. Reading it needs no
// privilege; changes go through the helper.
func SystemTable() Table { return newTable() }

// isTunnel decides from what the system reports whether an interface is a
// tunnel. macOS names interfaces by driver, so a point-to-point utun, ppp or
// ipsec is one and the always-present gif and stf are not. Linux tunnels take
// any name (cscotun0, tailscale0) but are all point-to-point. Windows has no
// such flag: a PPP, tunnel or Wintun-style virtual interface type is a
// tunnel, and so is an Ethernet-typed adapter whose driver names a VPN, as
// AnyConnect's and TAP-Windows' do.
func isTunnel(goos, name string, pointToPoint bool, ifType uint32, desc string) bool {
	switch goos {
	case "darwin":
		for _, p := range []string{"utun", "ppp", "ipsec", "tun", "wg"} {
			if pointToPoint && strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	case "windows":
		const ifTypePPP, ifTypePropVirtual, ifTypeTunnel = 23, 53, 131
		if ifType == ifTypePPP || ifType == ifTypePropVirtual || ifType == ifTypeTunnel {
			return true
		}
		desc = strings.ToLower(desc)
		for _, s := range []string{"vpn", "wintun", "wireguard", "tap-windows", "anyconnect", "fortinet", "pangp", "zscaler"} {
			if strings.Contains(desc, s) {
				return true
			}
		}
		return false
	default:
		return pointToPoint
	}
}

// DefaultGateway finds the local default gateway for dst's family: the
// default route with a next hop on a physical interface, so a full-tunnel
// VPN's own default route is never taken for it.
func DefaultGateway(routes []SysRoute, tunnel map[string]bool, dst netip.Prefix) (SysRoute, bool) {
	for _, r := range routes {
		if r.Dst.Bits() == 0 && r.Dst.Addr().Is4() == dst.Addr().Is4() && r.Gateway.IsValid() && !tunnel[r.Iface] {
			return r, true
		}
	}
	return SysRoute{}, false
}

// runTool runs a system tool by absolute path with argv, never a shell, and
// returns its output as the error when it fails.
func runTool(path string, args ...string) error {
	out, err := exec.Command(path, args...).CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return errors.New(msg)
		}
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
