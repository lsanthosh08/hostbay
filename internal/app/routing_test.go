package app

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"hostbay/internal/netd"
	"hostbay/internal/store"
	"hostbay/internal/vpn/detect"
	"hostbay/internal/vpn/routing"
	"hostbay/internal/vpn/vpntest"
)

type routingEnv struct {
	vpn  *vpnEnv
	nd   *NetdService
	svc  *RoutingService
	f    *fakeHelper
	sase VPN
}

// newRoutingEnv is Harmony SASE connected on utun4 with auto-apply on,
// routing 10.1.0.0/16 per destination, and the helper not yet connected.
func newRoutingEnv(t *testing.T) *routingEnv {
	t.Helper()
	routingDebounce, routingCap = 20*time.Millisecond, time.Second
	netdRetryMin = 20 * time.Millisecond
	e := &routingEnv{vpn: newVPNEnv(t, vpntest.OK), f: newFakeHelper()}
	det := &fakeDetect{}
	det.set(detect.Snapshot{Tunnels: []detect.Tunnel{p2pTunnel("utun4", "10.255.0.54", "10.255.0.54")}})
	e.vpn.svc.detect = det.detect
	e.sase = saveExternal(t, e.vpn.svc, "Harmony SASE", func(v *VPN) {
		v.Type, v.Routes, v.AutoApply, v.MTU = "perimeter81", []string{"10.1.0.0/16"}, true, 1280
	})
	e.nd = NewNetdService()
	e.nd.dial = func(context.Context) (netdClient, error) {
		e.f.reopen()
		return e.f, nil
	}
	e.svc = NewRoutingService(e.vpn.svc.st, e.vpn.svc, e.nd)
	e.svc.table = e.f
	e.svc.mtuOf = func(string) int { return 1280 }
	e.svc.emit = func(string, any) {}
	return e
}

func (e *routingEnv) start(t *testing.T) {
	e.svc.ServiceStartup(context.Background(), application.ServiceOptions{})
	e.nd.ServiceStartup(context.Background(), application.ServiceOptions{})
	t.Cleanup(func() {
		e.nd.ServiceShutdown()
		e.svc.ServiceShutdown()
	})
}

func (e *routingEnv) has(dst, iface string) bool {
	rs, _ := e.f.Routes()
	return slices.Contains(rs, netd.SysRoute{Dst: netip.MustParsePrefix(dst), Iface: iface})
}

// settle waits until the helper has taken want changes in all and nothing
// more arrives for a while.
func (e *routingEnv) settle(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for e.f.changes() < want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)
	if got := e.f.changes(); got != want {
		t.Fatalf("helper took %d changes, want %d", got, want)
	}
}

