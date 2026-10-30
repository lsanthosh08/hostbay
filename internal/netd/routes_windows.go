package netd

import (
	"context"
	"errors"
	"net/netip"
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// iphlpapi calls x/sys does not wrap.
var (
	iphlpapi                       = windows.NewLazySystemDLL("iphlpapi.dll")
	procInitializeIpForwardEntry   = iphlpapi.NewProc("InitializeIpForwardEntry")
	procCreateIpForwardEntry2      = iphlpapi.NewProc("CreateIpForwardEntry2")
	procDeleteIpForwardEntry2      = iphlpapi.NewProc("DeleteIpForwardEntry2")
	procInitializeIpInterfaceEntry = iphlpapi.NewProc("InitializeIpInterfaceEntry")
	procSetIpInterfaceEntry        = iphlpapi.NewProc("SetIpInterfaceEntry")
)

func callIphlp(p *windows.LazyProc, arg unsafe.Pointer) error {
	if r, _, _ := p.Call(uintptr(arg)); r != 0 {
		return windows.Errno(r)
	}
	return nil
}

// windowsTable reads and changes routes through the IP Helper API; no
// program is started.
type windowsTable struct{}

func newTable() Table { return windowsTable{} }

type adapter struct {
	name, desc string
	ifType     uint32
	luid       uint64
}

func adapters() ([]adapter, error) {
	size := uint32(15000)
	for {
		buf := make([]byte, size)
		aa := (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0]))
		err := windows.GetAdaptersAddresses(windows.AF_UNSPEC, windows.GAA_FLAG_INCLUDE_ALL_INTERFACES|windows.GAA_FLAG_SKIP_UNICAST|windows.GAA_FLAG_SKIP_ANYCAST|windows.GAA_FLAG_SKIP_MULTICAST|windows.GAA_FLAG_SKIP_DNS_SERVER, 0, aa, &size)
		if errors.Is(err, windows.ERROR_BUFFER_OVERFLOW) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var out []adapter
		for ; aa != nil; aa = aa.Next {
			out = append(out, adapter{windows.UTF16PtrToString(aa.FriendlyName), windows.UTF16PtrToString(aa.Description), aa.IfType, aa.Luid})
		}
		return out, nil
	}
}

func (windowsTable) Interfaces() ([]Iface, error) {
	as, err := adapters()
	if err != nil {
		return nil, err
	}
	out := make([]Iface, len(as))
	for i, a := range as {
		out[i] = Iface{a.name, isTunnel(runtime.GOOS, a.name, false, a.ifType, a.desc)}
	}
	return out, nil
}

func (windowsTable) Routes() ([]SysRoute, error) {
	as, err := adapters()
	if err != nil {
		return nil, err
	}
	names := map[uint64]string{}
	for _, a := range as {
		names[a.luid] = a.name
	}
	var table *windows.MibIpForwardTable2
	if err := windows.GetIpForwardTable2(windows.AF_UNSPEC, &table); err != nil {
		return nil, err
	}
	defer windows.FreeMibTable(unsafe.Pointer(table))
	var out []SysRoute
	for _, row := range table.Rows() {
		dst, ok := rawAddr(&row.DestinationPrefix.Prefix)
		iface := names[row.InterfaceLuid]
		if !ok || iface == "" {
			continue
		}
		p, err := dst.Prefix(int(row.DestinationPrefix.PrefixLength))
		if err != nil {
			continue
		}
		r := SysRoute{Dst: p, Iface: iface}
		if gw, ok := rawAddr(&row.NextHop); ok && !gw.IsUnspecified() {
			r.Gateway = gw
		}
		out = append(out, r)
	}
	return out, nil
}

func rawAddr(sa *windows.RawSockaddrInet) (netip.Addr, bool) {
	switch sa.Family {
	case windows.AF_INET:
		return netip.AddrFrom4((*windows.RawSockaddrInet4)(unsafe.Pointer(sa)).Addr), true
	case windows.AF_INET6:
		return netip.AddrFrom16((*windows.RawSockaddrInet6)(unsafe.Pointer(sa)).Addr), true
	}
	return netip.Addr{}, false
}

