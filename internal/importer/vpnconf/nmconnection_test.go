package vpnconf

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"hostbay/internal/importer/importertest"
	"hostbay/internal/store"
)

// The SSL VPN type never shows its vendor's name, in anything an import
// reports or saves.
func TestNMSSLVPNNeverNamesTheVendor(t *testing.T) {
	st, v, _ := importertest.Store(t)
	res := parse(t, "branch.nmconnection")
	rep := importertest.Import(t, st, v, res)
	p := importertest.VPN(t, st, "Branch office")
	if p.Type != "sslvpn" || p.Tier != store.VPNTierApp || p.Gateway != "ssl.example.test" || p.Port != 10443 || p.User != "bob" || p.Auth != "password" {
		t.Fatalf("profile = %+v", p)
	}
	if !slices.Equal(p.Routes, []string{"10.30.0.0/16"}) || !slices.Equal(p.DNSServers, []string{"10.30.0.53"}) || !slices.Equal(p.DNSDomains, []string{"branch.example.test"}) {
		t.Fatalf("routes %v, dns %v %v", p.Routes, p.DNSServers, p.DNSDomains)
	}
	if all := strings.ToLower(fmt.Sprintf("%+v %+v %+v", rep, res, p)); strings.Contains(all, "forti") {
		t.Fatalf("the vendor's name leaked: %s", all)
	}
}

func TestNMWireGuardAndVPNC(t *testing.T) {
	st, v, dir := importertest.Store(t)
	importertest.Import(t, st, v, parse(t, "lab.nmconnection"))
	wg := importertest.VPN(t, st, "Lab tunnel")
	if wg.Type != "wireguard" || wg.Gateway != "nmwg.example.test" || wg.Port != 51820 || !slices.Equal(wg.Routes, []string{"10.40.0.0/16", "10.41.0.0/16"}) ||
		!slices.Equal(wg.Address, []string{"10.40.0.2/32"}) || !slices.Equal(wg.DNSServers, []string{"10.40.0.1"}) || wg.PeerKey == "" {
		t.Fatalf("wireguard = %+v", wg)
	}
	if importertest.VPNSecrets(t, v, wg).PrivateKey == "" {
		t.Fatal("private key not kept")
	}
	importertest.Import(t, st, v, parse(t, "legacy.nmconnection"))
	ipsec := importertest.VPN(t, st, "Legacy IPsec")
	if ipsec.Type != "ipsec" || ipsec.Tier != store.VPNTierSystem || ipsec.Gateway != "vpnc.example.test" || ipsec.User != "carol" {
		t.Fatalf("vpnc = %+v", ipsec)
	}
	if s := importertest.VPNSecrets(t, v, ipsec); s.Password != "Nm-Pass-4!" || s.PresharedKey != "Nm-Group-3!" {
		t.Fatalf("vpnc secrets = %+v", s)
	}
	importertest.NoSecrets(t, dir, "Nm-Pass-4!", "Nm-Group-3!")
}
