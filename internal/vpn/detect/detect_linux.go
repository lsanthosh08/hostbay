package detect

import (
	"context"
	"net"
	"strings"
)

// tunnelPrefixes name the interfaces VPN clients bring up on Linux.
var tunnelPrefixes = []string{"tun", "tap", "wg", "ppp", "tailscale", "zt", "cscotun", "gpd", "nordlynx", "proton"}

// detect lists tunnel interfaces and NetworkManager's VPN connections, when
// nmcli is installed. Linux tunnels report no shape TunnelHub could use:
// nearly all are point-to-point, so shape alone never labels one here.
func detect(ctx context.Context) (Snapshot, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return Snapshot{}, err
	}
	var s Snapshot
	owner := map[string]string{}
	// -t escapes a colon inside a field as \:.
	for _, l := range strings.Split(run(ctx, "nmcli", "-t", "-f", "NAME,TYPE,DEVICE,ACTIVE", "connection", "show"), "\n") {
		f := strings.Split(strings.ReplaceAll(l, `\:`, "\x00"), ":")
		if len(f) != 4 || f[1] != "vpn" && f[1] != "wireguard" && f[1] != "tun" {
			continue
		}
		name := strings.ReplaceAll(f[0], "\x00", ":")
		v := VPN{Name: name, Type: TypeFor(name), Connected: f[3] == "yes"}
		if f[1] == "wireguard" {
			v.Type = "wireguard"
		}
		if v.Connected {
			// A plugin VPN's DEVICE is the link it runs over; its tunnel is
			// the connection's IP interface.
			v.Iface = f[2]
			if f[1] == "vpn" {
				v.Iface = strings.TrimSpace(run(ctx, "nmcli", "-g", "GENERAL.IP-IFACE", "connection", "show", name))
			}
			owner[v.Iface] = name
		}
		s.VPNs = append(s.VPNs, v)
	}
	for _, ifi := range ifs {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 ||
			ifi.Flags&net.FlagPointToPoint == 0 && !hasPrefix(ifi.Name, tunnelPrefixes) && owner[ifi.Name] == "" {
			continue
		}
		if local := ipv4(ifi); local != "" {
			s.Tunnels = append(s.Tunnels, Tunnel{Iface: ifi.Name, Local: local, VPN: owner[ifi.Name]})
		}
	}
	return s, nil
}

func hasPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// ipv4 is the interface's first IPv4 address, or "".
func ipv4(ifi net.Interface) string {
	addrs, _ := ifi.Addrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
			return n.IP.String()
		}
	}
	return ""
}
