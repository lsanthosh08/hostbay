package app

import (
	"context"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"hostbay/internal/vpn/vpntest"
)

type pacEnv struct {
	vpn *vpnEnv
	nd  *NetdService
	svc *PACService
	f   *fakeHelper
}

// newPACEnv is the office profile, app-scoped through the fake VPN helper,
// with the network helper connected.
func newPACEnv(t *testing.T) *pacEnv {
	t.Helper()
	pacDebounce, netdRetryMin = 10*time.Millisecond, 20*time.Millisecond
	e := &pacEnv{vpn: newVPNEnv(t, vpntest.OK), f: newFakeHelper()}
	e.nd = NewNetdService()
	e.nd.dial = func(context.Context) (netdClient, error) {
		e.f.reopen()
		return e.f, nil
	}
	e.svc = NewPACService(e.vpn.svc.st, e.vpn.svc, e.nd)
	// macOS applies whatever the helper set.
	e.svc.effective = func() (string, bool, error) {
		e.f.mu.Lock()
		defer e.f.mu.Unlock()
		return e.f.proxy, e.f.proxy != "", nil
	}
	e.svc.ServiceStartup(context.Background(), application.ServiceOptions{})
	e.nd.ServiceStartup(context.Background(), application.ServiceOptions{})
	t.Cleanup(func() {
		e.svc.ServiceShutdown()
		e.nd.ServiceShutdown()
	})
	return e
}

