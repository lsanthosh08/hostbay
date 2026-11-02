package app

import (
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"hostbay/internal/store"
	"hostbay/internal/vpn/vpntest"
)

// The tray lists each profile with its state; one holding a run
// disconnects when chosen, any other connects (R10).
func TestTrayMenuListsProfilesWithState(t *testing.T) {
	profiles := []VPN{
		{ID: "a", Name: "home"},
		{ID: "b", Name: "office"},
		{ID: "c", Name: "lab"},
		{ID: "d", Name: "dc", AutoReconnect: true},
		{ID: "e", Name: "old"},
		{ID: "f", Name: "sso"},
	}
	states := []VPNState{
		{ID: "b", Status: VPNConnected},
		{ID: "c", Status: VPNSigningIn, Need: "otp"},
		{ID: "d", Status: VPNDropped},
		{ID: "e", Status: VPNDropped},
		{ID: "f", Status: VPNFailed, Reason: "bad"},
	}
	want := []TrayItem{
		{Label: "home — Disconnected", Action: TrayConnect, VPN: "a"},
		{Label: "office — Connected", Checked: true, Action: TrayDisconnect, VPN: "b"},
		{Label: "lab — Waiting for a one-time code", Checked: true, Action: TrayDisconnect, VPN: "c"},
		{Label: "dc — Dropped, reconnecting", Checked: true, Action: TrayDisconnect, VPN: "d"},
		{Label: "old — Dropped", Action: TrayConnect, VPN: "e"},
		{Label: "sso — Failed", Action: TrayConnect, VPN: "f"},
		{},
		{Label: "Show " + Name, Action: TrayShow},
		{Label: "Quit " + Name, Action: TrayQuit},
	}
	if got := trayMenu(profiles, states); !reflect.DeepEqual(got, want) {
		t.Fatalf("menu\n got %+v\nwant %+v", got, want)
	}
	if got := trayMenu(nil, nil); len(got) != 2 || got[0].Action != TrayShow || got[1].Action != TrayQuit {
		t.Fatalf("no profiles: %+v", got)
	}
}

// trayEnv is a tray over a real VPNService and the fake helper, wired the
// way main.go wires it: every state event reaches StateChanged off the
// service's lock, as Wails delivers events to Go listeners.
type trayEnv struct {
	*vpnEnv
	tray *Tray

	mu     sync.Mutex
	shown  int
	panels int // ShowVPNsEvent emits
	menu   []TrayItem
}

func newTrayEnv(t *testing.T, mode string) *trayEnv {
	e := &trayEnv{vpnEnv: newVPNEnv(t, mode)}
	e.tray = NewTray(e.svc, func() { e.mu.Lock(); e.shown++; e.mu.Unlock() }, func() {})
	e.tray.render = func(items []TrayItem) { e.mu.Lock(); e.menu = items; e.mu.Unlock() }
	e.tray.emit = func(name string, _ any) {
		if name != ShowVPNsEvent {
			t.Errorf("tray event %q", name)
		}
		e.mu.Lock()
		e.panels++
		e.mu.Unlock()
	}
	record := e.svc.emit
	e.svc.emit = func(name string, data any) {
		record(name, data)
		go e.tray.StateChanged(data.(VPNState))
	}
	return e
}

// item is the profile's current row, after a refresh.
func (e *trayEnv) item(t *testing.T, id string) TrayItem {
	t.Helper()
	e.tray.Refresh()
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, it := range e.menu {
		if it.VPN == id {
			return it
		}
	}
	t.Fatalf("no row for %s in %+v", id, e.menu)
	return TrayItem{}
}

// waitShown waits until the window came forward with the VPN panel open.
func (e *trayEnv) waitShown(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		n, p := e.shown, e.panels
		e.mu.Unlock()
		if n > 0 && p > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the window was never shown")
}

func TestTrayTogglesConnect(t *testing.T) {
	e := newTrayEnv(t, vpntest.OK)
	v := e.save(t, false)
	it := e.item(t, v.ID)
	if it.Action != TrayConnect || !strings.HasPrefix(it.Label, "office — ") {
		t.Fatalf("before: %+v", it)
	}
	e.tray.Do(it)
	e.waitFor(t, v.ID, VPNConnected)
	if it = e.item(t, v.ID); it.Action != TrayDisconnect || it.Label != "office — Connected" || !it.Checked {
		t.Fatalf("connected: %+v", it)
	}
	e.tray.Do(it)
	e.waitFor(t, v.ID, VPNDisconnected)
	if it = e.item(t, v.ID); it.Action != TrayConnect {
		t.Fatalf("disconnected: %+v", it)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.shown != 0 || e.panels != 0 {
		t.Fatalf("a plain connect showed the window %d times, the VPN panel %d", e.shown, e.panels)
	}
}

// A tray connect that waits for a one-time code brings the window forward
// with the VPN panel open, where it asks for the code, instead of waiting
// unseen (R20).
func TestTrayConnectNeedingACodeShowsTheWindow(t *testing.T) {
	e := newTrayEnv(t, vpntest.OTP)
	v := e.save(t, false)
	e.tray.Do(e.item(t, v.ID))
	e.waitFor(t, v.ID, VPNSigningIn)
	e.waitShown(t)
	if err := e.svc.AnswerVPN(v.ID, vpntest.Code); err != nil {
		t.Fatal(err)
	}
	e.waitFor(t, v.ID, VPNConnected)
}

// A tray connect that fails, a gateway certificate to trust among the
// reasons, shows the window, where the profile's state says why.
func TestTrayConnectThatFailsShowsTheWindow(t *testing.T) {
	e := newTrayEnv(t, vpntest.OK)
	v := e.save(t, false)
	e.svc.helper = func() (string, error) { return filepath.Join(t.TempDir(), "hostbay-vpn"), nil }
	e.tray.Do(e.item(t, v.ID))
	e.waitShown(t)
}

// With the setting off, closing the window quits, and quitting asks first
// while tunnels or VPNs are up (R19); with it on, the window hides and
// nothing stops. Once quitting is decided, a close goes through.
func TestWindowClose(t *testing.T) {
	for _, c := range []struct {
		keep, quitting bool
		want           string
	}{
		{false, false, CloseQuit},
		{true, false, CloseHide},
		{false, true, CloseLet},
		{true, true, CloseLet},
	} {
		if got := WindowClose(c.keep, c.quitting); got != c.want {
			t.Errorf("WindowClose(keep %v, quitting %v) = %q, want %q", c.keep, c.quitting, got, c.want)
		}
	}
}

func TestQuitQuestion(t *testing.T) {
	for _, c := range []struct {
		tunnels, vpns int
		want          string
	}{
		{0, 0, ""},
		{2, 0, "2 tunnel(s) are running. Quitting stops them."},
		{0, 1, "1 VPN(s) are connected. Quitting disconnects them."},
		{1, 3, "1 tunnel(s) are running and 3 VPN(s) are connected. Quitting stops them."},
	} {
		if got := QuitQuestion(c.tunnels, c.vpns); got != c.want {
			t.Errorf("QuitQuestion(%d, %d) = %q, want %q", c.tunnels, c.vpns, got, c.want)
		}
	}
}

// An external profile is connected by its own app, so the tray must not
// start a run of Hostbay's for it: the item says so, and Do drives the client.
func TestTrayMarksExternalProfiles(t *testing.T) {
	items := trayMenu([]VPN{{ID: "a", Name: "Harmony", Tier: store.VPNTierExternal}, {ID: "b", Name: "Lab", Tier: store.VPNTierApp}}, nil)
	if !items[0].External || items[1].External {
		t.Fatalf("items %+v", items)
	}
}
