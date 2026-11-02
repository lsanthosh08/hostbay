package app

import (
	"context"
	"fmt"
	"log"

	"github.com/wailsapp/wails/v3/pkg/application"

	"hostbay/internal/store"
	"hostbay/internal/vpn/proto"
)

// What a tray menu item does.
const (
	TrayConnect    = "connect"
	TrayDisconnect = "disconnect"
	TrayShow       = "show"
	TrayQuit       = "quit"
)

// TrayItem is one row of the tray menu; one with no label is a separator.
type TrayItem struct {
	Label   string
	Checked bool // the profile holds a run
	Action  string
	VPN     string // the profile, for connect and disconnect
	// External profiles are connected by their own app (or the command line
	// Hostbay drives it with), never by a run of Hostbay's.
	External bool
}

var trayStatus = map[string]string{
	VPNDisconnected: "Disconnected",
	VPNConnecting:   "Connecting…",
	VPNConnected:    "Connected",
	VPNFailed:       "Failed",
	VPNDropped:      "Dropped",
}

// trayMenu lists each profile with its state, then Show and Quit (R10). A
// profile holding a run disconnects when chosen: connecting, signing in,
// connected, or dropped and waiting to reconnect, which only an
// auto-reconnect profile does. Any other connects.
func trayMenu(profiles []VPN, states []VPNState) []TrayItem {
	by := map[string]VPNState{}
	for _, s := range states {
		by[s.ID] = s
	}
	var items []TrayItem
	for _, p := range profiles {
		s := by[p.ID]
		text, ok := trayStatus[s.Status]
		if !ok {
			text = trayStatus[VPNDisconnected]
		}
		held := s.Status == VPNConnecting || s.Status == VPNSigningIn || s.Status == VPNConnected
		switch {
		case s.Status == VPNSigningIn && s.Need == proto.NeedOTP:
			text = "Waiting for a one-time code"
		case s.Status == VPNSigningIn:
			text = "Waiting for sign-in in the browser"
		case s.Status == VPNDropped && p.AutoReconnect:
			text, held = "Dropped, reconnecting", true
		}
		it := TrayItem{Label: p.Name + " — " + text, Checked: held, Action: TrayConnect, VPN: p.ID, External: p.Tier == store.VPNTierExternal}
		if held {
			it.Action = TrayDisconnect
		}
		items = append(items, it)
	}
	if len(items) > 0 {
		items = append(items, TrayItem{})
	}
	return append(items, TrayItem{Label: "Show " + Name, Action: TrayShow}, TrayItem{Label: "Quit " + Name, Action: TrayQuit})
}

// ShowVPNsEvent asks the page to open the VPN panel, where a profile's
// sign-in prompt and failure reason show.
const ShowVPNsEvent = "show-vpns"

// Tray is the system tray menu of VPN profiles (R10).
type Tray struct {
	vpns   *VPNService
	show   func() // brings the main window forward
	quit   func()
	render func([]TrayItem)
	emit   func(name string, data any)
}

func NewTray(vpns *VPNService, show, quit func()) *Tray {
	return &Tray{vpns: vpns, show: show, quit: quit, render: func([]TrayItem) {}, emit: emitEvent}
}

// showVPNs brings the window forward with the VPN panel open.
func (t *Tray) showVPNs() {
	t.emit(ShowVPNsEvent, nil)
	t.show()
}

// Attach draws the menu in st and redraws it each time it opens, since
// saving, deleting or syncing a profile sends no event.
func (t *Tray) Attach(st *application.SystemTray) {
	t.render = func(items []TrayItem) {
		m := application.NewMenu()
		for _, it := range items {
			var mi *application.MenuItem
			switch {
			case it.Label == "":
				m.AddSeparator()
				continue
			case it.VPN != "":
				mi = m.AddCheckbox(it.Label, it.Checked)
			default:
				mi = m.Add(it.Label)
			}
			mi.OnClick(func(*application.Context) { t.Do(it) })
		}
		// shortcut: the replaced menu is not destroyed, since it may be open
		// on screen; each redraw leaks a few items. Reuse one menu if that grows.
		st.SetMenu(m)
	}
	// shortcut: Linux cannot open the menu from Go, so there it is redrawn
	// only on state changes; a profile added there shows after the next one.
	open := func() { t.Refresh(); st.OpenMenu() }
	st.OnClick(open).OnRightClick(open)
	t.Refresh()
}

