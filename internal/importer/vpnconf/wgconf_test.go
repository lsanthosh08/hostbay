package vpnconf

import (
	"slices"
	"testing"

	"hostbay/internal/importer/importertest"
	"hostbay/internal/store"
)

func TestWireGuardKeepsAllowedIPsDNSAndSealsKeys(t *testing.T) {
	st, v, dir := importertest.Store(t)
	rep := importertest.Import(t, st, v, parse(t, "office.conf"))
	p := importertest.VPN(t, st, "office")
	if p.Type != "wireguard" || p.Tier != store.VPNTierApp || p.Gateway != "wg.example.test" || p.Port != 51821 || p.MTU != 1380 || p.PeerKey != "K0gfy3B7F4PAqD8MgFTQKgo9UJ36sv2TWb7FgyhkZok=" {
		t.Fatalf("profile = %+v", p)
	}
	if !slices.Equal(p.Address, []string{"10.8.0.2/32", "fd00:8::2/128"}) || !slices.Equal(p.Routes, []string{"10.8.0.0/24", "10.20.0.0/16"}) ||
		!slices.Equal(p.DNSServers, []string{"10.8.0.1"}) || !slices.Equal(p.DNSDomains, []string{"corp.example.test"}) {
		t.Fatalf("profile = %+v", p)
	}
	sec := importertest.VPNSecrets(t, v, p)
	priv, psk := "Yu0KE84zmPO94P2atNAzTri90/1crrULk7gsPmsgXUw=", "jtO7iCcxqtHmb9/nInarS1LD6RIJnVJIjd3oxLYjJio="
	if sec.PrivateKey != priv || sec.PresharedKey != psk {
		t.Fatalf("secrets = %+v", sec)
	}
	if ign := importertest.Ignored(rep, "office"); !slices.Contains(ign, "PostUp = iptables -A FORWARD -i %i -j ACCEPT") {
		t.Errorf("ignored = %q", ign)
	}
	importertest.NoSecrets(t, dir, priv, psk)
}
