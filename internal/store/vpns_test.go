package store

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// A profile round-trips with its tags, routes and sealed secrets; deleting
// it leaves a tombstone, and a host that named it still names it, so it
// fails to connect rather than connect around the VPN.
func TestVPNsPersistAndLeaveTombstones(t *testing.T) {
	s := openTestStore(t)
	secret, _ := json.Marshal(VPNSecrets{PrivateKey: "test-private-key", PresharedKey: "test-psk"})
	want := VPN{
		Name: "office", Type: "wireguard", Tier: VPNTierApp, Gateway: "vpn.example", Port: 51820, User: "u",
		Routes: []string{"10.10.0.0/16"}, DNSServers: []string{"10.10.0.53"}, DNSDomains: []string{"corp.example"}, Domains: []string{"wiki.corp.example"},
		Tags: []string{"Project-A", "prod"}, AutoReconnect: true, Secret: string(secret),
	}
	var id, hostID string
	d := edit(t, s, func(d *Data) error {
		id = d.PutVPN(want).ID
		hostID = d.PutHost(Host{Alias: "web", Hostname: "10.10.0.5", Port: 22, VPNID: &id}).ID
		return nil
	})
	if _, err := os.Stat(filepath.Join(s.dir, "vpns.json")); err != nil {
		t.Fatalf("vpns.json not written: %v", err)
	}
	got := d.VPN(id)
	if got == nil || got.Revision != 1 || got.DeviceID != s.DeviceID() {
		t.Fatalf("saved VPN = %+v", got)
	}
	want.Meta = got.Meta
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("saved VPN = %+v, want %+v", *got, want)
	}
	var opened VPNSecrets
	if err := json.Unmarshal([]byte(got.Secret), &opened); err != nil || opened.PrivateKey != "test-private-key" || opened.PresharedKey != "test-psk" {
		t.Fatalf("secrets = %+v, %v", opened, err)
	}
	if got.TunnelMTU() != 1280 {
		t.Fatalf("default MTU = %d, want 1280", got.TunnelMTU())
	}

	d = edit(t, s, func(d *Data) error { return d.PurgeVPN(id) })
	if d.VPN(id) != nil || len(d.VPNs.Tombstones) != 1 || d.VPNs.Tombstones[0].Revision != 2 {
		t.Fatalf("after purge: %+v", d.VPNs)
	}
	if h := d.Host(hostID); !h.UsesVPN() || *h.VPNID != id || d.VPN(*h.VPNID) != nil {
		t.Fatalf("host after its VPN was deleted = %+v; want it still naming the missing profile", h)
	}
}

// A profile's secrets are bound to where and how they are used: changing
// its gateway, type, user or tier makes the old sealed value unopenable.
func TestVPNSecretAADFollowsGatewayTypeUserAndTier(t *testing.T) {
	v := VPN{Meta: Meta{ID: "v1"}, Type: "wireguard", Tier: VPNTierApp, Gateway: "a.example", User: "u"}
	for name, change := range map[string]func(*VPN){
		"gateway": func(v *VPN) { v.Gateway = "b.example" },
		"type":    func(v *VPN) { v.Type = "openvpn" },
		"user":    func(v *VPN) { v.User = "w" },
		"tier":    func(v *VPN) { v.Tier = VPNTierSystem },
		"id":      func(v *VPN) { v.ID = "v2" },
	} {
		w := v
		change(&w)
		if bytes.Equal(v.SecretAAD(), w.SecretAAD()) {
			t.Errorf("changing the %s kept the binding", name)
		}
	}
	p := Proxy{Meta: Meta{ID: "v1"}, Type: "wireguard", Host: "a.example", User: "u"}
	if bytes.Equal(v.SecretAAD(), p.SecretAAD()) {
		t.Fatal("a proxy password could be swapped in for a VPN secret")
	}
}

// CheckVPN refuses what could break or inject into an engine
// configuration, for profiles typed in and read from files alike.
func TestCheckVPNRefusesUnusableFields(t *testing.T) {
	ok := VPN{Type: "wireguard", Tier: VPNTierApp, Gateway: "vpn.example", Port: 51820, Address: []string{"10.0.0.2/32"},
		DNSServers: []string{"10.0.0.53"}, DNSDomains: []string{"corp.example"}}
	if err := CheckVPN(ok); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(*VPN){
		"no type":          func(v *VPN) { v.Type = "" },
		"unknown tier":     func(v *VPN) { v.Tier = "cloud" },
		"gateway option":   func(v *VPN) { v.Gateway = "-oProxyCommand" },
		"gateway space":    func(v *VPN) { v.Gateway = "vpn example" },
		"peer key newline": func(v *VPN) { v.PeerKey = "key\nAllowedIPs" },
		"port":             func(v *VPN) { v.Port = 65536 },
		"low MTU":          func(v *VPN) { v.MTU = 575 },
		"high MTU":         func(v *VPN) { v.MTU = 65536 },
		"user newline":     func(v *VPN) { v.User = "a\nb" },
		"address":          func(v *VPN) { v.Address = []string{"10.0.0.2"} },
		"dns server":       func(v *VPN) { v.DNSServers = []string{"dns.example"} },
		"dns domain":       func(v *VPN) { v.DNSDomains = []string{"a,b"} },
		"domain wildcard":  func(v *VPN) { v.Domains = []string{"*.corp.example"} },
		"domain url":       func(v *VPN) { v.Domains = []string{"http://x"} },
		"domain space":     func(v *VPN) { v.Domains = []string{"a b"} },
		"domain quote":     func(v *VPN) { v.Domains = []string{`"x"`} },
		"domain control":   func(v *VPN) { v.Domains = []string{"a\x00b"} },
		"domain upper":     func(v *VPN) { v.Domains = []string{"Corp.example"} },
		"domain empty":     func(v *VPN) { v.Domains = []string{""} },
	} {
		v := ok
		edit(&v)
		if err := CheckVPN(v); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A domain is saved lowercased without its trailing dot; anything that is
// not a plain DNS name is refused.
func TestCleanDomain(t *testing.T) {
	for in, want := range map[string]string{"Corp.Example.": "corp.example", " wiki-2.corp.example ": "wiki-2.corp.example", "intranet": "intranet"} {
		if got, ok := CleanDomain(in); !ok || got != want {
			t.Errorf("CleanDomain(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"*.corp.example", "http://x", "a b", `"x"`, "a\x00b", "", ".", "-a.example", "a..example", "a_b.example"} {
		if got, ok := CleanDomain(in); ok {
			t.Errorf("CleanDomain(%q) = %q, accepted", in, got)
		}
	}
}

// Two domains overlap when one is the other or inside it.
func TestDomainsOverlap(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"corp.example", "corp.example", true},
		{"wiki.corp.example", "corp.example", true},
		{"corp.example", "wiki.corp.example", true},
		{"notcorp.example", "corp.example", false},
		{"corp.example", "example.corp", false},
	} {
		if got := DomainsOverlap(c.a, c.b); got != c.want {
			t.Errorf("DomainsOverlap(%q, %q) = %v", c.a, c.b, got)
		}
	}
}
