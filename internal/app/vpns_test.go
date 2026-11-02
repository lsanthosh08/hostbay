package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"hostbay/internal/apperr"
	"hostbay/internal/secrets"
	"hostbay/internal/store"
	"hostbay/internal/vpn/detect"
	"hostbay/internal/vpn/proto"
	"hostbay/internal/vpn/vpntest"
)

// Not real keys: the fake helper never parses them.
const (
	testWGKey = "VGVzdC1vbmx5LXByaXZhdGUta2V5LW5vdC1yZWFsLTA="
	testPeer  = "VGVzdC1vbmx5LXB1YmxpYy1rZXktbm90LXJlYWwtMDA="
)

type vpnEnv struct {
	svc  *VPNService
	fake *vpntest.Fake
	dir  string

	mu     sync.Mutex
	events []VPNState
	raw    []string // each event as JSON
}

func newVPNEnv(t *testing.T, mode string) *vpnEnv {
	t.Helper()
	hs, _, dir := newHostService(t, fakeVerifier(secrets.Passed))
	e := &vpnEnv{svc: NewVPNService(hs.st, hs.vault), fake: vpntest.Install(t, mode), dir: dir}
	e.svc.helper = func() (string, error) { return e.fake.Path, nil }
	e.svc.detect = (&fakeDetect{}).detect
	e.svc.emit = func(name string, data any) {
		if name != VPNStateEvent {
			t.Errorf("event %q", name)
		}
		b, _ := json.Marshal(data)
		e.mu.Lock()
		e.events = append(e.events, data.(VPNState))
		e.raw = append(e.raw, string(b))
		e.mu.Unlock()
	}
	t.Cleanup(func() { e.svc.ServiceShutdown() })
	return e
}

