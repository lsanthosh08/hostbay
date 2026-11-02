package vpnconf

import (
	"testing"

	"hostbay/internal/importer/importertest"
	"hostbay/internal/store"
)

func TestAnyConnectHostEntriesBecomeProfiles(t *testing.T) {
	st, v, _ := importertest.Store(t)
	rep := importertest.Import(t, st, v, parse(t, "anyconnect.xml"))
	if len(rep.VPNs) != 3 {
		t.Fatalf("report VPNs = %+v", rep.VPNs)
	}
	hq, br := importertest.VPN(t, st, "HQ"), importertest.VPN(t, st, "Branch")
	if hq.Type != "anyconnect" || hq.Tier != store.VPNTierApp || hq.Gateway != "vpn-hq.example.test" || hq.Port != 443 {
		t.Fatalf("HQ = %+v", hq)
	}
	if hq.AuthGroup != "engineering" {
		t.Fatalf("HQ group = %q", hq.AuthGroup)
	}
	if pa := importertest.VPN(t, st, "Pathy"); pa.Gateway != "vpn-path.example.test/staff" || pa.Port != 443 {
		t.Fatalf("Pathy = %+v", pa)
	}
	if br.Gateway != "vpn-branch.example.test" || br.Port != 8443 {
		t.Fatalf("Branch = %+v", br)
	}
}
