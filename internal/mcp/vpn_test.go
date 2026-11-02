package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"hostbay/internal/mcp/mcptest"
	"hostbay/internal/store"
)

// fakeVPNs stands in for the app's VPN service: a connect comes up at once
// and remembers who made it.
type fakeVPNs struct {
	mu       sync.Mutex
	live     map[string]VPNLive
	drivable map[string]bool
	connects []string
	stops    []string
}

func (f *fakeVPNs) State(id string) VPNLive {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.live[id]
}

func (f *fakeVPNs) Connect(_ context.Context, id, principal string) (VPNLive, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connects = append(f.connects, id)
	l := VPNLive{Status: "connected", By: principal}
	f.live[id] = l
	return l, nil
}

func (f *fakeVPNs) Disconnect(id, principal string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.live[id].By != principal {
		return errors.New("not yours")
	}
	delete(f.live, id)
	f.stops = append(f.stops, id)
	return nil
}

// CanDrive makes the profiles named in drivable connectable from here.
func (f *fakeVPNs) CanDrive(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.drivable[id]
}

func (f *fakeVPNs) calls() (connects, stops []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.connects), slices.Clone(f.stops)
}

// addVPNs saves profiles: atlas-a and atlas-b share the tag atlas, solo
// alone has beta, and whole is system-wide.
func addVPNs(t *testing.T, r *rig) {
	t.Helper()
	err := r.st.Edit(func(d *store.Data) error {
		d.AddVPN(store.VPN{Meta: store.Meta{ID: "atlas-a"}, Name: "Atlas A", Type: "wireguard", Tier: store.VPNTierApp, Gateway: "a.example.test", Tags: []string{"atlas"}})
		d.AddVPN(store.VPN{Meta: store.Meta{ID: "atlas-b"}, Name: "Atlas B", Type: "openvpn", Tier: store.VPNTierApp, Gateway: "b.example.test", Tags: []string{"ATLAS"}})
		d.AddVPN(store.VPN{Meta: store.Meta{ID: "solo"}, Name: "Solo", Type: "wireguard", Tier: store.VPNTierApp, Gateway: "s.example.test", Tags: []string{"beta"}})
		d.AddVPN(store.VPN{Meta: store.Meta{ID: "whole"}, Name: "Whole", Type: "openvpn", Tier: store.VPNTierSystem, Gateway: "w.example.test", Tags: []string{"gamma"}})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// addExternalVPN saves ext, a profile that connects in its own app.
func addExternalVPN(t *testing.T, r *rig) {
	t.Helper()
	err := r.st.Edit(func(d *store.Data) error {
		d.AddVPN(store.VPN{Meta: store.Meta{ID: "ext"}, Name: "Ext", Type: "other", Tier: store.VPNTierExternal, Gateway: "x.example.test"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// allowVPN adds an allow entry for the profile as it is saved now, as
// Settings does; system-wide ones too, which Settings refuses.
func (r *rig) allowVPN(t *testing.T, tokenID, id string) {
	t.Helper()
	d, _ := r.st.Data()
	v := d.VPN(id)
	err := r.st.EditLocal(func(l *store.Local) error {
		l.MCP.VPNAllow = append(l.MCP.VPNAllow, store.VPNAllowEntry{Token: tokenID, Profile: id, Type: v.Type, Gateway: v.Gateway, Tier: v.Tier})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// background runs a tool call while the test answers its card.
func background(t *testing.T, cs *sdk.ClientSession, name string, args map[string]any) <-chan string {
	done := make(chan string, 1)
	go func() {
		res, text := call(t, cs, name, args)
		if res.IsError {
			text = "error: " + text
		}
		done <- text
	}()
	return done
}

func trayEmpty(t *testing.T, r *rig) {
	t.Helper()
	if l := r.srv.Jobs.Approvals.List(); len(l) != 0 {
		t.Fatalf("a card showed: %+v", l)
	}
}

// AE7: two profiles carry the tag, so a connect by it names both and
// connects nothing, with no card.
func TestConnectVPNByTagNamesEveryCandidate(t *testing.T) {
	r := newRig(t)
	addVPNs(t, r)
	cs := r.session(t, mcptest.V20260728)
	res, text := call(t, cs, ToolConnectVPN, map[string]any{"tag": "Atlas"})
	if !res.IsError || !strings.Contains(text, "Atlas A") || !strings.Contains(text, "Atlas B") || !strings.Contains(text, "VPNAmbiguous") {
		t.Fatalf("connect by tag = %v %s", res.IsError, text)
	}
	trayEmpty(t, r)
	if c, _ := r.vpns.calls(); len(c) != 0 {
		t.Fatalf("connected %v", c)
	}
}

// A connect waits for the card, which names the client, profile, tag,
// tier and gateway. Denied, it returns ApprovalDenied and connects nothing;
// approved, the profile connects and is the token's.
func TestConnectVPNWaitsForApproval(t *testing.T) {
	r := newRig(t)
	addVPNs(t, r)
	cs := r.session(t, mcptest.V20260728)

	done := background(t, cs, ToolConnectVPN, map[string]any{"tag": "beta"})
	p := r.approveNext(t, false)
	if p.Tool != ToolConnectVPN || p.Client != "claude-code" || p.Host != "Solo" || p.Target != "solo" || p.Tag != "beta" ||
		p.Tier != store.VPNTierApp || p.Gateway != "s.example.test" || p.Bytes != "" {
		t.Fatalf("card = %+v", p)
	}
	if text := <-done; !strings.HasPrefix(text, "error: ApprovalDenied") {
		t.Fatalf("denied = %s", text)
	}
	if c, _ := r.vpns.calls(); len(c) != 0 {
		t.Fatalf("connected %v after a denial", c)
	}

	done = background(t, cs, ToolConnectVPN, map[string]any{"profileId": "solo"})
	r.approveNext(t, true)
	var v VPNView
	if err := json.Unmarshal([]byte(<-done), &v); err != nil || v.State != "connected" || v.ConnectedBy != "you" {
		t.Fatalf("approved = %+v %v", v, err)
	}
	// Already up: nothing changes, so no card.
	if _, text := call(t, cs, ToolConnectVPN, map[string]any{"profileId": "solo"}); !strings.Contains(text, `"connected"`) {
		t.Fatalf("second connect = %s", text)
	}
	trayEmpty(t, r)
	if c, _ := r.vpns.calls(); !slices.Equal(c, []string{"solo"}) {
		t.Fatalf("connects %v", c)
	}
}

// A card can show only what it holds: a profile name with an invisible
// character, or such a tag, is refused before any card.
func TestConnectVPNRefusesInvisibleText(t *testing.T) {
	r := newRig(t)
	err := r.st.Edit(func(d *store.Data) error {
		d.AddVPN(store.VPN{Meta: store.Meta{ID: "rlo"}, Name: "Prod‮TSET", Type: "wireguard", Tier: store.VPNTierApp, Tags: []string{"x​y"}})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	cs := r.session(t, mcptest.V20260728)
	for _, args := range []map[string]any{{"profileId": "rlo"}, {"tag": "x​y"}} {
		if res, text := call(t, cs, ToolConnectVPN, args); !res.IsError || !strings.Contains(text, "U+") {
			t.Fatalf("%v = %s", args, text)
		}
	}
	trayEmpty(t, r)
	if c, _ := r.vpns.calls(); len(c) != 0 {
		t.Fatalf("connected %v", c)
	}
}

// An allow entry skips the card only for its own token; a system-wide
// profile asks even with one.
func TestVPNAllowEntryIsPerTokenAndNeverSystemWide(t *testing.T) {
	r := newRig(t)
	addVPNs(t, r)
	r.allowVPN(t, r.tokID, "solo")
	r.allowVPN(t, r.tokID, "whole")
	cs := r.session(t, mcptest.V20260728)

	if res, text := call(t, cs, ToolConnectVPN, map[string]any{"profileId": "solo"}); res.IsError || !strings.Contains(text, `"connected"`) {
		t.Fatalf("allowed connect = %s", text)
	}
	trayEmpty(t, r)
	if res, text := call(t, cs, ToolDisconnectVPN, map[string]any{"profileId": "solo"}); res.IsError {
		t.Fatalf("allowed disconnect = %s", text)
	}
	trayEmpty(t, r)

	done := background(t, cs, ToolConnectVPN, map[string]any{"profileId": "whole"})
	if p := r.approveNext(t, false); p.Target != "whole" || p.Tier != store.VPNTierSystem {
		t.Fatalf("system-wide card = %+v", p)
	}
	<-done

	_, other, _ := r.srv.Tokens.Create("other")
	b, err := mcptest.HTTP(t.Context(), r.url, other, mcptest.V20260728)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	done = background(t, b, ToolConnectVPN, map[string]any{"profileId": "solo"})
	if p := r.approveNext(t, false); p.Client != "other" {
		t.Fatalf("other token's card = %+v", p)
	}
	<-done
	if c, _ := r.vpns.calls(); !slices.Equal(c, []string{"solo"}) {
		t.Fatalf("connects %v", c)
	}
}

// An allow entry is bound to the gateway it was added for: a change synced
// from another computer makes the next connect ask.
func TestSyncedGatewayChangeInvalidatesVPNAllow(t *testing.T) {
	r := newRig(t)
	addVPNs(t, r)
	r.allowVPN(t, r.tokID, "solo")
	err := r.st.Edit(func(d *store.Data) error {
		v := d.VPN("solo") // as sync replaces the item with the remote one, Meta and all
		v.Gateway, v.Revision, v.DeviceID = "elsewhere.example.test", v.Revision+1, "other-computer"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	cs := r.session(t, mcptest.V20260728)
	done := background(t, cs, ToolConnectVPN, map[string]any{"profileId": "solo"})
	if p := r.approveNext(t, false); p.Gateway != "elsewhere.example.test" {
		t.Fatalf("card = %+v", p)
	}
	<-done
}

// disconnect_vpn ends only what this token connected: the user's profile
// or another token's is refused with no card.
func TestDisconnectVPNOnlyWhatTheTokenConnected(t *testing.T) {
	r := newRig(t)
	addVPNs(t, r)
	r.vpns.live["atlas-a"] = VPNLive{Status: "connected", By: "user"}
	r.vpns.live["atlas-b"] = VPNLive{Status: "connected", By: "mcp:someone-else"}
	cs := r.session(t, mcptest.V20260728)
	for _, id := range []string{"atlas-a", "atlas-b", "solo"} {
		if res, text := call(t, cs, ToolDisconnectVPN, map[string]any{"profileId": id}); !res.IsError {
			t.Fatalf("disconnect %s = %s", id, text)
		}
	}
	trayEmpty(t, r)

	r.vpns.live["solo"] = VPNLive{Status: "connected", By: principalScope + r.tokID}
	done := background(t, cs, ToolDisconnectVPN, map[string]any{"profileId": "solo"})
	if p := r.approveNext(t, true); p.Tool != ToolDisconnectVPN || p.Host != "Solo" {
		t.Fatalf("card = %+v", p)
	}
	if text := <-done; !strings.Contains(text, `"disconnected"`) {
		t.Fatalf("disconnect = %s", text)
	}
	if _, s := r.vpns.calls(); !slices.Equal(s, []string{"solo"}) {
		t.Fatalf("stops %v", s)
	}
}

// list_vpns filters by tag and text and says who holds a profile up.
func TestListVPNs(t *testing.T) {
	r := newRig(t)
	addVPNs(t, r)
	r.vpns.live["atlas-a"] = VPNLive{Status: "connected", By: "user"}
	cs := r.session(t, mcptest.V20260728)
	_, text := call(t, cs, "list_vpns", map[string]any{"tag": "atlas", "text": "s a"})
	var l vpnList
	json.Unmarshal([]byte(text), &l)
	if len(l.VPNs) != 1 || l.VPNs[0].ID != "atlas-a" || l.VPNs[0].State != "connected" || l.VPNs[0].ConnectedBy != "user" {
		t.Fatalf("list_vpns = %s", text)
	}
	_, text = call(t, cs, "vpn_status", map[string]any{"profileId": "whole"})
	if !strings.Contains(text, `"state":"disconnected"`) || strings.Contains(text, "connectedBy") {
		t.Fatalf("vpn_status = %s", text)
	}
}

// vpn_status finds a profile by its one tag, as connect_vpn does, and a tag
// several share is an error naming them.
func TestVPNStatusByTag(t *testing.T) {
	r := newRig(t)
	addVPNs(t, r)
	cs := r.session(t, mcptest.V20260728)
	_, text := call(t, cs, "vpn_status", map[string]any{"tag": "gamma"})
	if !strings.Contains(text, `"id":"whole"`) {
		t.Fatalf("vpn_status by tag = %s", text)
	}
	_, text = call(t, cs, "vpn_status", map[string]any{"tag": "atlas"})
	if !strings.Contains(text, "atlas-a") || !strings.Contains(text, "atlas-b") {
		t.Fatalf("ambiguous tag = %s", text)
	}
}

// list_connections gives each host's tags and the profile it is reached
// through as exactly {id, name, state}: never a SOCKS port.
func TestListConnectionsTagsAndVPN(t *testing.T) {
	r := newRig(t)
	addVPNs(t, r)
	err := r.st.Edit(func(d *store.Data) error {
		id := "atlas-a"
		held := store.HeldVPNID
		d.EditHost("h1", func(h *store.Host) { h.Tags, h.VPNID = []string{"atlas", "web"}, &id })
		d.EditHost("h2", func(h *store.Host) { h.VPNID = &held })
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	r.vpns.live["atlas-a"] = VPNLive{Status: "connected", By: "user"}
	cs := r.session(t, mcptest.V20260728)
	_, text := call(t, cs, "list_connections", nil)
	var raw struct {
		Connections []map[string]json.RawMessage `json:"connections"`
	}
	if err := json.Unmarshal([]byte(text), &raw); err != nil || len(raw.Connections) != 2 {
		t.Fatalf("list = %s", text)
	}
	if string(raw.Connections[0]["tags"]) != `["atlas","web"]` {
		t.Fatalf("tags = %s", raw.Connections[0]["tags"])
	}
	var vpn map[string]any
	json.Unmarshal(raw.Connections[0]["vpn"], &vpn)
	if len(vpn) != 3 || vpn["id"] != "atlas-a" || vpn["name"] != "Atlas A" || vpn["state"] != "connected" {
		t.Fatalf("vpn = %s", raw.Connections[0]["vpn"])
	}
	if _, ok := raw.Connections[1]["vpn"]; ok {
		t.Fatalf("a held profile showed: %s", text)
	}
	if strings.Contains(strings.ToLower(text), "socks") {
		t.Fatalf("list_connections = %s", text)
	}
}

// An external profile connects in its own app: down, connect_vpn fails
// telling the agent so, with no card and no connect; up, it returns the
// connected view with no card even though no run of ours holds it.
func TestConnectVPNExternal(t *testing.T) {
	r := newRig(t)
	addExternalVPN(t, r)
	cs := r.session(t, mcptest.V20260728)
	res, text := call(t, cs, ToolConnectVPN, map[string]any{"profileId": "ext"})
	if !res.IsError || !strings.Contains(text, "VPNFailed") || !strings.Contains(text, "own app") {
		t.Fatalf("external down = %v %s", res.IsError, text)
	}
	trayEmpty(t, r)

	r.vpns.live["ext"] = VPNLive{Status: "connected"}
	res, text = call(t, cs, ToolConnectVPN, map[string]any{"profileId": "ext"})
	if res.IsError || !strings.Contains(text, `"state":"connected"`) {
		t.Fatalf("external up = %v %s", res.IsError, text)
	}
	trayEmpty(t, r)
	if c, _ := r.vpns.calls(); len(c) != 0 {
		t.Fatalf("connected %v", c)
	}
}

// The card showed the profile as it was: a change to its gateway, type or
// tier while it waited refuses the connect.
func TestConnectVPNRefusesChangeWhileWaiting(t *testing.T) {
	for _, c := range []struct {
		field string
		edit  func(*store.VPN)
	}{
		{"gateway", func(v *store.VPN) { v.Gateway = "elsewhere.example.test" }},
		{"type", func(v *store.VPN) { v.Type = "openvpn" }},
		{"tier", func(v *store.VPN) { v.Tier = store.VPNTierSystem }},
	} {
		t.Run(c.field, func(t *testing.T) {
			r := newRig(t)
			addVPNs(t, r)
			cs := r.session(t, mcptest.V20260728)
			done := background(t, cs, ToolConnectVPN, map[string]any{"profileId": "solo"})
			for range 400 {
				if len(r.srv.Jobs.Approvals.List()) > 0 {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			if err := r.st.Edit(func(d *store.Data) error { c.edit(d.VPN("solo")); return nil }); err != nil {
				t.Fatal(err)
			}
			r.approveNext(t, true)
			if text := <-done; !strings.HasPrefix(text, "error:") || !strings.Contains(text, "changed while the request waited") {
				t.Fatalf("connect = %s", text)
			}
			if c, _ := r.vpns.calls(); len(c) != 0 {
				t.Fatalf("connected %v", c)
			}
		})
	}
}

// A host reached through a jump host's VPN lists that VPN, found by walking
// up the chain; its own VPN wins.
func TestListConnectionsShowsJumpHostVPN(t *testing.T) {
	r := newRig(t)
	addVPNs(t, r)
	err := r.st.Edit(func(d *store.Data) error {
		a, j := "atlas-a", "h2"
		d.EditHost("h2", func(h *store.Host) { h.VPNID = &a })
		d.EditHost("h1", func(h *store.Host) { h.JumpHostID = &j })
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	cs := r.session(t, mcptest.V20260728)
	_, text := call(t, cs, "list_connections", nil)
	var l connections
	if err := json.Unmarshal([]byte(text), &l); err != nil || len(l.Connections) < 2 {
		t.Fatalf("list = %s", text)
	}
	for _, c := range l.Connections[:2] {
		if c.VPN == nil || c.VPN.ID != "atlas-a" {
			t.Fatalf("%s vpn = %+v", c.ID, c.VPN)
		}
	}
}

// An external profile whose app Hostbay can drive connects like any other:
// after the user approves the card, and it is then the token's.
func TestConnectVPNExternalThatCanBeDriven(t *testing.T) {
	r := newRig(t)
	addExternalVPN(t, r)
	r.vpns.drivable = map[string]bool{"ext": true}
	cs := r.session(t, mcptest.V20260728)
	done := background(t, cs, ToolConnectVPN, map[string]any{"profileId": "ext"})
	if p := r.approveNext(t, true); p.Tier != store.VPNTierExternal || p.Target != "ext" {
		t.Fatalf("card = %+v", p)
	}
	var v VPNView
	if err := json.Unmarshal([]byte(<-done), &v); err != nil || v.State != "connected" || v.ConnectedBy != "you" {
		t.Fatalf("driven external = %+v %v", v, err)
	}
	if c, _ := r.vpns.calls(); !slices.Equal(c, []string{"ext"}) {
		t.Fatalf("connects %v", c)
	}
}
