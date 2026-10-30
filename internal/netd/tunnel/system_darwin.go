package tunnel

import (
	"net/netip"

	"golang.zx2c4.com/wireguard/tun"
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