func (e *vpnEnv) save(t *testing.T, autoReconnect bool) VPN {
	t.Helper()
	key := testWGKey
	v, err := e.svc.SaveVPN(VPN{
		Name: "office", Type: "wireguard", Tier: "app", Gateway: "vpn.example.test", Port: 51820,
		Address: []string{"10.9.0.2/32"}, PeerKey: testPeer, Routes: []string{"10.9.0.0/24"},
		DNSServers: []string{"10.9.0.1"}, DNSDomains: []string{"corp.example"}, AutoReconnect: autoReconnect,
		PrivateKey: &key,
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (e *vpnEnv) statuses(id string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, s := range e.events {
		if s.ID == id {
			out = append(out, s.Status)
		}
	}
	return out
}

// waitFor waits until the profile's latest event has status.
func (e *vpnEnv) waitFor(t *testing.T, id, status string) VPNState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		for i := len(e.events) - 1; i >= 0; i-- {
			if e.events[i].ID == id {
				if e.events[i].Status == status {
					s := e.events[i]
					e.mu.Unlock()
					return s
				}
				break
			}
		}
		e.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("profile never reached %s; events %v", status, e.statuses(id))
	panic("unreachable")
}

func TestSaveVPNSealsSecretsAndHidesThem(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	v := e.save(t, false)
	if !v.HasSecret || v.PrivateKey != nil {
		t.Fatalf("SaveVPN returned privateKey %v, hasSecret %v", v.PrivateKey, v.HasSecret)
	}
	files, _ := os.ReadDir(e.dir)
	for _, f := range files {
		b, _ := os.ReadFile(filepath.Join(e.dir, f.Name()))
		if strings.Contains(string(b), testWGKey) {
			t.Fatalf("%s holds the private key in plaintext", f.Name())
		}
	}
	// Saving again with no secrets keeps the saved key.
	v.Name = "office 2"
	if _, err := e.svc.SaveVPN(v); err != nil {
		t.Fatal(err)
	}
	list, err := e.svc.ListVPNs()
	if err != nil || len(list) != 1 || list[0].Name != "office 2" || !list[0].HasSecret {
		t.Fatalf("ListVPNs %+v, %v", list, err)
	}
	if _, err := e.svc.ConnectVPN(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.fake.Starts(t)[0].Secrets["privateKey"]; got != testWGKey {
		t.Fatalf("helper got private key %q", got)
	}
}

func TestSaveVPNMergesNewSecretsWithSavedOnes(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	v := e.save(t, false)
	psk := "VGVzdC1vbmx5LXByZXNoYXJlZC1rZXktbm90LXJlYWw="
	v.PresharedKey = &psk
	if _, err := e.svc.SaveVPN(v); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ConnectVPN(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.fake.Starts(t)[0].Secrets; got["privateKey"] != testWGKey || got["presharedKey"] != psk {
		t.Fatalf("helper got secrets %v", got)
	}
}

func TestChangingThePeerKeyDropsTheSavedSecrets(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	v := e.save(t, false)
	v.PeerKey = "VGVzdC1vbmx5LW90aGVyLXB1YmxpYy1rZXktbm90LXI="
	if _, err := e.svc.SaveVPN(v); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ConnectVPN(context.Background(), v.ID); err == nil {
		t.Fatal("connected with a private key saved for another server key")
	}
	if n := len(e.fake.Starts(t)); n != 0 {
		t.Fatalf("%d helper starts", n)
	}
}

func TestSaveVPNRefusesConfigInjection(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	for _, v := range []VPN{
		{Name: "x", Type: "wireguard", Tier: "app", Gateway: "vpn.example.test\nPostUp = rm -rf /"},
		{Name: "x", Type: "wireguard", Tier: "app", Gateway: "vpn.example.test", PeerKey: "k\n[Peer]"},
		{Name: "x", Type: "wireguard", Tier: "app", Gateway: "vpn.example.test", Routes: []string{"10.0.0.0/8\nDNS = 1.1.1.1"}},
	} {
		if _, err := e.svc.SaveVPN(v); err == nil {
			t.Fatalf("saved %+v", v)
		}
	}
}

func TestConnectVPNSpawnsOneHelperAndEmitsStatesInOrder(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	v := e.save(t, false)
	st, err := e.svc.ConnectVPN(context.Background(), v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != VPNConnected || st.Address[0] != vpntest.Address || st.Routes[0] != vpntest.Route || st.Since == 0 {
		t.Fatalf("state %+v", st)
	}
	if got := strings.Join(e.statuses(v.ID), ","); got != "connecting,connected" {
		t.Fatalf("events %s", got)
	}
	starts := e.fake.Starts(t)
	if len(starts) != 1 || starts[0].Type != proto.TypeWireGuard {
		t.Fatalf("starts %+v", starts)
	}
	conf := starts[0].Config
	for _, want := range []string{"Address = 10.9.0.2/32", "PublicKey = " + testPeer, "Endpoint = vpn.example.test:51820", "AllowedIPs = 10.9.0.0/24", "DNS = 10.9.0.1, corp.example", "MTU = 1280"} {
		if !strings.Contains(conf, want) {
			t.Fatalf("config lacks %q:\n%s", want, conf)
		}
	}
	if strings.Contains(conf, testWGKey) {
		t.Fatal("the config carries the private key")
	}

	// Connecting again returns the live state without a second helper.
	again, err := e.svc.ConnectVPN(context.Background(), v.ID)
	if err != nil || again.Status != VPNConnected || len(e.fake.Starts(t)) != 1 {
		t.Fatalf("second connect: %+v, %v, %d starts", again, err, len(e.fake.Starts(t)))
	}
}

func TestSocksIsTheLiveLoginAndNeverLeaves(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	v := e.save(t, false)
	if _, _, _, err := e.svc.socks(v.ID); apperr.CodeOf(err) != apperr.VPNDown {
		t.Fatalf("socks before connect: %v", err)
	}
	if _, err := e.svc.ConnectVPN(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	addr, user, pass, err := e.svc.socks(v.ID)
	start := e.fake.Starts(t)[0]
	if err != nil || addr != "127.0.0.1:"+strconv.Itoa(vpntest.Port) || user != start.SocksUser || pass != start.SocksPass || len(user) < 16 || len(pass) < 16 {
		t.Fatalf("socks %q %q %q %v; helper got %q %q", addr, user, pass, err, start.SocksUser, start.SocksPass)
	}
	states, _ := json.Marshal(e.svc.VPNStates())
	e.mu.Lock()
	defer e.mu.Unlock()
	allowed := map[string]bool{"id": true, "status": true, "reason": true, "need": true, "address": true, "routes": true, "since": true}
	for _, s := range append(e.raw, strings.Trim(string(states), "[]")) {
		if strings.Contains(s, pass) || strings.Contains(s, user) {
			t.Fatalf("%s carries the SOCKS login", s)
		}
		var fields map[string]any
		if err := json.Unmarshal([]byte(s), &fields); err != nil {
			t.Fatal(err)
		}
		for k := range fields {
			if !allowed[k] {
				t.Fatalf("%s carries field %q", s, k)
			}
		}
	}
}

func TestDroppedVPNReconnectsOnlyWithAutoReconnect(t *testing.T) {
	old := vpnRetryMin
	vpnRetryMin = 10 * time.Millisecond
	t.Cleanup(func() { vpnRetryMin = old })

	t.Run("on", func(t *testing.T) {
		e := newVPNEnv(t, vpntest.Crash)
		v := e.save(t, true)
		if _, err := e.svc.ConnectVPN(context.Background(), v.ID); err != nil {
			t.Fatal(err)
		}
		e.waitFor(t, v.ID, VPNDropped)
		e.waitFor(t, v.ID, VPNConnected)
		if n := len(e.fake.Starts(t)); n != 2 {
			t.Fatalf("%d helper starts, want 2", n)
		}
		if got := strings.Join(e.statuses(v.ID), ","); got != "connecting,connected,dropped,connecting,connected" {
			t.Fatalf("events %s", got)
		}
	})
	t.Run("off", func(t *testing.T) {
		e := newVPNEnv(t, vpntest.Crash)
		v := e.save(t, false)
		if _, err := e.svc.ConnectVPN(context.Background(), v.ID); err != nil {
			t.Fatal(err)
		}
		st := e.waitFor(t, v.ID, VPNDropped)
		if !strings.Contains(st.Reason, "stopped unexpectedly") {
			t.Fatalf("reason %q", st.Reason)
		}
		time.Sleep(200 * time.Millisecond)
		if n := len(e.fake.Starts(t)); n != 1 || e.svc.ConnectedVPNs() != 0 {
			t.Fatalf("%d helper starts, %d connected; want 1 and 0", n, e.svc.ConnectedVPNs())
		}
		if got := e.statuses(v.ID); got[len(got)-1] != VPNDropped {
			t.Fatalf("events %v", got)
		}
	})
}

func TestOneTimeCodeContinuesTheConnect(t *testing.T) {
	e := newVPNEnv(t, vpntest.OTP)
	v := e.save(t, false)
	done := make(chan VPNState, 1)
	go func() {
		st, err := e.svc.ConnectVPN(context.Background(), v.ID)
		if err != nil {
			t.Error(err)
		}
		done <- st
	}()
	if st := e.waitFor(t, v.ID, VPNSigningIn); st.Need != proto.NeedOTP {
		t.Fatalf("signing in needs %q", st.Need)
	}
	if err := e.svc.AnswerVPN(v.ID, vpntest.Code); err != nil {
		t.Fatal(err)
	}
	select {
	case st := <-done:
		if st.Status != VPNConnected {
			t.Fatalf("state %+v", st)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("connect did not finish")
	}
	if got := strings.Join(e.statuses(v.ID), ","); got != "connecting,signing_in,connecting,connected" {
		t.Fatalf("events %s", got)
	}
}

func TestConnectedVPNsCountsForQuit(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	v := e.save(t, false)
	if _, err := e.svc.ConnectVPN(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	if n := e.svc.ConnectedVPNs(); n != 1 {
		t.Fatalf("ConnectedVPNs %d, want 1", n)
	}
	if err := e.svc.DisconnectVPN(v.ID); err != nil {
		t.Fatal(err)
	}
	if n := e.svc.ConnectedVPNs(); n != 0 {
		t.Fatalf("ConnectedVPNs %d after disconnect", n)
	}
	if got := e.statuses(v.ID); got[len(got)-1] != VPNDisconnected {
		t.Fatalf("events %v", got)
	}
}

func TestConnectVPNFailsWithACode(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	v := e.save(t, false)
	e.svc.helper = func() (string, error) { return filepath.Join(t.TempDir(), "hostbay-vpn"), nil }
	st, err := e.svc.ConnectVPN(context.Background(), v.ID)
	if apperr.CodeOf(err) != apperr.HelperMissing || !strings.Contains(err.Error(), "Reinstall") || st.Status != VPNFailed {
		t.Fatalf("missing helper: %+v, %v", st, err)
	}

	v.Type = "openvpn"
	if _, err := e.svc.SaveVPN(v); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ConnectVPN(context.Background(), v.ID); apperr.CodeOf(err) != apperr.VPNFailed {
		t.Fatalf("unsupported type: %v", err)
	}
}

// A system-wide connect the routing plan cannot place is refused before
// any helper starts, naming the conflict (R13).
func TestSystemWideConnectRefusedOnConflict(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	v := e.save(t, false)
	v.Tier = store.VPNTierSystem
	if _, err := e.svc.SaveVPN(v); err != nil {
		t.Fatal(err)
	}
	asked := ""
	e.svc.checkConnect = func(id string) error { asked = id; return errors.New("Office is already a full tunnel") }
	_, err := e.svc.ConnectVPN(context.Background(), v.ID)
	if apperr.CodeOf(err) != apperr.VPNFailed || !strings.Contains(err.Error(), "Office") || asked != v.ID {
		t.Fatalf("conflict: %v (asked %q)", err, asked)
	}
	if n := e.svc.ConnectedVPNs(); n != 0 {
		t.Fatalf("ConnectedVPNs %d after a refused connect", n)
	}
}

// A connection remembers who made it, so an MCP token disconnects only its
// own: joining a run someone else started takes nothing over (U14).
func TestMCPVPNsDisconnectOnlyTheirOwn(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	v := e.save(t, false)
	m := &mcpVPNs{creds: &HostCredentials{vpns: e.svc}}
	if l := m.State(v.ID); l.By != "" || l.Status != "" {
		t.Fatalf("before connect %+v", l)
	}
	if _, err := e.svc.ConnectVPN(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	if l := m.State(v.ID); l.By != "user" || l.Status != VPNConnected {
		t.Fatalf("user's connection %+v", l)
	}
	if _, err := m.Connect(context.Background(), v.ID, "mcp:a"); err != nil || m.State(v.ID).By != "user" {
		t.Fatalf("joining took it over: %v %+v", err, m.State(v.ID))
	}
	if err := m.Disconnect(v.ID, "mcp:a"); err == nil || e.svc.ConnectedVPNs() != 1 {
		t.Fatalf("token disconnected the user's profile: %v", err)
	}

	e.svc.DisconnectVPN(v.ID)
	if l, err := m.Connect(context.Background(), v.ID, "mcp:a"); err != nil || l.By != "mcp:a" || l.Status != VPNConnected {
		t.Fatalf("token's connect %+v %v", l, err)
	}
	if err := m.Disconnect(v.ID, "mcp:b"); err == nil || e.svc.ConnectedVPNs() != 1 {
		t.Fatalf("another token disconnected it: %v", err)
	}
	if err := m.Disconnect(v.ID, "mcp:a"); err != nil || e.svc.ConnectedVPNs() != 0 || m.State(v.ID).Status != VPNDisconnected {
		t.Fatalf("own disconnect %v %+v", err, m.State(v.ID))
	}
}

// fakeDetect is what external VPN detection sees.
type fakeDetect struct {
	mu sync.Mutex
	s  detect.Snapshot
}

func (f *fakeDetect) set(s detect.Snapshot) {
	f.mu.Lock()
	f.s = s
	f.mu.Unlock()
}

func (f *fakeDetect) detect(context.Context) (detect.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.s, nil
}

func saveExternal(t *testing.T, s *VPNService, name string, edit func(*VPN)) VPN {
	t.Helper()
	v := VPN{Name: name, Type: "other", Tier: "external"}
	if edit != nil {
		edit(&v)
	}
	out, err := s.SaveVPN(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func stateOf(s *VPNService, id string) VPNState {
	for _, st := range s.VPNStates() {
		if st.ID == id {
			return st
		}
	}
	return VPNState{ID: id, Status: VPNDisconnected}
}

func p2pTunnel(iface, local, peer string) detect.Tunnel {
	return detect.Tunnel{Iface: iface, Local: local, Peer: peer, Shape: detect.PointToPoint}
}

// External profiles move between disconnected and connected as detection
// sees their tunnels (KTD7); two that cannot be told apart stay down and
// ask for a pin, and a pin settles both (AE4, R24).
func TestExternalProfilesFollowTheirTunnels(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	f := &fakeDetect{}
	e.svc.detect = f.detect
	ctx := context.Background()
	sase := saveExternal(t, e.svc, "Harmony SASE", func(v *VPN) { v.Type = "perimeter81" })
	lab := saveExternal(t, e.svc, "Lab", func(v *VPN) { v.Type = "wireguard" })
	f.set(detect.Snapshot{Tunnels: []detect.Tunnel{p2pTunnel("utun4", "10.255.0.54", "10.255.0.54"), p2pTunnel("utun6", "172.16.9.2", "172.16.9.1")}})
	if err := e.svc.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{sase.ID, lab.ID} {
		if st := stateOf(e.svc, id); st.Status != VPNDisconnected || !strings.Contains(st.Reason, "pin") {
			t.Fatalf("ambiguous profile state %+v, want disconnected asking for a pin", st)
		}
	}

	lab.Iface = "utun6"
	if _, err := e.svc.SaveVPN(lab); err != nil {
		t.Fatal(err)
	}
	if l, _ := e.svc.ListVPNs(); l[1].Iface != "utun6" {
		t.Fatalf("pin not saved: %+v", l)
	}
	e.svc.refresh(ctx)
	if st := stateOf(e.svc, lab.ID); st.Status != VPNConnected || st.Iface != "utun6" || st.Since == 0 {
		t.Fatalf("pinned profile %+v", st)
	}
	if st := stateOf(e.svc, sase.ID); st.Status != VPNConnected || st.Iface != "utun4" || !slices.Equal(st.Address, []string{"10.255.0.54"}) {
		t.Fatalf("the other profile %+v", st)
	}

	f.set(detect.Snapshot{})
	e.svc.refresh(ctx)
	e.svc.refresh(ctx)
	if got := e.statuses(sase.ID); !slices.Equal(got, []string{VPNDisconnected, VPNConnected, VPNDisconnected}) {
		t.Fatalf("events %v", got)
	}
	if e.svc.ConnectedVPNs() != 0 {
		t.Fatal("an external profile counted as Hostbay's own connection")
	}
}

// R22, R23: detection offers the VPNs no profile names yet, and a refresh
// drops a profile whose client this computer added and has since removed,
// unless a host uses it; a profile set up on another computer stays.
func TestDetectVPNsOffersNewOnesAndDropsRemovedOnes(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	f := &fakeDetect{}
	e.svc.detect = f.detect
	ctx := context.Background()
	f.set(detect.Snapshot{VPNs: []detect.VPN{
		{Name: "FortiClient VPN", Type: "sslvpn"},
		{Name: "Harmony SASE", Type: "perimeter81", Connected: true, Client: "/Applications/Harmony SASE.app"},
	}})
	saveExternal(t, e.svc, "Office", func(v *VPN) { v.Client = "FortiClient VPN" })
	offers, err := e.svc.DetectVPNs(ctx)
	if err != nil || len(offers) != 1 || offers[0].Name != "Harmony SASE" {
		t.Fatalf("offers %+v, %v", offers, err)
	}

	dir := t.TempDir()
	app := func(name string) string {
		p := filepath.Join(dir, name+".app")
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	gone := saveExternal(t, e.svc, "Gone VPN", func(v *VPN) { v.ClientPath = app("Gone") })
	used := saveExternal(t, e.svc, "Used VPN", func(v *VPN) { v.ClientPath = app("Used") })
	saveExternal(t, e.svc, "Elsewhere VPN", nil)
	if err := e.svc.st.Edit(func(d *store.Data) error {
		d.PutHost(store.Host{Alias: "db", Hostname: "10.0.0.5", VPNID: &used.ID})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(filepath.Join(dir, "Gone.app"))
	os.RemoveAll(filepath.Join(dir, "Used.app"))
	if _, err := e.svc.DetectVPNs(ctx); err != nil {
		t.Fatal(err)
	}
	var names []string
	l, _ := e.svc.ListVPNs()
	for _, v := range l {
		names = append(names, v.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"Elsewhere VPN", "Office", "Used VPN"}) {
		t.Fatalf("profiles %v", names)
	}
	if loc, _ := e.svc.st.Local(); loc.VPNClients[gone.ID] != "" {
		t.Fatal("the dropped profile's client path stayed")
	}
}

// A profile that points at the app by path is that app, whatever the app
// calls itself: Harmony SASE installs as "Check Point SASE", and detection
// must not offer it again as a new VPN.
func TestDetectVPNsRecognisesAProfilesAppByPath(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	f := &fakeDetect{}
	e.svc.detect = f.detect
	app := filepath.Join(t.TempDir(), "Check Point SASE.app")
	if err := os.Mkdir(app, 0o700); err != nil {
		t.Fatal(err)
	}
	saveExternal(t, e.svc, "Harmony SASE", func(v *VPN) { v.Client = "Harmony SASE"; v.ClientPath = app })
	f.set(detect.Snapshot{VPNs: []detect.VPN{{Name: "Check Point SASE", Type: "perimeter81", Client: app}}})
	if offers, err := e.svc.DetectVPNs(context.Background()); err != nil || len(offers) != 0 {
		t.Fatalf("offers %+v, %v", offers, err)
	}
}

// Hostbay never runs a helper for an external profile, and launches only
// the client program saved on this computer, never a synced name (KTD4).
func TestExternalProfilesAreLeftToTheirOwnApp(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	e.svc.detect = (&fakeDetect{}).detect
	ext := saveExternal(t, e.svc, "Office", func(v *VPN) { v.Client = "FortiClient VPN" })
	if _, err := e.svc.ConnectVPN(context.Background(), ext.ID); apperr.CodeOf(err) != apperr.VPNFailed {
		t.Fatalf("connect external: %v", err)
	}
	if len(e.fake.Starts(t)) != 0 || len(e.statuses(ext.ID)) != 0 {
		t.Fatalf("connect external started a helper or changed state: %v", e.statuses(ext.ID))
	}
	if err := e.svc.LaunchVPNClient(ext.ID); err == nil || !strings.Contains(err.Error(), "client program") {
		t.Fatalf("launch with no local client: %v", err)
	}
	for _, p := range []string{"Applications/Forti.app", filepath.Join(t.TempDir(), "missing.app")} {
		ext.ClientPath = p
		if _, err := e.svc.SaveVPN(ext); err == nil {
			t.Fatalf("saved client path %q", p)
		}
	}
}

// A profile's routes name where its traffic goes, which another VPN's
// tunnel may hold too: only its own addresses label a tunnel (R24).
func TestRoutesNeverClaimAnotherVPNsTunnel(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	f := &fakeDetect{}
	e.svc.detect = f.detect
	sase := saveExternal(t, e.svc, "Harmony SASE", func(v *VPN) { v.Type = "perimeter81" })
	office := saveExternal(t, e.svc, "Office", func(v *VPN) { v.Type = "anyconnect"; v.Routes = []string{"10.0.0.0/8"} })
	f.set(detect.Snapshot{Tunnels: []detect.Tunnel{p2pTunnel("utun4", "10.255.0.54", "10.255.0.54")}})
	e.svc.refresh(context.Background())
	if st := stateOf(e.svc, office.ID); st.Status == VPNConnected {
		t.Fatalf("Office took Harmony's tunnel by its route: %+v", st)
	}
	if st := stateOf(e.svc, sase.ID); st.Status == VPNConnected {
		t.Fatalf("two peer profiles, one tunnel: %+v, want no guess", st)
	}

	office.Address = []string{"10.212.0.0/16"}
	sase.Address = []string{"10.255.0.0/16"}
	e.svc.SaveVPN(office)
	e.svc.SaveVPN(sase)
	e.svc.refresh(context.Background())
	if st := stateOf(e.svc, sase.ID); st.Status != VPNConnected || st.Iface != "utun4" {
		t.Fatalf("Harmony by its own range: %+v", st)
	}
}

// The profile form offers the tunnels up now to pin one; it never gets nil.
func TestVPNTunnelsListsWhatDetectionSees(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	f := &fakeDetect{}
	e.svc.detect = f.detect
	f.set(detect.Snapshot{})
	if got, err := e.svc.VPNTunnels(context.Background()); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("no tunnels: %+v, %v", got, err)
	}
	f.set(detect.Snapshot{Tunnels: []detect.Tunnel{{Iface: "utun4", Local: "10.212.134.41", VPN: "FortiClient VPN"}}})
	if got, err := e.svc.VPNTunnels(context.Background()); err != nil || len(got) != 1 || got[0].Iface != "utun4" {
		t.Fatalf("tunnels: %+v, %v", got, err)
	}
}

// A client Hostbay cannot drive is left to its own app: connecting with no
// client program saved asks for one, and disconnecting points at the app.
func TestVPNClientWithoutACommandLineIsLeftToItsApp(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	v := saveExternal(t, e.svc, "Harmony SASE", nil)
	if err := e.svc.ConnectVPNClient(context.Background(), v.ID); err == nil || !strings.Contains(err.Error(), "client program") {
		t.Fatalf("connect: %v", err)
	}
	if err := e.svc.DisconnectVPNClient(context.Background(), v.ID); err == nil || !strings.Contains(err.Error(), "own app") {
		t.Fatalf("disconnect: %v", err)
	}
	if err := e.svc.ConnectVPNClient(context.Background(), "nope"); err == nil {
		t.Fatal("an unknown profile connected")
	}
}
