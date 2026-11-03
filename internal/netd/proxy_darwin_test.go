package netd

import (
	"slices"
	"strings"
	"testing"
)

// fakeNetworksetup answers like networksetup and records each call's argv.
func fakeNetworksetup(calls *[]string, answers map[string]string) func(args ...string) (string, error) {
	return func(args ...string) (string, error) {
		*calls = append(*calls, strings.Join(args, " | "))
		return answers[strings.Join(args, " ")], nil
	}
}

func TestDarwinProxiesListsOnlyEnabledServices(t *testing.T) {
	var calls []string
	p := darwinProxies{run: fakeNetworksetup(&calls, map[string]string{
		"-listallnetworkservices": "An asterisk (*) denotes that a network service is disabled.\nWi-Fi\n*Thunderbolt Bridge\nUSB 10/100/1000 LAN\n",
	})}
	got, err := p.Services()
	if err != nil || !slices.Equal(got, []string{"Wi-Fi", "USB 10/100/1000 LAN"}) {
		t.Fatalf("Services = %q, %v", got, err)
	}
}

func TestDarwinProxiesReadsAndSetsTheAutomaticProxy(t *testing.T) {
	var calls []string
	p := darwinProxies{run: fakeNetworksetup(&calls, map[string]string{
		"-getautoproxyurl Wi-Fi":    "URL: http://proxy.corp.example/proxy.pac\nEnabled: Yes\n",
		"-getautoproxyurl Ethernet": "URL: (null)\nEnabled: No\n",
	})}
	if st, _ := p.Get("Wi-Fi"); st != (ProxyState{URL: "http://proxy.corp.example/proxy.pac", On: true}) {
		t.Errorf("Wi-Fi = %+v", st)
	}
	if st, _ := p.Get("Ethernet"); st != (ProxyState{}) {
		t.Errorf("Ethernet = %+v", st)
	}
	calls = nil
	p.Set("Wi-Fi", ProxyState{URL: pacV1, On: true})
	p.Set("Ethernet", ProxyState{})
	want := []string{"-setautoproxyurl | Wi-Fi | " + pacV1, "-setautoproxystate | Wi-Fi | on", "-setautoproxystate | Ethernet | off"}
	if !slices.Equal(calls, want) {
		t.Errorf("calls = %q", calls)
	}
}
