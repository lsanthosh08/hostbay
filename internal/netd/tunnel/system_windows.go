package tunnel

import (
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"strconv"

	"golang.zx2c4.com/wintun"
	"golang.zx2c4.com/wireguard/tun"
	"golang.zx2c4.com/wireguard/windows/tunnel/winipcfg"
)

func tunName(h string) string { return "Hostbay " + h[:8] }

// setAddrs puts addrs on the Wintun adapter.
func setAddrs(dev tun.Device, _ string, addrs []netip.Prefix) error {
	nt, ok := dev.(*tun.NativeTun)
	if !ok {
		return errors.New("the tunnel is not a Wintun adapter")
	}
	return winipcfg.LUID(nt.LUID()).SetIPAddresses(addrs)
}

// createAdapter creates the Wintun adapter openconnect opens by name;
// closing it removes it.
func createAdapter(name string) (io.Closer, error) {
	a, err := wintun.CreateAdapter(name, "Hostbay", nil)
	if err != nil {
		return nil, err
	}
	return a, nil
}

// Script is "hostbay-netd oc-script", openconnect's script: on connect it
// puts the tunnel's addresses, from openconnect's environment (see
// vpnc-script), on the adapter. Routes come from Hostbay (U11); nothing
// else is changed.
func Script() error {
	if os.Getenv("reason") != "connect" {
		return nil
	}
	idx, err := strconv.ParseUint(os.Getenv("TUNIDX"), 10, 32)
	if err != nil {
		return fmt.Errorf("openconnect named no adapter: %v", err)
	}
	luid, err := winipcfg.LUIDFromIndex(uint32(idx))
	if err != nil {
		return err
	}
	var addrs []netip.Prefix
	if a, err := netip.ParseAddr(os.Getenv("INTERNAL_IP4_ADDRESS")); err == nil {
		bits, err := strconv.Atoi(os.Getenv("INTERNAL_IP4_NETMASKLEN"))
		if err != nil || bits < 1 || bits > 32 {
			bits = 32
		}
		addrs = append(addrs, netip.PrefixFrom(a, bits))
	}
	if p, err := netip.ParsePrefix(os.Getenv("INTERNAL_IP6_NETMASK")); err == nil {
		addrs = append(addrs, p)
	}
	if len(addrs) == 0 {
		return errors.New("openconnect gave the tunnel no address")
	}
	return luid.SetIPAddresses(addrs)
}
