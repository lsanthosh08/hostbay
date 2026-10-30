package app

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"hostbay/internal/apperr"
	"hostbay/internal/backup"
	"hostbay/internal/secrets"
	"hostbay/internal/store"
	"hostbay/internal/vpn/proto"
	"hostbay/internal/vpn/vpntest"
)

// A valid pin that is not the fake gateway's.
const otherPin = "pin-sha256:QW5vdGhlci1nYXRld2F5LWNlcnRpZmljYXRlLXBpbjA="

func (e *vpnEnv) saveProfile(t *testing.T, v VPN) VPN {
	t.Helper()
	if v.Name == "" {
		v.Name = v.Type
	}
	if v.Tier == "" {
		v.Tier = store.VPNTierApp
	}
	out, err := e.svc.SaveVPN(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func strp(s string) *string { return &s }

func keys(m map[string]string) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// configKeys decodes a start's Config, failing on a field the engine does
// not take, and returns the fields it carries.
func configKeys(t *testing.T, conf string, into any) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(conf))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		t.Fatalf("config %s: %v", conf, err)
	}
	var m map[string]any
	json.Unmarshal([]byte(conf), &m)
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// The engines take a fixed set of fields (internal/vpn/openconnect and
// internal/vpn/tailnet); anything else would be refused, or worse, obeyed.
func TestStartMsgSendsEachEngineExactlyItsFields(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	type oc struct {
		Gateway    string `json:"gateway"`
		User       string `json:"user"`
		ServerCert string `json:"serverCert"`
		Browser    bool   `json:"browser"`
	}
	for _, typ := range []string{proto.TypeAnyConnect, proto.TypeSSLVPN, proto.TypeGlobalProtect, proto.TypePulse, proto.TypeNetworkConnect, proto.TypeF5, proto.TypeArray} {
		v := e.saveProfile(t, VPN{Type: typ, Gateway: "vpn.example.test", Port: 8443, User: "alice", Auth: "password", PeerKey: vpntest.Pin, Password: strp("s3cret")})
		m, _, err := e.svc.startMsg(v.ID)
		if err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
		var c oc
		if got := configKeys(t, m.Config, &c); !slices.Equal(got, []string{"gateway", "serverCert", "user"}) {
			t.Fatalf("%s: config fields %v", typ, got)
		}
		if m.Type != typ || c.Gateway != "vpn.example.test:8443" || c.User != "alice" || c.ServerCert != vpntest.Pin || c.Browser {
			t.Fatalf("%s: start %s %+v", typ, m.Type, c)
		}
		if !slices.Equal(keys(m.Secrets), []string{"password"}) || m.Secrets["password"] != "s3cret" {
			t.Fatalf("%s: secrets %v", typ, keys(m.Secrets))
		}
		if strings.Contains(m.Config, "s3cret") {
			t.Fatalf("%s: the password is in the config", typ)
		}
	}

	// Browser sign-in is AnyConnect's only, and sends no password line.
	v := e.saveProfile(t, VPN{Type: proto.TypeAnyConnect, Gateway: "sso.example.test", Auth: "browser"})
	m, _, err := e.svc.startMsg(v.ID)
	var c oc
	if err != nil || !slices.Equal(configKeys(t, m.Config, &c), []string{"browser", "gateway"}) || !c.Browser || c.Gateway != "sso.example.test" ||
		!slices.Equal(keys(m.Secrets), []string{"password"}) || m.Secrets["password"] != "" {
		t.Fatalf("browser sign-in: %+v %v %v", c, m.Secrets, err)
	}
	v = e.saveProfile(t, VPN{Type: proto.TypeGlobalProtect, Gateway: "gp.example.test", Auth: "browser"})
	if m, _, err := e.svc.startMsg(v.ID); err != nil || strings.Contains(m.Config, "browser") {
		t.Fatalf("browser on a type without it: %s %v", m.Config, err)
	}

	// A port joins the gateway only where it has none.
	for gw, want := range map[string]string{
		"vpn.example.test:10443": "vpn.example.test:10443",
		"vpn.example.test/group": "vpn.example.test:443/group",
		"2001:db8::1":            "[2001:db8::1]:443",
		"[2001:db8::1]":          "[2001:db8::1]:443",
	} {
		v := e.saveProfile(t, VPN{Type: proto.TypeF5, Gateway: gw, Port: 443, Auth: "password"})
		m, _, err := e.svc.startMsg(v.ID)
		var c oc
		if err == nil {
			configKeys(t, m.Config, &c)
		}
		if c.Gateway != want {
			t.Fatalf("gateway %s port 443 -> %q, want %q (%v)", gw, c.Gateway, want, err)
		}
	}

	// Certificate sign-in is not built for OpenConnect: refused, not sent
	// as a password sign-in.
	v = e.saveProfile(t, VPN{Type: proto.TypeAnyConnect, Gateway: "vpn.example.test", Auth: "certificate"})
	if _, _, err := e.svc.startMsg(v.ID); apperr.CodeOf(err) != apperr.VPNFailed || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("certificate sign-in: %v", err)
	}

	type ts struct {
		Dir        string `json:"dir"`
		ControlURL string `json:"controlURL"`
		Hostname   string `json:"hostname"`
	}
	for _, control := range []string{"", "https://headscale.example.test"} {
		v := e.saveProfile(t, VPN{Type: proto.TypeTailscale, Gateway: control, AuthKey: strp("tskey-auth-test")})
		m, p, err := e.svc.startMsg(v.ID)
		if err != nil {
			t.Fatal(err)
		}
		var c ts
		if got := configKeys(t, m.Config, &c); !slices.Equal(got, []string{"controlURL", "dir", "hostname"}) {
			t.Fatalf("tailscale config fields %v", got)
		}
		if c.Dir != filepath.Join(e.svc.st.Dir(), "vpn", v.ID) || c.ControlURL != control || !strings.HasSuffix(c.Hostname, "-hostbay") || len(c.Hostname) > 63 {
			t.Fatalf("tailscale config %+v", c)
		}
		if fi, err := os.Stat(c.Dir); err != nil || fi.Mode().Perm() != 0o700 {
			t.Fatalf("state folder: %v %v", fi, err)
		}
		if !slices.Equal(keys(m.Secrets), []string{"authKey", "nodeState"}) || m.Secrets["authKey"] != "tskey-auth-test" || m.Secrets["nodeState"] != "" {
			t.Fatalf("tailscale secrets %v", keys(m.Secrets))
		}
		if p == nil || p.ID != v.ID || strings.Contains(m.Config, "tskey") {
			t.Fatalf("profile %v, config %s", p, m.Config)
		}
	}
}