func setRawAddr(sa *windows.RawSockaddrInet, a netip.Addr) {
	*sa = windows.RawSockaddrInet{}
	if a.Is4() {
		v4 := (*windows.RawSockaddrInet4)(unsafe.Pointer(sa))
		v4.Family, v4.Addr = windows.AF_INET, a.As4()
		return
	}
	v6 := (*windows.RawSockaddrInet6)(unsafe.Pointer(sa))
	v6.Family, v6.Addr = windows.AF_INET6, a.As16()
}

func luidOf(name string) (uint64, error) {
	as, err := adapters()
	if err != nil {
		return 0, err
	}
	for _, a := range as {
		if a.name == name {
			return a.luid, nil
		}
	}
	return 0, errors.New("no interface " + name)
}

func forwardRow(r SysRoute) (*windows.MibIpForwardRow2, error) {
	luid, err := luidOf(r.Iface)
	if err != nil {
		return nil, err
	}
	row := new(windows.MibIpForwardRow2)
	procInitializeIpForwardEntry.Call(uintptr(unsafe.Pointer(row)))
	row.InterfaceLuid = luid
	setRawAddr(&row.DestinationPrefix.Prefix, r.Dst.Addr())
	row.DestinationPrefix.PrefixLength = uint8(r.Dst.Bits())
	nh := r.Gateway
	if !nh.IsValid() { // on-link: the unspecified address of the family
		nh = netip.IPv6Unspecified()
		if r.Dst.Addr().Is4() {
			nh = netip.IPv4Unspecified()
		}
	}
	setRawAddr(&row.NextHop, nh.WithZone(""))
	row.Protocol = windows.MIB_IPPROTO_NETMGMT
	return row, nil
}

func (windowsTable) Add(r SysRoute) error {
	row, err := forwardRow(r)
	if err != nil {
		return err
	}
	return callIphlp(procCreateIpForwardEntry2, unsafe.Pointer(row))
}

func (windowsTable) Delete(r SysRoute) error {
	row, err := forwardRow(r)
	if err != nil {
		return err
	}
	return callIphlp(procDeleteIpForwardEntry2, unsafe.Pointer(row))
}

// SetMTU sets the IPv4 MTU, and the IPv6 one where the interface has IPv6
// and the MTU meets IPv6's 1280 minimum.
func (windowsTable) SetMTU(iface string, mtu int) error {
	luid, err := luidOf(iface)
	if err != nil {
		return err
	}
	set := func(family uint16) error {
		var row windows.MibIpInterfaceRow
		procInitializeIpInterfaceEntry.Call(uintptr(unsafe.Pointer(&row)))
		row.Family, row.InterfaceLuid = family, luid
		if err := windows.GetIpInterfaceEntry(&row); err != nil {
			return err
		}
		row.NlMtu = uint32(mtu)
		row.SitePrefixLength = 0 // SetIpInterfaceEntry refuses IPv4 rows that carry one
		return callIphlp(procSetIpInterfaceEntry, unsafe.Pointer(&row))
	}
	if err := set(windows.AF_INET); err != nil {
		return err
	}
	if mtu >= 1280 {
		set(windows.AF_INET6)
	}
	return nil
}

var (
	routeNotifyOnce sync.Once
	routeNotifyCb   uintptr
	routeNotifyMu   sync.Mutex
	routeNotify     func()
)

// watchRoutes calls notify for each route change Windows reports.
func watchRoutes(ctx context.Context, notify func()) error {
	routeNotifyMu.Lock()
	routeNotify = notify
	routeNotifyMu.Unlock()
	// Callbacks are never freed, so make the one this process needs once.
	routeNotifyOnce.Do(func() {
		routeNotifyCb = windows.NewCallback(func(_ uintptr, _ *windows.MibIpForwardRow2, _ uint32) uintptr {
			routeNotifyMu.Lock()
			f := routeNotify
			routeNotifyMu.Unlock()
			if f != nil {
				f()
			}
			return 0
		})
	})
	var h windows.Handle
	if err := windows.NotifyRouteChange2(windows.AF_UNSPEC, routeNotifyCb, nil, false, &h); err != nil {
		return err
	}
	<-ctx.Done()
	return windows.CancelMibChangeNotify2(h)
}
