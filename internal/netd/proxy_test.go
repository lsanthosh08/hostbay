package netd

import (
	"context"
	"path/filepath"
	"testing"
)

const (
	pacV1     = "http://127.0.0.1:49152/hostbay-pac/abcdefghijklmnop/1.pac"
	pacV2     = "http://127.0.0.1:49152/hostbay-pac/abcdefghijklmnop/2.pac"
	corporate = "http://proxy.corp.example/proxy.pac"
)

func proxySession(t *testing.T, p *fakeProxies) *session {
	t.Helper()
	s := connect(t, newFakeTable(defaultRoute))
	s.server.Proxies = p
	return s
}

// Each enabled service is pointed at Hostbay's PAC, a newer version moves it
// along, and the connection's end puts back what each service had: a
// corporate PAC comes back, and a service that had none ends switched off.
func TestProxySetThenDisconnectRestoresEachService(t *testing.T) {
	ctx := context.Background()
	p := newFakeProxies(map[string]ProxyState{"Wi-Fi": {URL: corporate, On: true}, "Ethernet": {}})
	s := proxySession(t, p)
	if err := s.client.SetProxy(ctx, pacV1); err != nil {
		t.Fatal(err)
	}
	if err := s.client.SetProxy(ctx, pacV2); err != nil {
		t.Fatal(err)
	}
	for _, svc := range []string{"Wi-Fi", "Ethernet"} {
		if got := p.of(svc); got != (ProxyState{URL: pacV2, On: true}) {
			t.Fatalf("%s = %+v", svc, got)
		}
	}
	s.disconnect(t)
	if got := p.of("Wi-Fi"); got != (ProxyState{URL: corporate, On: true}) {
		t.Errorf("Wi-Fi after the sweep = %+v", got)
	}
	if got := p.of("Ethernet"); got.On {
		t.Errorf("Ethernet after the sweep = %+v", got)
	}
}

// Clear restores at once, and a second clear has nothing left to undo.
func TestProxyClearRestoresThePriorSetting(t *testing.T) {
	ctx := context.Background()
	p := newFakeProxies(map[string]ProxyState{"Wi-Fi": {URL: corporate, On: true}})
	s := proxySession(t, p)
	s.client.SetProxy(ctx, pacV1)
	if err := s.client.ClearProxy(ctx); err != nil {
		t.Fatal(err)
	}
	if got := p.of("Wi-Fi"); got != (ProxyState{URL: corporate, On: true}) {
		t.Fatalf("Wi-Fi = %+v", got)
	}
	p.Set("Wi-Fi", ProxyState{URL: pacV1, On: true}) // not Hostbay's doing any more
	if err := s.client.ClearProxy(ctx); err != nil {
		t.Fatal(err)
	}
	if got := p.of("Wi-Fi"); got.URL != pacV1 {
		t.Fatalf("a second clear changed Wi-Fi to %+v", got)
	}
}

// A service someone else repointed after Hostbay set it is theirs.
func TestProxySweepLeavesAServiceSomeoneElseChanged(t *testing.T) {
	p := newFakeProxies(map[string]ProxyState{"Wi-Fi": {}})
	s := proxySession(t, p)
	s.client.SetProxy(context.Background(), pacV1)
	p.Set("Wi-Fi", ProxyState{URL: corporate, On: true})
	s.disconnect(t)
	if got := p.of("Wi-Fi"); got != (ProxyState{URL: corporate, On: true}) {
		t.Errorf("Wi-Fi = %+v", got)
	}
}

// A helper that died with an older version of Hostbay's PAC still set
// restores the prior setting when it starts again.
func TestProxyRestartSweepRestoresAnOlderHostbayURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.json")
	j, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.recordProxy("Wi-Fi", ProxyState{URL: corporate, On: true}); err != nil {
		t.Fatal(err)
	}
	p := newFakeProxies(map[string]ProxyState{"Wi-Fi": {URL: pacV1, On: true}})
	j, _ = OpenJournal(path)
	if err := j.RestoreProxies(p); err != nil {
		t.Fatal(err)
	}
	if got := p.of("Wi-Fi"); got != (ProxyState{URL: corporate, On: true}) {
		t.Errorf("Wi-Fi = %+v", got)
	}
	if j, _ = OpenJournal(path); len(j.entries) != 0 {
		t.Errorf("journal keeps %+v", j.entries)
	}
}

// Route sweeps leave proxy entries for the proxy restore, and the other way round.
func TestRouteSweepKeepsProxyEntries(t *testing.T) {
	j, _ := OpenJournal(filepath.Join(t.TempDir(), "journal.json"))
	j.recordProxy("Wi-Fi", ProxyState{})
	r := sr("10.1.0.0/16", "utun3")
	j.record(Added, r)
	table := newFakeTable(defaultRoute, r)
	if err := Sweep(table, j); err != nil {
		t.Fatal(err)
	}
	if len(j.entries) != 1 || j.entries[0].Proxy == nil {
		t.Fatalf("entries after the route sweep = %+v", j.entries)
	}
}

func TestProxyUnsupportedWithoutProxies(t *testing.T) {
	enc, in := rawSession(t, newServer(t, newFakeTable()))
	enc.Encode(Msg{T: SetProxy, V: Version, ID: 1, URL: pacV1})
	if m := readMsg(t, in); m.Code != CodeUnsupported {
		t.Errorf("reply = %+v, want %s", m, CodeUnsupported)
	}
}
