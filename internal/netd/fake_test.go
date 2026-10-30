package netd

import (
	"fmt"
	"net/netip"
	"slices"
	"sync"
)

// fakeTable is an in-memory route table with the system's refusals: adding a
// route that exists or deleting one that does not fails.
type fakeTable struct {
	mu     sync.Mutex
	routes []SysRoute
	ifaces []Iface
	mtu    map[string]int
	onAdd  func(SysRoute) // runs before an add lands
}

func newFakeTable(routes ...SysRoute) *fakeTable {
	return &fakeTable{
		routes: routes,
		ifaces: []Iface{{Name: "en0"}, {Name: "en1"}, {Name: "utun3", Tunnel: true}, {Name: "utun5", Tunnel: true}},
		mtu:    map[string]int{},
	}
}

func (f *fakeTable) Routes() ([]SysRoute, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.routes), nil
}

func (f *fakeTable) Interfaces() ([]Iface, error) { return f.ifaces, nil }

func (f *fakeTable) Add(r SysRoute) error {
	if f.onAdd != nil {
		f.onAdd(r)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if slices.Contains(f.routes, r) {
		return fmt.Errorf("route %v exists", r)
	}
	f.routes = append(f.routes, r)
	return nil
}

func (f *fakeTable) Delete(r SysRoute) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := slices.Index(f.routes, r)
	if i < 0 {
		return fmt.Errorf("route %v not found", r)
	}
	f.routes = slices.Delete(f.routes, i, i+1)
	return nil
}

func (f *fakeTable) SetMTU(iface string, mtu int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mtu[iface] = mtu
	return nil
}

func (f *fakeTable) has(r SysRoute) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Contains(f.routes, r)
}

func (f *fakeTable) snapshot() []SysRoute {
	r, _ := f.Routes()
	return r
}

func sr(dst, iface string) SysRoute {
	return SysRoute{Dst: netip.MustParsePrefix(dst), Iface: iface}
}

var (
	lanGateway   = netip.MustParseAddr("192.168.1.1")
	defaultRoute = SysRoute{Dst: netip.MustParsePrefix("0.0.0.0/0"), Iface: "en0", Gateway: lanGateway}
	lanRoute     = sr("192.168.1.0/24", "en0")
)
