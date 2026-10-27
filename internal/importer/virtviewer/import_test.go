package virtviewer

import (
	"path/filepath"
	"strings"
	"testing"

	"hostbay/internal/importer/importertest"
)

func fixture(name string) string { return filepath.Join(importertest.Fixtures, "virtviewer", name) }

func TestImportPrefersTheTLSPortAndSealsTheTicket(t *testing.T) {
	st, v, dir := importertest.Store(t)
	res, err := Parse(fixture("spice.vv"), "")
	if err != nil {
		t.Fatal(err)
	}
	rep := importertest.Import(t, st, v, res)
	h := importertest.Host(t, st, "vm-guest-1")
	if rep.Hosts != 1 || h.KindName() != "spice" || h.Port != 5901 || h.SpiceSecurity() != "tls" || importertest.Password(t, v, h) != "Vv-Ticket-1!" {
		t.Fatalf("report %+v, host %+v", rep, h)
	}
	if !importertest.Noted(rep, "vm-guest-1", "CA certificate was not imported") {
		t.Errorf("notes = %v", rep.Notes)
	}
	importertest.NoSecrets(t, dir, "Vv-Ticket-1!")
	res, _ = Parse(fixture("spice.vv"), "")
	importertest.Reimport(t, st, v, res)
}

func TestPlainSpiceProxyAndVNC(t *testing.T) {
	st, v, _ := importertest.Store(t)
	for _, f := range []string{"plain-spice.vv", "vnc.vv"} {
		res, err := Parse(fixture(f), "")
		if err != nil {
			t.Fatal(err)
		}
		rep := importertest.Import(t, st, v, res)
		switch f {
		case "plain-spice.vv":
			h := importertest.Host(t, st, "plain-spice")
			if h.SpiceSecurity() != "none" || h.Port != 5903 || h.ProxyID == nil {
				t.Errorf("plain spice = %+v", h)
			}
			if !importertest.Noted(rep, "plain-spice", "unencrypted") || !importertest.Noted(rep, "plain-spice", "proxy") {
				t.Errorf("notes = %v", rep.Notes)
			}
		case "vnc.vv":
			h := importertest.Host(t, st, "vm-guest-2")
			if h.KindName() != "vnc" || h.Port != 5902 || h.VNCSecurity() != "vencrypt" || importertest.Password(t, v, h) != "Vv-Vnc-3!" {
				t.Errorf("vnc = %+v", h)
			}
		}
	}
}

func TestSkipsWhatCannotImport(t *testing.T) {
	for f, want := range map[string]string{"ovirt.vv": "oVirt", "evil.vv": "not a valid address"} {
		res, err := Parse(fixture(f), "")
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Bundle.Hosts) != 0 || len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0].Reason, want) {
			t.Errorf("%s: %+v", f, res)
		}
	}
}
