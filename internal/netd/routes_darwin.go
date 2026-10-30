package netd

import (
	"context"
	"math/bits"
	"net"
	"net/netip"
	"os"
	"runtime"
	"strconv"

	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
)

// darwinTable reads routes from the kernel's routing sysctl and changes them
// with /sbin/route and /sbin/ifconfig, by absolute path and argv.
type darwinTable struct{}

func newTable() Table { return darwinTable{} }

func (darwinTable) Interfaces() ([]Iface, error) {
	nis, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]Iface, len(nis))
	for i, ni := range nis {
		out[i] = Iface{ni.Name, isTunnel(runtime.GOOS, ni.Name, ni.Flags&net.FlagPointToPoint != 0, 0, "")}
	}
	return out, nil
}

func (darwinTable) Routes() ([]SysRoute, error) {
	rib, err := route.FetchRIB(unix.AF_UNSPEC, route.RIBTypeRoute, 0)
	if err != nil {
		return nil, err
	}
	msgs, err := route.ParseRIB(route.RIBTypeRoute, rib)
	if err != nil {
		return nil, err
	}
	names := map[int]string{}
	if nis, err := net.Interfaces(); err == nil {
		for _, ni := range nis {
			names[ni.Index] = ni.Name
		}
	}
	var out []SysRoute
	for _, m := range msgs {
		rm, ok := m.(*route.RouteMessage)
		// Cloned host entries (ARP and neighbour cache) are not routes anyone set.
		if !ok || rm.Flags&unix.RTF_UP == 0 || rm.Flags&(unix.RTF_WASCLONED|unix.RTF_LLINFO) != 0 || len(rm.Addrs) <= unix.RTAX_NETMASK {
			continue
		}
		iface := names[rm.Index]
		dst, ok := sockAddr(rm.Addrs[unix.RTAX_DST], iface)
		if !ok || iface == "" {
			continue
		}
		ones := dst.BitLen()
		if rm.Flags&unix.RTF_HOST == 0 {
			ones = maskBits(rm.Addrs[unix.RTAX_NETMASK])
		}
		p, err := dst.WithZone("").Prefix(ones)
		if err != nil {
			continue
		}
		r := SysRoute{Dst: p, Iface: iface}
		if rm.Flags&unix.RTF_GATEWAY != 0 {
			r.Gateway, _ = sockAddr(rm.Addrs[unix.RTAX_GATEWAY], iface)
		}
		out = append(out, r)
	}
	return out, nil
}

func sockAddr(a route.Addr, iface string) (netip.Addr, bool) {
	switch a := a.(type) {
	case *route.Inet4Addr:
		return netip.AddrFrom4(a.IP), true
	case *route.Inet6Addr:
		ip := netip.AddrFrom16(a.IP)
		if ip.IsLinkLocalUnicast() {
			ip = ip.WithZone(iface)
		}
		return ip, true
	}
	return netip.Addr{}, false
}

// maskBits counts a netmask's ones; a missing mask is the default route's.
func maskBits(a route.Addr) int {
	var b []byte
	switch a := a.(type) {
	case *route.Inet4Addr:
		b = a.IP[:]
	case *route.Inet6Addr:
		b = a.IP[:]
	}
	n := 0
	for _, x := range b {
		n += bits.OnesCount8(x)
	}
	return n
}

// routeArgs spells a route for /sbin/route: by gateway when it has one,
// else on its interface.
func routeArgs(verb string, r SysRoute) []string {
	args := []string{"-n", verb}
	if r.Dst.Addr().Is6() {
		args = append(args, "-inet6")
	}
	if r.Dst.IsSingleIP() {
		args = append(args, "-host", r.Dst.Addr().String())
	} else {
		args = append(args, "-net", r.Dst.String())
	}
	if r.Gateway.IsValid() {
		return append(args, r.Gateway.String())
	}
	return append(args, "-interface", r.Iface)
}

func (darwinTable) Add(r SysRoute) error    { return runTool("/sbin/route", routeArgs("add", r)...) }
func (darwinTable) Delete(r SysRoute) error { return runTool("/sbin/route", routeArgs("delete", r)...) }

func (darwinTable) SetMTU(iface string, mtu int) error {
	return runTool("/sbin/ifconfig", iface, "mtu", strconv.Itoa(mtu))
}

// watchRoutes calls notify for each route added, deleted or changed, as the
// kernel announces them on a routing socket.
func watchRoutes(ctx context.Context, notify func()) error {
	fd, err := unix.Socket(unix.AF_ROUTE, unix.SOCK_RAW, unix.AF_UNSPEC)
	if err != nil {
		return err
	}
	// Non-blocking, so closing the file at ctx's end interrupts the read.
	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return err
	}
	f := os.NewFile(uintptr(fd), "route")
	context.AfterFunc(ctx, func() { f.Close() })
	buf := make([]byte, 2048)
	for {
		n, err := f.Read(buf)
		if err != nil {
			return err
		}
		// rt_msghdr: u_short rtm_msglen; u_char rtm_version; u_char rtm_type.
		if n >= 4 && (buf[3] == unix.RTM_ADD || buf[3] == unix.RTM_DELETE || buf[3] == unix.RTM_CHANGE) {
			notify()
		}
	}
}
