package app

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"hostbay/internal/netd"
)

// fakeHelper is hostbay-netd over an in-memory route table: adds and deletes
// land in routes, and closing the connection sweeps what it added.
type fakeHelper struct {
	mu     sync.Mutex
	routes []netd.SysRoute
	mtu    map[string]int
	fail   map[string]bool // Dst: refuse the change
	added  []netd.SysRoute
	calls  int // changes sent
	events chan struct{}
	done   chan struct{}
}

var fakeGW = netd.SysRoute{Dst: netip.MustParsePrefix("0.0.0.0/0"), Iface: "en0", Gateway: netip.MustParseAddr("192.168.1.1")}

func newFakeHelper(routes ...netd.SysRoute) *fakeHelper {
	return &fakeHelper{routes: append([]netd.SysRoute{fakeGW}, routes...), mtu: map[string]int{}, fail: map[string]bool{},
		events: make(chan struct{}, 1), done: make(chan struct{})}
}

func (f *fakeHelper) sys(r netd.Route) netd.SysRoute {
	s := netd.SysRoute{Dst: netip.MustParsePrefix(r.Dst), Iface: r.Iface}
	if r.Gateway {
		s.Iface, s.Gateway = fakeGW.Iface, fakeGW.Gateway
	}
	return s
}

func (f *fakeHelper) Add(_ context.Context, r netd.Route) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail[r.Dst] {
		return &netd.Error{Code: netd.CodeFailed, Reason: "route: writing to routing socket: File exists"}
	}
	s := f.sys(r)
	f.routes = append(f.routes, s)
	f.added = append(f.added, s)
	return nil
}

func (f *fakeHelper) Delete(_ context.Context, r netd.Route) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	s := f.sys(r)
	f.routes = slices.DeleteFunc(f.routes, func(x netd.SysRoute) bool { return x.Dst == s.Dst && x.Iface == s.Iface })
	return nil
}

func (f *fakeHelper) SetMTU(_ context.Context, iface string, mtu int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mtu[iface] = mtu
	return nil
}

func (f *fakeHelper) Events() <-chan struct{} { return f.events }
func (f *fakeHelper) Done() <-chan struct{}   { return f.done }

func (f *fakeHelper) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	select {
	case <-f.done:
	default:
		close(f.done)
		f.routes = slices.DeleteFunc(f.routes, func(x netd.SysRoute) bool { return slices.Contains(f.added, x) })
		f.added = nil
	}
	return nil
}

// Routes and Interfaces make it the read-only route table too.
func (f *fakeHelper) Routes() ([]netd.SysRoute, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.routes), nil
}

func (f *fakeHelper) Interfaces() ([]netd.Iface, error) {
	return []netd.Iface{{Name: "en0"}, {Name: "utun4", Tunnel: true}, {Name: "utun6", Tunnel: true}}, nil
}

func (f *fakeHelper) set(r ...netd.SysRoute) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes = append(f.routes, r...)
}

func (f *fakeHelper) changes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// reopen is the helper accepting a fresh connection after the last ended.
func (f *fakeHelper) reopen() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.done = make(chan struct{})
}

func TestHelperStatusStates(t *testing.T) {
	cases := []struct {
		st   netd.Status
		want string
	}{
		{netd.Status{Unavailable: "System-wide routing cannot run from the AppImage"}, HelperUnavailable},
		{netd.Status{}, HelperMissing},
		{netd.Status{Installed: true, Paired: false, Running: true, Current: true}, HelperRepair},
		{netd.Status{Installed: true, Paired: true}, HelperStopped}, // Current is false whenever it does not answer
		{netd.Status{Installed: true, Paired: true, Running: true}, HelperReinstall},
		{netd.Status{Installed: true, Paired: true, Running: true, Current: true}, HelperReady},
	}
	for _, c := range cases {
		s := NewNetdService()
		s.status = func(context.Context) netd.Status { return c.st }
		got := s.Status(context.Background())
		if got.State != c.want {
			t.Errorf("%+v: state %q, want %q", c.st, got.State, c.want)
		}
		if c.want == HelperUnavailable && got.Reason != c.st.Unavailable {
			t.Errorf("unavailable reason %q", got.Reason)
		}
	}
}

func TestInstallCancelledReportsIt(t *testing.T) {
	s := NewNetdService()
	s.status = func(context.Context) netd.Status { return netd.Status{} }
	s.install = func() error { return &netd.Error{Code: netd.CodeCancelled, Reason: "cancelled"} }
	st, err := s.Install()
	var e *netd.Error
	if !errors.As(err, &e) || e.Code != netd.CodeCancelled || st.State != HelperMissing {
		t.Fatalf("Install = %+v, %v", st, err)
	}
}

// The connection survives a busy helper, comes back after it drops, and
// reports each (re)connect and each route event; quitting closes it, so the
// helper sweeps Hostbay's routes.
func TestHelperConnectionKeepsItself(t *testing.T) {
	f := newFakeHelper()
	s := NewNetdService()
	busy := 2
	s.dial = func(context.Context) (netdClient, error) {
		if busy > 0 {
			busy--
			return nil, &netd.Error{Code: netd.CodeBusy, Reason: "busy"}
		}
		f.reopen()
		return f, nil
	}
	connects, events := make(chan struct{}, 4), make(chan struct{}, 4)
	s.onConnect = func() { connects <- struct{}{} }
	s.onEvent = func() { events <- struct{}{} }
	s.ServiceStartup(context.Background(), application.ServiceOptions{})
	wait := func(ch chan struct{}, what string) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatalf("no %s", what)
		}
	}
	wait(connects, "connect")
	if s.Status(context.Background()).State != HelperReady {
		t.Fatal("a held connection is not ready")
	}
	f.events <- struct{}{}
	wait(events, "route event")
	f.Close()
	wait(connects, "reconnect")
	s.ServiceShutdown()
	select {
	case <-f.Done():
	default:
		t.Fatal("quitting left the helper connection open")
	}
}

type staleClient struct{ netdClient }

func (staleClient) Stale() bool { return true }

// Connected to a helper copied by an older build, Hostbay still offers to
// reinstall it rather than reporting it ready.
func TestStatusOffersReinstallForAStaleConnectedHelper(t *testing.T) {
	s := NewNetdService()
	s.c = staleClient{}
	if st := s.Status(context.Background()); st.State != HelperReinstall {
		t.Fatalf("status %+v", st)
	}
	s.c = nil
	s.status = func(context.Context) netd.Status { return netd.Status{Installed: true, Paired: true, Running: true, Current: true} }
	if st := s.Status(context.Background()); st.State != HelperReady {
		t.Fatalf("status %+v", st)
	}
}