// R26, AE5: one failed route is named in the report and the profile shows
// "partly applied".
func TestApplyReportsEachChangeAndPartlyApplied(t *testing.T) {
	e := newRoutingEnv(t)
	sase := e.sase
	sase.Routes = []string{"10.1.0.0/16", "10.2.0.0/16"}
	sase.AutoApply = false
	if _, err := e.vpn.svc.SaveVPN(sase); err != nil {
		t.Fatal(err)
	}
	e.f.fail["10.2.0.0/16"] = true
	e.start(t)
	e.settle(t, 0)

	plan, err := e.svc.Preview(context.Background(), "", "")
	if err != nil || len(plan.Changes) != 2 {
		t.Fatalf("preview %+v, %v", plan, err)
	}
	rep, err := e.svc.Apply(context.Background(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Results) != 2 || rep.Results[0].Error != "" || !strings.Contains(rep.Results[1].Error, "File exists") || rep.Results[1].Dst != "10.2.0.0/16" {
		t.Fatalf("results %+v", rep.Results)
	}
	if !slices.Equal(rep.Partly, []string{sase.ID}) || e.svc.State().Profiles[sase.ID] != RoutePartly {
		t.Fatalf("partly %v, state %+v", rep.Partly, e.svc.State())
	}
}

// KTD9: startup applies the saved mode once, and so does each reconnect to
// the helper, whose sweep removed Hostbay's routes.
func TestStartupAndReconnectReapplyOnce(t *testing.T) {
	e := newRoutingEnv(t)
	e.start(t)
	e.settle(t, 1)
	if !e.has("10.1.0.0/16", "utun4") {
		t.Fatal("startup did not apply the saved routes")
	}
	e.f.Close()
	e.settle(t, 2)
	if !e.has("10.1.0.0/16", "utun4") {
		t.Fatal("reconnect did not re-apply")
	}
}

// R29: a vendor reverting a planned route is re-applied once however many
// events it raises, and not again within the cap until the cap has passed.
func TestRouteEventsReapplyDebouncedAndCapped(t *testing.T) {
	e := newRoutingEnv(t)
	e.start(t)
	e.settle(t, 1)
	time.Sleep(routingCap)

	revert := func() {
		e.f.Delete(context.Background(), netd.Route{Dst: "10.1.0.0/16", Iface: "utun4"})
		for range 10 {
			e.nd.onEvent()
		}
	}
	revert() // takes one helper call itself
	e.settle(t, 3)
	if !e.has("10.1.0.0/16", "utun4") {
		t.Fatal("reverted route not re-applied")
	}

	revert()
	time.Sleep(routingCap / 4)
	if e.has("10.1.0.0/16", "utun4") {
		t.Fatal("re-applied again within the cap")
	}
	e.settle(t, 5)
	if !e.has("10.1.0.0/16", "utun4") {
		t.Fatal("held re-apply never ran after the cap")
	}
}

// A vendor that keeps reverting is reported as fighting.
func TestVendorThatKeepsRevertingIsFighting(t *testing.T) {
	e := newRoutingEnv(t)
	e.start(t)
	e.settle(t, 1)
	for i := range 3 {
		time.Sleep(routingCap)
		e.f.Delete(context.Background(), netd.Route{Dst: "10.1.0.0/16", Iface: "utun4"})
		e.nd.onEvent()
		e.settle(t, 3+2*i)
	}
	if st := e.svc.State().Profiles[e.sase.ID]; st != RouteFighting {
		t.Fatalf("state %q, want fighting", st)
	}
}

// R25 reset: every Hostbay route goes, and nothing re-applies until the
// user applies a mode again.
func TestResetRemovesHostbayRoutesAndStaysReset(t *testing.T) {
	e := newRoutingEnv(t)
	e.start(t)
	e.settle(t, 1)
	rep, err := e.svc.Reset(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Results) != 1 || rep.Results[0].Kind != routing.KindDelete || rep.Results[0].Error != "" {
		t.Fatalf("reset results %+v", rep.Results)
	}
	if e.has("10.1.0.0/16", "utun4") {
		t.Fatal("reset left Hostbay's route")
	}
	e.nd.onEvent()
	e.settle(t, 1)
	if !e.svc.State().Reset {
		t.Fatal("state does not show reset")
	}
	if _, err := e.svc.Apply(context.Background(), "", ""); err != nil || !e.has("10.1.0.0/16", "utun4") {
		t.Fatalf("apply after reset: %v", err)
	}
}

func TestNarrowRefusesAnEmptyKeepListAndSavesOne(t *testing.T) {
	e := newRoutingEnv(t)
	if err := e.svc.Narrow(e.sase.ID, "10.0.0.0/8", nil); err == nil {
		t.Fatal("an empty keep list was saved")
	}
	if err := e.svc.Narrow(e.sase.ID, "10.0.0.0/8", []string{"10.1.0.0/16"}); err != nil {
		t.Fatal(err)
	}
	e.f.set(netd.SysRoute{Dst: netip.MustParsePrefix("10.0.0.0/8"), Iface: "utun4"})
	plan, err := e.svc.Preview(context.Background(), "", "")
	if err != nil || len(plan.Changes) != 2 || plan.Changes[0].Kind != routing.KindNarrow || plan.Changes[0].Dst != "10.0.0.0/8" {
		t.Fatalf("plan %+v, %v", plan, err)
	}
}

// An unlabeled tunnel pinned to a profile routes through it.
func TestPinInterfaceLabelsTheTunnel(t *testing.T) {
	e := newRoutingEnv(t)
	other := saveExternal(t, e.vpn.svc, "SSL VPN", func(v *VPN) { v.Type, v.Routes = "perimeter81", []string{"10.212.0.0/16"} })
	plan, _ := e.svc.Preview(context.Background(), "", "")
	if len(plan.Unlabeled) != 1 || plan.Unlabeled[0] != "utun4" || len(plan.Changes) != 0 {
		t.Fatalf("two same-shape profiles on one tunnel: %+v", plan)
	}
	if err := e.svc.PinInterface(other.ID, "en0"); err == nil {
		t.Fatal("pinned a physical interface")
	}
	if err := e.svc.PinInterface(other.ID, "utun4"); err != nil {
		t.Fatal(err)
	}
	plan, _ = e.svc.Preview(context.Background(), "", "")
	if len(plan.Changes) != 1 || plan.Changes[0].Dst != "10.212.0.0/16" || plan.Changes[0].Iface != "utun4" {
		t.Fatalf("pinned plan %+v", plan)
	}
}

// R13: connecting a second full tunnel system-wide is refused naming the
// first; an app-scoped profile is not.
func TestConflictNamesTheFullTunnel(t *testing.T) {
	e := newRoutingEnv(t)
	sase := e.sase
	sase.RoutingMode = routing.Full
	if _, err := e.vpn.svc.SaveVPN(sase); err != nil {
		t.Fatal(err)
	}
	e.vpn.svc.refresh(context.Background())
	corp := saveExternal(t, e.vpn.svc, "Corp", func(v *VPN) { v.Routes = []string{"0.0.0.0/0"} })
	if err := e.svc.conflict(corp.ID); err == nil || !strings.Contains(err.Error(), "Harmony SASE") {
		t.Fatalf("conflict %v", err)
	}
}

// KTD9: an app-scoped profile connecting after a full tunnel was applied by
// hand gets its gateway pinned, auto-apply or not, at every address its
// name resolves to.
func TestAppScopedGatewayIsPinnedWithoutAutoApply(t *testing.T) {
	e := newRoutingEnv(t)
	sase := e.sase
	sase.AutoApply, sase.RoutingMode = false, routing.Full
	if _, err := e.vpn.svc.SaveVPN(sase); err != nil {
		t.Fatal(err)
	}
	e.svc.resolve = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("2001:db8::7"), netip.MustParseAddr("203.0.113.7")}, nil
	}
	e.start(t)
	e.settle(t, 0)
	if _, err := e.svc.Apply(context.Background(), "", ""); err != nil || !e.has("0.0.0.0/1", "utun4") {
		t.Fatalf("full tunnel: %v", err)
	}
	wg := e.vpn.save(t, false)
	if _, err := e.vpn.svc.ConnectVPN(context.Background(), wg.ID); err != nil {
		t.Fatal(err)
	}
	e.settle(t, 3)
	rs, _ := e.f.Routes()
	if !slices.ContainsFunc(rs, func(r netd.SysRoute) bool { return r.Dst.String() == "203.0.113.7/32" && r.Iface == "en0" }) {
		t.Fatalf("gateway not pinned: %v", rs)
	}
}

