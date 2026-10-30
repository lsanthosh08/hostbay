package vpnconf

import (
	"testing"

	"hostbay/internal/importer/importertest"
	"hostbay/internal/store"
)

func TestPhonebookEntries(t *testing.T) {
	st, v, _ := importertest.Store(t)
	rep := importertest.Import(t, st, v, parse(t, "rasphone.pbk"))
	for name, gw := range map[string]string{"Office IKEv2": "ike-win.example.test", "Office L2TP": "l2tp.example.test", "Office SSTP": "sstp.example.test"} {
		p := importertest.VPN(t, st, name)
		if p.Type != "ipsec" || p.Tier != store.VPNTierSystem || p.Gateway != gw || p.Auth != "password" {
			t.Errorf("%s = %+v", name, p)
		}
	}
	if importertest.Skipped(rep, "Old PPTP") == "" || len(rep.VPNs) != 3 {
		t.Fatalf("report = %+v", rep)
	}
}
