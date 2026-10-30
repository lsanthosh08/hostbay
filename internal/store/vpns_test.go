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
		Routes: []string{"10.10.0.0/16"}, DNSServers: []string{"10.10.0.53"}, DNSDomains: []string{"corp.example"},
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