// A system-wide WireGuard connecting behind a full tunnel gets its peer
// pinned to the local network too, or its own packets would enter the
// other tunnel.
func TestSystemWideWireGuardEndpointIsPinnedBehindAFullTunnel(t *testing.T) {
	e := newRoutingEnv(t)
	sase := e.sase
	sase.AutoApply, sase.RoutingMode = false, routing.Full
	if _, err := e.vpn.svc.SaveVPN(sase); err != nil {
		t.Fatal(err)
	}
	e.svc.resolve = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("203.0.113.7")}, nil
	}
	ft := &fakeTunnels{}
	e.nd.dial = func(context.Context) (netdClient, error) {
		e.f.reopen()
		return tunnelHelper{e.f, ft}, nil
	}
	ft.reply = func(m netd.Msg) (netd.Msg, error) {
		go ft.event(netd.Msg{Tunnel: m.Tunnel, State: netd.TunnelUp, Iface: "utun6", Address: []string{"10.30.0.2/32"}})
		return netd.Msg{Iface: "utun6"}, nil
	}
	e.start(t)
	e.settle(t, 0)
	if _, err := e.svc.Apply(context.Background(), "", ""); err != nil || !e.has("0.0.0.0/1", "utun4") {
		t.Fatalf("full tunnel: %v", err)
	}
	v := e.vpn.save(t, false)
	key := testWGKey
	v.Tier, v.PrivateKey, v.Routes, v.AutoApply = store.VPNTierSystem, &key, []string{"10.30.0.0/16"}, true
	if _, err := e.vpn.svc.SaveVPN(v); err != nil {
		t.Fatal(err)
	}
	if _, err := e.vpn.svc.ConnectVPN(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	pinned := func() bool {
		rs, _ := e.f.Routes()
		return slices.ContainsFunc(rs, func(r netd.SysRoute) bool { return r.Dst.String() == "203.0.113.7/32" && r.Iface == "en0" })
	}
	for i := 0; i < 100 && !pinned(); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if !pinned() {
		rs, _ := e.f.Routes()
		t.Fatalf("peer not pinned: %v", rs)
	}
}

func TestRouteGoneRecognisesTheToolsMissingRouteErrors(t *testing.T) {
	for _, m := range []string{"route: writing to routing socket: not in table", "RTNETLINK answers: No such process"} {
		if !routeGone(errors.New(m)) {
			t.Errorf("%q not taken for a gone route", m)
		}
	}
	if routeGone(errors.New("operation not permitted")) {
		t.Error("a permission error taken for a gone route")
	}
}
