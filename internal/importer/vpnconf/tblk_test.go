package vpnconf

import (
	"slices"
	"testing"

	"hostbay/internal/importer/importertest"
)

func TestTunnelblickBundleReadsItsOwnFilesOnly(t *testing.T) {
	st, v, dir := importertest.Store(t)
	rep := importertest.Import(t, st, v, parse(t, "Office.tblk"))
	p := importertest.VPN(t, st, "Office")
	if p.Type != "openvpn" || p.Gateway != "tb.example.test" || !slices.Equal(p.Routes, []string{"10.50.0.0/16"}) || p.Auth != "certificate" {
		t.Fatalf("profile = %+v", p)
	}
	key := pemBody(t, "Office.tblk/Contents/Resources/client.key", "FakeTblkClientKey")
	s := importertest.VPNSecrets(t, v, p)
	if !slices.Contains(importertest.Ignored(rep, "Office"), "tls-crypt ../../../../outside.key") {
		t.Errorf("ignored = %q", importertest.Ignored(rep, "Office"))
	}
	if s.PrivateKey == "" || !contains(s.OpenVPN, pemBody(t, "Office.tblk/Contents/Resources/ca.crt", "FakeOvpnCA")) {
		t.Fatalf("secrets = %+v", s)
	}
	importertest.NoSecrets(t, dir, key)
}

func TestViscosityBundle(t *testing.T) {
	st, v, _ := importertest.Store(t)
	importertest.Import(t, st, v, parse(t, "Home.visc"))
	p := importertest.VPN(t, st, "Home Lab")
	if p.Gateway != "visc.example.test" || !slices.Equal(p.Routes, []string{"0.0.0.0/0", "::/0"}) || p.Auth != "password" {
		t.Fatalf("profile = %+v", p)
	}
	if importertest.VPNSecrets(t, v, p).PrivateKey == "" {
		t.Fatal("key not kept")
	}
}
