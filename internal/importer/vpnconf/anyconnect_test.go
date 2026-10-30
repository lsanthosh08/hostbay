package vpnconf

import (
	"testing"

	"hostbay/internal/importer/importertest"
	"hostbay/internal/store"
)

func TestAnyConnectHostEntriesBecomeProfiles(t *testing.T) {
	st, v, _ := importertest.Store(t)
	rep := importertest.Import(t, st, v, parse(t, "anyconnect.xml"))
	if len(rep.VPNs) != 2 {
		t.Fatalf("report VPNs = %+v", rep.VPNs)
	}
	hq, br := importertest.VPN(t, st, "HQ"), importertest.VPN(t, st, "Branch")
	if hq.Type != "anyconnect" || hq.Tier != store.VPNTierApp || hq.Gateway != "vpn-hq.example.test" || hq.Port != 443 {
		t.Fatalf("HQ = %+v", hq)
	}
	if br.Gateway != "vpn-branch.example.test" || br.Port != 8443 {
		t.Fatalf("Branch = %+v", br)
	}
}