// proxyIs waits until the helper holds a PAC URL (want true) or none.
func (e *pacEnv) proxyIs(t *testing.T, set bool) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		e.f.mu.Lock()
		url := e.f.proxy
		e.f.mu.Unlock()
		if (url != "") == set {
			return url
		}
		if time.Now().After(deadline) {
			t.Fatalf("proxy set = %q, want set %v", url, set)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (e *pacEnv) proxyCalls() []string {
	e.f.mu.Lock()
	defer e.f.mu.Unlock()
	return slices.Clone(e.f.proxys)
}

func get(t *testing.T, url string, header map[string]string) (int, string, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	for k, v := range header {
		if k == "Host" {
			req.Host = v
		} else {
			req.Header.Set(k, v)
		}
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header.Get("Content-Type"), string(b)
}

// Connecting a profile with sites serves a PAC with them and sets the system
// proxy to it once; disconnecting the last one clears it.
func TestPACFollowsAnAppScopedVPN(t *testing.T) {
	e := newPACEnv(t)
	v := e.vpn.save(t, false)
	v.Domains = []string{"corp.example"}
	if _, err := e.vpn.svc.SaveVPN(v); err != nil {
		t.Fatal(err)
	}
	if _, err := e.vpn.svc.ConnectVPN(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	url := e.proxyIs(t, true)
	code, ctype, body := get(t, url, nil)
	if code != 200 || ctype != "application/x-ns-proxy-autoconfig" || !strings.Contains(body, `"corp.example"`) ||
		!strings.Contains(body, "SOCKS5 127.0.0.1:"+strconv.Itoa(vpntest.BrowserPort)) || !strings.Contains(body, `isInNet(h, "10.9.0.0"`) {
		t.Fatalf("GET %s = %d %q\n%s", url, code, ctype, body)
	}
	time.Sleep(100 * time.Millisecond)
	if calls := e.proxyCalls(); len(calls) != 1 {
		t.Fatalf("proxy calls %q", calls)
	}
	if st := e.svc.State(); st.State != PACSet || st.Domains[v.ID] != 1 || st.Networks[v.ID] == 0 {
		t.Fatalf("state %+v", st)
	}
	if err := e.vpn.svc.DisconnectVPN(v.ID); err != nil {
		t.Fatal(err)
	}
	e.proxyIs(t, false)
	if st := e.svc.State(); st.State != PACOff {
		t.Fatalf("state %+v", st)
	}
}

// A site added to a connected profile reaches the PAC at a new address.
func TestPACMovesToANewAddressWhenItChanges(t *testing.T) {
	e := newPACEnv(t)
	v := e.vpn.save(t, false)
	e.vpn.svc.ConnectVPN(context.Background(), v.ID)
	first := e.proxyIs(t, true)
	v.Domains = []string{"wiki.corp.example"}
	if _, err := e.vpn.svc.SaveVPN(v); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for e.proxyIs(t, true) == first && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	second := e.proxyIs(t, true)
	if second == first {
		t.Fatal("the PAC address did not change")
	}
	if _, _, body := get(t, second, nil); !strings.Contains(body, "wiki.corp.example") {
		t.Fatalf("new PAC lacks the site:\n%s", body)
	}
}

// A page in a browser cannot read the PAC: a foreign Host or any Origin
// gets 403, and a wrong path 404.
func TestPACRefusesPagesAndStrangers(t *testing.T) {
	e := newPACEnv(t)
	v := e.vpn.save(t, false)
	e.vpn.svc.ConnectVPN(context.Background(), v.ID)
	url := e.proxyIs(t, true)
	if code, _, _ := get(t, url, map[string]string{"Host": "evil.example"}); code != 403 {
		t.Errorf("foreign host: %d", code)
	}
	if code, _, _ := get(t, url, map[string]string{"Origin": "https://evil.example"}); code != 403 {
		t.Errorf("origin: %d", code)
	}
	base := url[:strings.Index(url, "/hostbay-pac/")]
	if code, _, _ := get(t, base+"/hostbay-pac/wrongtokenwrongtoken/1.pac", nil); code != 404 {
		t.Errorf("wrong token: %d", code)
	}
}

// The helper's sweep clears the setting when its connection drops, so a
// reconnect sets the same PAC again.
func TestPACIsSetAgainAfterTheHelperReconnects(t *testing.T) {
	e := newPACEnv(t)
	v := e.vpn.save(t, false)
	e.vpn.svc.ConnectVPN(context.Background(), v.ID)
	url := e.proxyIs(t, true)
	e.f.Close()
	if got := e.proxyIs(t, true); got != url {
		t.Fatalf("after reconnect %q, want %q", got, url)
	}
}

// Quitting clears the setting before the helper connection closes.
func TestPACClearsOnShutdown(t *testing.T) {
	e := newPACEnv(t)
	v := e.vpn.save(t, false)
	e.vpn.svc.ConnectVPN(context.Background(), v.ID)
	e.proxyIs(t, true)
	e.svc.ServiceShutdown()
	if calls := e.proxyCalls(); calls[len(calls)-1] != "clear" {
		t.Fatalf("calls %q", calls)
	}
}

// A reconnect gets a new listener port, which the PAC follows.
func TestPACFollowsANewListenerPort(t *testing.T) {
	e := newPACEnv(t)
	port := 41000
	e.svc.port = func(context.Context, string) (int, error) { port++; return port, nil }
	v := e.vpn.save(t, false)
	e.vpn.svc.ConnectVPN(context.Background(), v.ID)
	first := e.proxyIs(t, true)
	e.vpn.svc.DisconnectVPN(v.ID)
	e.proxyIs(t, false)
	e.vpn.svc.ConnectVPN(context.Background(), v.ID)
	second := e.proxyIs(t, true)
	if _, _, body := get(t, second, nil); first == second || !strings.Contains(body, "127.0.0.1:41002") {
		t.Fatalf("%q then %q:\n%s", first, second, body)
	}
}

// Another proxy configuration that wins over Hostbay's is reported, without
// naming either address.
func TestPACReportsAnOverridingProxy(t *testing.T) {
	e := newPACEnv(t)
	v := e.vpn.save(t, false)
	e.vpn.svc.ConnectVPN(context.Background(), v.ID)
	url := e.proxyIs(t, true)
	e.svc.effective = func() (string, bool, error) { return url, true, nil }
	if st := e.svc.State(); st.State != PACSet {
		t.Fatalf("state %+v", st)
	}
	e.svc.effective = func() (string, bool, error) { return "http://proxy.corp.example/proxy.pac", true, nil }
	if st := e.svc.State(); st.State != PACOverridden {
		t.Fatalf("state %+v", st)
	}
}
