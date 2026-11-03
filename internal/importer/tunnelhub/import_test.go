package tunnelhub

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"hostbay/internal/importer"
	"hostbay/internal/importer/importertest"
	"hostbay/internal/store"
)

func fixture() string { return filepath.Join(importertest.Fixtures, "tunnelhub", "vpn-config.json") }

func TestVPNsRoutesAndSkippedBrowserEntries(t *testing.T) {
	st, v, _ := importertest.Store(t)
	res, err := Parse(fixture(), "")
	if err != nil {
		t.Fatal(err)
	}
	rep := importertest.Import(t, st, v, res)
	if len(rep.VPNs) != 3 {
		t.Fatalf("report VPNs = %+v", rep.VPNs)
	}
	routes := 0
	for name, want := range map[string]struct {
		typ    string
		mtu    int
		routes []string
	}{
		"Branch SSL VPN": {"sslvpn", 1280, []string{"10.212.0.0/16", "10.213.0.0/16"}},
		"WireGuard Lab":  {"wireguard", 1380, []string{"10.60.0.0/16", "10.61.0.0/16"}},
		"Harmony SASE":   {"perimeter81", 1280, []string{"10.0.0.0/12", "172.20.0.0/16", "172.21.4.0/24"}},
	} {
		p := importertest.VPN(t, st, name)
		if p.Tier != store.VPNTierExternal || p.Type != want.typ || p.MTU != want.mtu || p.RoutingMode != "" || p.Client != name || !slices.Equal(p.Routes, want.routes) {
			t.Errorf("%s = %+v", name, p)
		}
		routes += len(p.Routes)
	}
	if routes != 7 {
		t.Errorf("%d routes, want 7", routes)
	}
	if !slices.ContainsFunc(rep.Skipped, func(s importer.Skipped) bool {
		return s.File == "chrome Work" && strings.Contains(s.Reason, "list the sites on Harmony SASE")
	}) {
		t.Errorf("skipped = %+v", rep.Skipped)
	}
	if !importertest.Noted(rep, "WireGuard Lab", "detection") {
		t.Errorf("notes = %v", rep.Notes)
	}
	res, _ = Parse(fixture(), "")
	importertest.Reimport(t, st, v, res)
}
