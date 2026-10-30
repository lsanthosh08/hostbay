package detect

import (
	"context"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// detect reads every network adapter: a VPN client installs its own, whose
// description names the vendor ("Fortinet SSL VPN Virtual Ethernet
// Adapter"), and it is up while the VPN is connected. Windows tunnels have
// no shape TunnelHub could use, so shape alone never labels one here.
func detect(ctx context.Context) (Snapshot, error) {
	var s Snapshot
	size := uint32(15 << 10)
	var buf []byte
	for {
		buf = make([]byte, size)
		err := windows.GetAdaptersAddresses(windows.AF_INET, 0, 0, (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0])), &size)
		if err == nil {
			break
		}
		if err != windows.ERROR_BUFFER_OVERFLOW {
			return Snapshot{}, err
		}
	}
	for a := (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0])); a != nil; a = a.Next {
		desc := windows.UTF16PtrToString(a.Description)
		name := windows.UTF16PtrToString(a.FriendlyName)
		vendor := TypeFor(desc) != "other" || containsAny(strings.ToLower(desc), "vpn", "tap-windows", "wintun")
		tunnel := vendor || a.IfType == windows.IF_TYPE_PPP || a.IfType == windows.IF_TYPE_TUNNEL || a.IfType == 53 // IF_TYPE_PROP_VIRTUAL
		up := a.OperStatus == windows.IfOperStatusUp
		if vendor {
			v := VPN{Name: desc, Type: TypeFor(desc), Connected: up}
			if up {
				v.Iface = name
			}
			s.VPNs = append(s.VPNs, v)
		}
		if !tunnel || !up {
			continue
		}
		for u := a.FirstUnicastAddress; u != nil; u = u.Next {
			if ip := u.Address.IP(); ip.To4() != nil {
				t := Tunnel{Iface: name, Local: ip.String()}
				if vendor {
					t.VPN = desc
				}
				s.Tunnels = append(s.Tunnels, t)
				break
			}
		}
	}
	return s, nil
}