func TestNodeHostnameIsADNSLabel(t *testing.T) {
	for in, want := range map[string]string{
		"Santhoshs-MacBook-Pro.local": "santhoshs-macbook-pro-hostbay",
		"desk_01":                     "desk-01-hostbay",
		"":                            "hostbay",
		strings.Repeat("a", 80):       strings.Repeat("a", 55) + "-hostbay",
	} {
		if got := nodeHostname(in); got != want {
			t.Fatalf("nodeHostname(%q) = %q, want %q", in, got, want)
		}
	}
}

func (e *vpnEnv) nodeState(t *testing.T, id string) string {
	t.Helper()
	l, err := e.svc.st.Local()
	if err != nil {
		t.Fatal(err)
	}
	return l.VPNNodeStates[id]
}

// waitNode waits until the profile's saved node state opens to want.
func (e *vpnEnv) waitNode(t *testing.T, v *store.VPN, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sealed := e.nodeState(t, v.ID); sealed != "" {
			if b, err := e.svc.vault.Open(sealed, v.NodeStateAAD()); err == nil && string(b) == want {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("node state never became %q", want)
}

func (e *vpnEnv) profile(t *testing.T, id string) *store.VPN {
	t.Helper()
	d, err := e.svc.st.Data()
	if err != nil {
		t.Fatal(err)
	}
	return d.VPN(id)
}

// A Tailscale node's keys are sealed on this computer only: in local.json,
// never in what sync or a backup carries, never in cleartext (KTD3).
func TestNodeStateIsSealedAndKeptOnThisDevice(t *testing.T) {
	e := newVPNEnv(t, vpntest.Node)
	v := e.saveProfile(t, VPN{Type: proto.TypeTailscale, Gateway: "https://headscale.example.test"})
	if _, err := e.svc.ConnectVPN(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	p := e.profile(t, v.ID)
	e.waitNode(t, p, vpntest.NodeUp)
	// The state a node writes while closing is the one the next start gets.
	e.svc.DisconnectVPN(v.ID)
	e.waitNode(t, p, vpntest.NodeClosing)

	sealed := e.nodeState(t, v.ID)
	if l, err := NewSettingsService(e.svc.st).GetLocal(); err != nil || l.VPNNodeStates != nil {
		t.Fatalf("the page is sent the node states: %v", err)
	}
	other := *p
	other.ID = "another-profile"
	if _, err := e.svc.vault.Open(sealed, other.NodeStateAAD()); err == nil {
		t.Fatal("another profile's node state opens")
	}
	if _, err := e.svc.vault.Open(sealed, p.SecretAAD()); err == nil {
		t.Fatal("the node state opens as the profile's secrets")
	}

	d, _ := e.svc.st.Data()
	synced, _ := json.Marshal(d)
	bak, err := backup.Export(d, e.svc.vault, "", "backup passphrase")
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := backup.Read(bak, "backup passphrase")
	if err != nil {
		t.Fatal(err)
	}
	restored, _ := json.Marshal(bundle)
	for _, s := range []string{vpntest.NodeUp, vpntest.NodeClosing, sealed} {
		if strings.Contains(string(synced), s) || strings.Contains(string(restored), s) {
			t.Fatalf("synced data or a backup carries the node state")
		}
	}
	filepath.WalkDir(e.svc.st.Dir(), func(path string, de fs.DirEntry, err error) error {
		if err == nil && !de.IsDir() {
			b, _ := os.ReadFile(path)
			if strings.Contains(string(b), vpntest.NodeUp) || strings.Contains(string(b), vpntest.NodeClosing) {
				t.Errorf("%s holds the node state in cleartext", path)
			}
		}
		return nil
	})

	if _, err := e.svc.ConnectVPN(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	starts := e.fake.Starts(t)
	if got := starts[len(starts)-1].Secrets["nodeState"]; got != vpntest.NodeClosing {
		t.Fatalf("second start's node state %q", got)
	}
	e.svc.DisconnectVPN(v.ID)
	e.waitNode(t, p, vpntest.NodeClosing)

	// A new control server is another tailnet: the old node is dropped.
	v.Gateway = "https://other.example.test"
	if _, err := e.svc.SaveVPN(v); err != nil {
		t.Fatal(err)
	}
	if e.nodeState(t, v.ID) != "" {
		t.Fatal("node state kept after the control server changed")
	}
}

func TestSignInAgainAndDeleteRemoveTheNode(t *testing.T) {
	e := newVPNEnv(t, vpntest.Node)
	v := e.saveProfile(t, VPN{Type: proto.TypeTailscale})
	if _, err := e.svc.ConnectVPN(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	p := e.profile(t, v.ID)
	e.waitNode(t, p, vpntest.NodeUp)
	if err := e.svc.SignInAgainVPN(v.ID); err != nil {
		t.Fatal(err)
	}
	// Nothing the closing node wrote comes back.
	time.Sleep(100 * time.Millisecond)
	if e.nodeState(t, v.ID) != "" || e.svc.ConnectedVPNs() != 0 {
		t.Fatalf("node state %q, %d connected after sign in again", e.nodeState(t, v.ID), e.svc.ConnectedVPNs())
	}

	if _, err := e.svc.ConnectVPN(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	e.waitNode(t, p, vpntest.NodeUp)
	dir := filepath.Join(e.svc.st.Dir(), "vpn", v.ID)
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DeleteVPN(v.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("state folder left: %v", err)
	}
	if e.nodeState(t, v.ID) != "" {
		t.Fatal("node state left after delete")
	}
}

func TestUntrustedGatewayAsksAndTrustKeepsThePassword(t *testing.T) {
	e := newVPNEnv(t, vpntest.Cert)
	v := e.saveProfile(t, VPN{Type: proto.TypeAnyConnect, Gateway: "vpn.example.test", Port: 443, User: "alice", Auth: "password", Password: strp("s3cret")})
	st, err := e.svc.ConnectVPN(context.Background(), v.ID)
	var ae *apperr.Error
	if !asAppErr(err, &ae) || ae.Details["reason"] != proto.CodeCertUnknown || ae.Details["serverCert"] != vpntest.Pin {
		t.Fatalf("connect: %v", err)
	}
	if st.Status != VPNFailed || st.Code != proto.CodeCertUnknown || st.ServerCert != vpntest.Pin {
		t.Fatalf("state %+v", st)
	}

	// Only the certificate the gateway presented can be trusted.
	if err := e.svc.TrustVPNCert(v.ID, otherPin); err == nil {
		t.Fatal("trusted a certificate the gateway did not present")
	}
	if err := e.svc.TrustVPNCert(v.ID, vpntest.Pin); err != nil {
		t.Fatal(err)
	}
	if p := e.profile(t, v.ID); p.PeerKey != vpntest.Pin {
		t.Fatalf("pinned %q", p.PeerKey)
	}
	if _, err := e.svc.ConnectVPN(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	starts := e.fake.Starts(t)
	last := starts[len(starts)-1]
	if last.Secrets["password"] != "s3cret" || !strings.Contains(last.Config, vpntest.Pin) {
		t.Fatalf("after trust the helper got %v %s", keys(last.Secrets), last.Config)
	}
}

// A changed certificate is refused and never trusted in its place (R21,
// AE8): the saved one can only be forgotten, keeping the password, and the
// next connect asks about the new one as unknown.
func TestChangedGatewayCertificateIsRefusedAndOnlyForgotten(t *testing.T) {
	e := newVPNEnv(t, vpntest.Cert)
	v := e.saveProfile(t, VPN{Type: proto.TypeAnyConnect, Gateway: "vpn.example.test", Auth: "password", PeerKey: otherPin, Password: strp("s3cret")})
	st, err := e.svc.ConnectVPN(context.Background(), v.ID)
	var ae *apperr.Error
	if !asAppErr(err, &ae) || ae.Details["reason"] != proto.CodeCertChanged || ae.Details["serverCert"] != vpntest.Pin || ae.Details["pinned"] != otherPin {
		t.Fatalf("connect: %v %+v", err, ae)
	}
	if st.Code != proto.CodeCertChanged || st.ServerCert != vpntest.Pin {
		t.Fatalf("state %+v", st)
	}
	if err := e.svc.TrustVPNCert(v.ID, vpntest.Pin); apperr.CodeOf(err) != apperr.HostKeyChanged {
		t.Fatalf("trusting a changed certificate: %v", err)
	}
	if p := e.profile(t, v.ID); p.PeerKey != otherPin {
		t.Fatalf("pin %q after a refused trust", p.PeerKey)
	}

	if err := e.svc.ForgetVPNCert(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	if p := e.profile(t, v.ID); p.PeerKey != "" {
		t.Fatalf("pin %q after forgetting", p.PeerKey)
	}
	st, err = e.svc.ConnectVPN(context.Background(), v.ID)
	if !asAppErr(err, &ae) || ae.Details["reason"] != proto.CodeCertUnknown || st.Code != proto.CodeCertUnknown {
		t.Fatalf("connect after forgetting: %v %+v", err, st)
	}
	if starts := e.fake.Starts(t); starts[len(starts)-1].Secrets["password"] != "s3cret" {
		t.Fatal("the password did not survive forgetting the certificate")
	}
	if err := e.svc.TrustVPNCert(v.ID, vpntest.Pin); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ConnectVPN(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	if starts := e.fake.Starts(t); starts[len(starts)-1].Secrets["password"] != "s3cret" {
		t.Fatal("the password did not survive trusting the new certificate")
	}
}

// Forgetting a certificate needs the saved secrets open: on a locked vault
// it changes nothing, so the password is never lost.
func TestForgetVPNCertOnALockedVaultChangesNothing(t *testing.T) {
	e := newVPNEnv(t, vpntest.Cert)
	v := e.saveProfile(t, VPN{Type: proto.TypeAnyConnect, Gateway: "vpn.example.test", Auth: "password", PeerKey: otherPin, Password: strp("s3cret")})
	mp, err := store.Load[map[string]any](e.svc.st, "masterpassword.json")
	if err != nil {
		t.Fatal(err)
	}
	mp["unlocksKey"] = true // the key opens only with the master password
	if err := store.Save(e.svc.st, "masterpassword.json", mp); err != nil {
		t.Fatal(err)
	}
	e.svc.vault = secrets.New(e.svc.st, fakeVerifier(secrets.Passed)) // as after a restart: locked
	if err := e.svc.ForgetVPNCert(context.Background(), v.ID); apperr.CodeOf(err) != apperr.SecretNeeded {
		t.Fatalf("forgetting on a locked vault: %v", err)
	}
	if p := e.profile(t, v.ID); p.PeerKey != otherPin {
		t.Fatalf("pin %q", p.PeerKey)
	}
}

func asAppErr(err error, ae **apperr.Error) bool { return errors.As(err, ae) }

// A coded failure needs the user, so auto-reconnect does not retry it.
func TestCodedFailureEndsAutoReconnect(t *testing.T) {
	old := vpnRetryMin
	vpnRetryMin = 10 * time.Millisecond
	t.Cleanup(func() { vpnRetryMin = old })
	e := newVPNEnv(t, vpntest.CrashThenCert)
	v := e.saveProfile(t, VPN{Type: proto.TypeAnyConnect, Gateway: "vpn.example.test", Auth: "password", AutoReconnect: true})
	if _, err := e.svc.ConnectVPN(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	st := e.waitFor(t, v.ID, VPNFailed)
	time.Sleep(200 * time.Millisecond)
	if n := len(e.fake.Starts(t)); n != 2 || e.svc.ConnectedVPNs() != 0 || st.Code != proto.CodeCertChanged {
		t.Fatalf("%d starts, %d connected, state %+v", n, e.svc.ConnectedVPNs(), st)
	}
	if got := strings.Join(e.statuses(v.ID), ","); got != "connecting,connected,dropped,connecting,failed" {
		t.Fatalf("events %s", got)
	}
}

func TestNeedsSystemWideSaysSo(t *testing.T) {
	e := newVPNEnv(t, vpntest.SystemWide)
	v := e.saveProfile(t, VPN{Type: proto.TypeAnyConnect, Gateway: "vpn.example.test", Auth: "password"})
	st, err := e.svc.ConnectVPN(context.Background(), v.ID)
	if err == nil || !strings.Contains(st.Reason, "system-wide mode on Windows") || st.Code != proto.CodeNeedsSystemWide {
		t.Fatalf("%+v %v", st, err)
	}
}

// A reconnect waits for the profile's last helper to exit, so two never
// run on one profile's state at once.
func TestReconnectWaitsForThePreviousHelper(t *testing.T) {
	e := newVPNEnv(t, vpntest.SlowExit)
	v := e.save(t, false)
	for range 2 {
		if _, err := e.svc.ConnectVPN(context.Background(), v.ID); err != nil {
			t.Fatal(err)
		}
		e.svc.DisconnectVPN(v.ID)
	}
	if _, err := e.svc.ConnectVPN(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	if e.fake.Overlapped() {
		t.Fatal("a helper started while the last was still exiting")
	}
}

func TestBrowserSignInOpensOnlyWebURLs(t *testing.T) {
	for url, open := range map[string]bool{
		"https://sso.example.test/login?x=1": true,
		"http://sso.example.test/":           true,
		"javascript:alert(1)":                false,
		"file:///etc/passwd":                 false,
		"sso.example.test":                   false,
	} {
		e := newVPNEnv(t, vpntest.Browser)
		e.fake.SetURL(t, url)
		var mu sync.Mutex
		var opened []string
		e.svc.openURL = func(u string) { mu.Lock(); opened = append(opened, u); mu.Unlock() }
		v := e.saveProfile(t, VPN{Type: proto.TypeAnyConnect, Gateway: "sso.example.test", Auth: "browser"})
		go e.svc.ConnectVPN(context.Background(), v.ID)
		st := e.waitFor(t, v.ID, VPNSigningIn)
		var got []string
		// The browser opens just after the state.
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			mu.Lock()
			got = slices.Clone(opened)
			mu.Unlock()
			if len(got) > 0 || !open && time.Until(deadline) < 1900*time.Millisecond {
				break
			}
		}
		if st.Need != proto.NeedBrowser || (st.URL == url) != open || (len(got) == 1 && got[0] == url) != open || (!open && len(got) != 0) {
			t.Fatalf("%s: state %+v, opened %v", url, st, got)
		}
		e.svc.DisconnectVPN(v.ID)
	}
}
