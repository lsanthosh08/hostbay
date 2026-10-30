package vpnconf

import (
	"slices"
	"testing"

	"hostbay/internal/importer/importertest"
	"hostbay/internal/store"
)

func TestMobileconfigVPNPayloads(t *testing.T) {
	st, v, dir := importertest.Store(t)
	rep := importertest.Import(t, st, v, parse(t, "profile.mobileconfig"))
	ike := importertest.VPN(t, st, "Corp IKEv2")
	if ike.Type != "ipsec" || ike.Tier != store.VPNTierSystem || ike.Gateway != "ike.example.test" || ike.User != "alice" || ike.Auth != "password" {
		t.Fatalf("IKEv2 = %+v", ike)
	}
	if s := importertest.VPNSecrets(t, v, ike); s.Password != "Mc-Pass-2!" || s.PresharedKey != "Mc-Psk-1!" {
		t.Fatalf("IKEv2 secrets = %+v", s)
	}
	wg := importertest.VPN(t, st, "Lab WireGuard")
	if wg.Type != "wireguard" || wg.Tier != store.VPNTierApp || wg.Gateway != "mcwg.example.test" || !slices.Equal(wg.Routes, []string{"10.9.0.0/16"}) || wg.PeerKey == "" {
		t.Fatalf("WireGuard = %+v", wg)
	}
	ext := importertest.VPN(t, st, "Vendor App")
	if ext.Tier != store.VPNTierExternal || ext.Type != "other" || ext.Client != "com.example.vendorvpn" {
		t.Fatalf("external = %+v", ext)
	}
	if len(rep.VPNs) != 3 {
		t.Fatalf("report VPNs = %+v", rep.VPNs)
	}
	importertest.NoSecrets(t, dir, "Mc-Pass-2!", "Mc-Psk-1!")
}