// Refresh redraws the menu from the saved profiles and their states. Two
// refreshes racing may leave the older one drawn until the next; opening
// the menu always redraws it.
func (t *Tray) Refresh() {
	var profiles []VPN
	d, err := t.vpns.st.Data()
	if err != nil {
		log.Printf("tray: %v", err)
	} else {
		for _, v := range d.VPNs.Items {
			profiles = append(profiles, VPN{ID: v.ID, Name: v.Name, AutoReconnect: v.AutoReconnect, Tier: v.Tier})
		}
	}
	t.render(trayMenu(profiles, t.vpns.VPNStates()))
}

// StateChanged takes each vpn-state event. A profile waiting for a
// one-time code or a browser sign-in brings the window forward with the
// VPN panel open, where it asks for it. It reads the service, so it must not run under
// the service's lock; Wails calls Go listeners on their own goroutines.
func (t *Tray) StateChanged(st VPNState) {
	// Only a one-time code is typed in the window; a browser sign-in happens in
	// the browser that just opened, which the window would take focus from.
	if st.Status == VPNSigningIn && st.Need == proto.NeedOTP {
		t.showVPNs()
	}
	t.Refresh()
}

// Do runs a menu item. Connect and disconnect run off the menu's thread:
// a connect waits through sign-in.
func (t *Tray) Do(it TrayItem) {
	switch it.Action {
	case TrayConnect, TrayDisconnect:
		if it.External {
			go func() {
				do := t.vpns.ConnectVPNClient
				if it.Action == TrayDisconnect {
					do = t.vpns.DisconnectVPNClient
				}
				// A client Hostbay cannot drive is opened or left to its app; either way the panel says why.
				if err := do(context.Background(), it.VPN); err != nil {
					t.showVPNs()
				}
			}()
			return
		}
	}
	switch it.Action {
	case TrayConnect:
		go func() {
			// A failure, a gateway certificate to trust among them, shows the
			// window: the panel says why, and its own connect asks to trust.
			if st, err := t.vpns.ConnectVPN(context.Background(), it.VPN); err != nil && st.Status == VPNFailed {
				t.showVPNs()
			}
		}()
	case TrayDisconnect:
		go t.vpns.DisconnectVPN(it.VPN)
	case TrayShow:
		t.show()
	case TrayQuit:
		t.quit()
	}
}

// What closing the main window does.
const (
	CloseHide = "hide" // keep running in the menu bar
	CloseQuit = "quit" // quit instead, which asks first while anything runs (R19)
	CloseLet  = "let"  // quitting is decided; the close goes through
)

// WindowClose decides what closing the main window does. Closing never
// goes straight through before quitting is decided, since a cancelled quit
// would leave Hostbay running with its window gone.
func WindowClose(keepRunning, quitting bool) string {
	switch {
	case quitting:
		return CloseLet
	case keepRunning:
		return CloseHide
	}
	return CloseQuit
}

// QuitQuestion is what quitting asks while tunnels run or VPNs Hostbay
// connected are up (R19); "" quits without asking.
func QuitQuestion(tunnels, vpns int) string {
	switch {
	case tunnels == 0 && vpns == 0:
		return ""
	case tunnels == 0:
		return fmt.Sprintf("%d VPN(s) are connected. Quitting disconnects them.", vpns)
	case vpns > 0:
		return fmt.Sprintf("%d tunnel(s) are running and %d VPN(s) are connected. Quitting stops them.", tunnels, vpns)
	}
	return fmt.Sprintf("%d tunnel(s) are running. Quitting stops them.", tunnels)
}
