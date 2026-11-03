package app

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"hostbay/internal/apperr"
	"hostbay/internal/importer"
	"hostbay/internal/store"
	"hostbay/internal/vpn/browser"
	"hostbay/internal/vpn/vpntest"
)

// fakeChrome installs a Chrome whose user data folder holds profiles
// "Default" and "Profile 3" (shown as Work), and records launches in place
// of starting anything.
func fakeChrome(t *testing.T) (userData string, launched *[]*exec.Cmd) {
	t.Helper()
	userData = t.TempDir()
	if err := os.WriteFile(filepath.Join(userData, "Local State"),
		[]byte(`{"profile":{"info_cache":{"Default":{"name":"Personal"},"Profile 3":{"name":"Work"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	launched = &[]*exec.Cmd{}
	oldInstalled, oldStart := installedBrowsers, startBrowser
	installedBrowsers = func() []browser.Browser {
		return []browser.Browser{{ID: "chrome", Name: "Google Chrome", Exe: "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", UserData: userData}}
	}
	startBrowser = func(c *exec.Cmd) error { *launched = append(*launched, c); return nil }
	t.Cleanup(func() { installedBrowsers, startBrowser = oldInstalled, oldStart })
	return userData, launched
}

// Launch connects the app-scoped VPN and starts the profile pointed at the
// helper's no-login browser listener; the port never reaches an event.
func TestLaunchBrowserGoesThroughTheVPN(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	_, launched := fakeChrome(t)
	v := e.save(t, false)
	// A mapping imported by display name still finds the profile's folder.
	if err := e.svc.SaveBrowserMapping(store.BrowserMapping{Browser: "chrome", Profile: "Work", VPN: v.ID}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.LaunchBrowser(context.Background(), "chrome", "Work"); err != nil {
		t.Fatal(err)
	}
	if stateOf(e.svc, v.ID).Status != VPNConnected || len(e.fake.Starts(t)) != 1 {
		t.Fatalf("VPN %v, %d helper starts", stateOf(e.svc, v.ID), len(e.fake.Starts(t)))
	}
	if len(*launched) != 1 {
		t.Fatalf("%d launches", len(*launched))
	}
	c := (*launched)[0]
	// The profile carries only 10.9.0.0/24, so only that and corp.example take the
	// proxy; the rest of the internet goes direct, which the VPN has no path for.
	enc, ok := strings.CutPrefix(c.Args[2], "--proxy-pac-url=data:application/x-ns-proxy-autoconfig;base64,")
	pac, _ := base64.StdEncoding.DecodeString(enc)
	if c.Path != "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" || len(c.Args) != 4 || !ok || c.Args[1] != "--profile-directory=Profile 3" ||
		c.Args[3] != "--force-webrtc-ip-handling-policy=disable_non_proxied_udp" ||
		!strings.Contains(string(pac), "SOCKS5 127.0.0.1:"+strconv.Itoa(vpntest.BrowserPort)) ||
		strings.Count(string(pac), `isInNet(ip, "10.9.0.0", "255.255.255.0")`) != 1 || !strings.Contains(string(pac), ".corp.example") {
		t.Fatalf("launched %q %q\n%s", c.Path, c.Args, pac)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range e.raw {
		if strings.Contains(r, strconv.Itoa(vpntest.BrowserPort)) {
			t.Fatalf("event carries the browser port: %s", r)
		}
	}
}

// AE6: a profile mapped to a VPN with no SOCKS endpoint is not launched,
// and the user is told why; nothing connects.
func TestLaunchBrowserRefusesAVPNWithoutSOCKS(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	_, launched := fakeChrome(t)
	ext := saveExternal(t, e.svc, "Office", nil)
	if err := e.svc.SaveBrowserMapping(store.BrowserMapping{Browser: "chrome", Profile: "Profile 3", VPN: ext.ID}); err != nil {
		t.Fatal(err)
	}
	err := e.svc.LaunchBrowser(context.Background(), "chrome", "Profile 3")
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != apperr.VPNFailed || ae.Details["reason"] != "no-socks" || !strings.Contains(err.Error(), "no SOCKS endpoint") {
		t.Fatalf("launch: %v", err)
	}
	if len(*launched) != 0 || len(e.fake.Starts(t)) != 0 {
		t.Fatalf("%d launches, %d helper starts", len(*launched), len(e.fake.Starts(t)))
	}
}

// A browser already running would open the profile itself, unproxied, so
// Hostbay refuses and says to quit it (KTD11).
func TestLaunchBrowserRefusesWhileTheBrowserRuns(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	userData, launched := fakeChrome(t)
	liveLock(t, userData)
	v := e.save(t, false)
	e.svc.SaveBrowserMapping(store.BrowserMapping{Browser: "chrome", Profile: "Default", VPN: v.ID})
	err := e.svc.LaunchBrowser(context.Background(), "chrome", "Default")
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("launch: %v", err)
	}
	if len(*launched) != 0 || len(e.fake.Starts(t)) != 0 {
		t.Fatalf("%d launches, %d helper starts", len(*launched), len(e.fake.Starts(t)))
	}
}

// A browser opened while the VPN connects is caught before the launch,
// which would otherwise hand the profile to it unproxied.
func TestLaunchBrowserRechecksTheLockAfterConnecting(t *testing.T) {
	e := newVPNEnv(t, vpntest.OTP)
	userData, launched := fakeChrome(t)
	v := e.save(t, false)
	e.svc.SaveBrowserMapping(store.BrowserMapping{Browser: "chrome", Profile: "Default", VPN: v.ID})
	done := make(chan error, 1)
	go func() { done <- e.svc.LaunchBrowser(context.Background(), "chrome", "Default") }()
	e.waitFor(t, v.ID, VPNSigningIn)
	liveLock(t, userData)
	if err := e.svc.AnswerVPN(v.ID, vpntest.Code); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("launch: %v", err)
	}
	if len(*launched) != 0 {
		t.Fatalf("%d launches", len(*launched))
	}
}

// liveLock makes the browser look running: this process holds its lock.
func liveLock(t *testing.T, userData string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows browsers hold a lockfile, not a SingletonLock")
	}
	host, _ := os.Hostname()
	if err := os.Symlink(host+"-"+strconv.Itoa(os.Getpid()), filepath.Join(userData, "SingletonLock")); err != nil {
		t.Fatal(err)
	}
}

// Mappings are kept per browser profile on this computer; saving one again
// replaces it, and delete removes it.
func TestBrowserMappingsSaveReplaceDelete(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	v := e.save(t, false)
	for _, bad := range []store.BrowserMapping{
		{Browser: "netscape", Profile: "Default", VPN: v.ID},
		{Browser: "chrome", Profile: " ", VPN: v.ID},
		{Browser: "chrome", Profile: "Default", VPN: "missing"},
	} {
		if err := e.svc.SaveBrowserMapping(bad); err == nil {
			t.Errorf("saved %+v", bad)
		}
	}
	e.svc.SaveBrowserMapping(store.BrowserMapping{Browser: "chrome", Profile: "Default", VPN: v.ID})
	other := e.save(t, false)
	e.svc.SaveBrowserMapping(store.BrowserMapping{Browser: "chrome", Profile: "Default", VPN: other.ID})
	got, err := e.svc.BrowserMappings()
	if err != nil || len(got) != 1 || got[0].VPN != other.ID {
		t.Fatalf("mappings %+v, %v", got, err)
	}
	if err := e.svc.DeleteBrowserMapping("chrome", "Default"); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.svc.BrowserMappings(); len(got) != 0 {
		t.Fatalf("after delete %+v", got)
	}
}

// Deleting a profile takes its browser mappings with it, not leaving rows
// that point at nothing.
func TestDeletingAVPNDropsItsBrowserMappings(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	v, keep := e.save(t, false), e.save(t, false)
	e.svc.SaveBrowserMapping(store.BrowserMapping{Browser: "chrome", Profile: "Default", VPN: v.ID})
	e.svc.SaveBrowserMapping(store.BrowserMapping{Browser: "chrome", Profile: "Work", VPN: keep.ID})
	if err := e.svc.DeleteVPN(v.ID); err != nil {
		t.Fatal(err)
	}
	got, err := e.svc.BrowserMappings()
	if err != nil || len(got) != 1 || got[0].VPN != keep.ID {
		t.Fatalf("mappings after delete %+v, %v", got, err)
	}
}

// A TunnelHub import saves the Chrome mappings of the VPNs the user kept,
// matched by the name the file gave them, and drops the rest.
func TestImportedBrowserMappingsSaveForKeptVPNs(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	v := e.save(t, false)
	rep := importer.Report{
		VPNs:     []importer.VPNImport{{ID: v.ID, Name: "Corp"}},
		Browsers: []importer.BrowserMapping{{Profile: "Work", VPN: "Corp"}, {Profile: "Home", VPN: "Skipped"}},
	}
	saved := saveImportedBrowsers(e.svc.st, rep)
	if len(saved) != 1 || saved[0].Profile != "Work" {
		t.Fatalf("saved %+v", saved)
	}
	got, _ := e.svc.BrowserMappings()
	if len(got) != 1 || got[0] != (store.BrowserMapping{Browser: "chrome", Profile: "Work", VPN: v.ID}) {
		t.Fatalf("mappings %+v", got)
	}
}
