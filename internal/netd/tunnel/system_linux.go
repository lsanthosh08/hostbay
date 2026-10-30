package tunnel

import (
	"net/netip"

	"golang.zx2c4.com/wireguard/tun"
)

func tunName(h string) string { return "hb" + h[:8] } // Linux allows 15 characters

// setAddrs puts addrs on the interface and brings it up, as wg-quick does.
func setAddrs(_ tun.Device, name string, addrs []netip.Prefix) error {
	for _, a := range addrs {
		family := "-6"
		if a.Addr().Is4() {
			family = "-4"
		}
		if err := command("ip", family, "address", "add", a.String(), "dev", name); err != nil {
			return err
		}
	}
	return command("ip", "link", "set", "dev", name, "up")
}
