package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"hostbay/internal/apperr"
	"hostbay/internal/secrets"
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
