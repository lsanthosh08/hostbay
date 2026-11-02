package vpnconf

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"hostbay/internal/importer"
	"hostbay/internal/importer/importertest"
	"hostbay/internal/store"
)

func fixture(name string) string { return filepath.Join(importertest.Fixtures, "vpn", name) }

func parse(t *testing.T, name string) importer.Result {
	t.Helper()
	res, err := Parse(fixture(name), "")
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func pemBody(t *testing.T, name, marker string) string {
	t.Helper()
	raw, err := os.ReadFile(fixture(name))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(l, marker) {
			return l
		}
	}
	t.Fatalf("%s: no line starting %q", name, marker)
	return ""
}

// AE9: script and file-bearing directives never reach the profile, and the
// report lists each one; inline certificates and keys are sealed.
func TestOVPNDropsScriptsAndSealsInlineCertificates(t *testing.T) {
	st, v, dir := importertest.Store(t)
	rep := importertest.Import(t, st, v, parse(t, "office.ovpn"))
	p := importertest.VPN(t, st, "office")
	if p.Type != "openvpn" || p.Tier != store.VPNTierSystem || p.Gateway != "vpn1.example.test" || p.Port != 1195 || p.Auth != "password" || p.MTU != 1400 {
		t.Fatalf("profile = %+v", p)
	}
	if !slices.Equal(p.Routes, []string{"10.1.0.0/16", "192.168.40.0/24"}) || !slices.Equal(p.DNSServers, []string{"10.1.0.53"}) || !slices.Equal(p.DNSDomains, []string{"corp.example.test"}) {
		t.Fatalf("routes %v, dns %v %v", p.Routes, p.DNSServers, p.DNSDomains)
	}
	ign := importertest.Ignored(rep, "office")
	for _, want := range []string{"up /tmp/x.sh", "script-security 2", "plugin /usr/lib/openvpn/plugin-auth-pam.so login", "log /tmp/openvpn.log", "frobnicate yes", "remote vpn2.example.test 1195 udp"} {
		if !slices.Contains(ign, want) {
			t.Errorf("ignored %q missing from %q", want, ign)
		}
	}
	sec := importertest.VPNSecrets(t, v, p)
	key := pemBody(t, "office.ovpn", "FakeOvpnClientKey")
	ca := pemBody(t, "office.ovpn", "FakeOvpnCA")
	if !strings.Contains(sec.PrivateKey, key) || sec.Password != "" {
		t.Fatalf("secrets: key %q, password %q", sec.PrivateKey, sec.Password)
	}
	for _, want := range []string{"<ca>", ca, "<cert>", "<tls-auth>", "key-direction 1", "remote-cert-tls server", "verify-x509-name vpn1.example.test name", "cipher AES-256-GCM", "proto udp"} {
		if !strings.Contains(sec.OpenVPN, want) {
			t.Errorf("kept config lacks %q:\n%s", want, sec.OpenVPN)
		}
	}
	for _, gone := range []string{"up ", "script-security", "plugin", "log ", "frobnicate", "remote ", "route ", "dhcp-option", "auth-user-pass", "<key>"} {
		if strings.Contains(sec.OpenVPN, "\n"+gone) || strings.HasPrefix(sec.OpenVPN, gone) {
			t.Errorf("kept config holds %q:\n%s", gone, sec.OpenVPN)
		}
	}
	if !askPassword(rep, "office") {
		t.Errorf("a password sign-in without its password should ask on first connect: %+v", rep.VPNs)
	}
	importertest.NoSecrets(t, dir, key, pemBody(t, "office.ovpn", "FakeOvpnClientCert"))
	importertest.Reimport(t, st, v, parse(t, "office.ovpn"))
}

func TestOVPNRedirectGatewayIsFullTunnelAndOutsideFilesAreNotRead(t *testing.T) {
	st, v, _ := importertest.Store(t)
	rep := importertest.Import(t, st, v, parse(t, "full.ovpn"))
	p := importertest.VPN(t, st, "full")
	if !slices.Equal(p.Routes, []string{"0.0.0.0/0", "::/0"}) || p.Gateway != "gw.example.test" || p.Port != 443 || p.Auth != "password" {
		t.Fatalf("profile = %+v", p)
	}
	ign := importertest.Ignored(rep, "full")
	if !slices.Contains(ign, "ca /etc/ssl/certs/ca.pem") {
		t.Errorf("a file outside the profile's folder should be ignored: %q", ign)
	}
	if !askPassword(rep, "full") || importertest.VPNSecrets(t, v, p).Password != "" {
		t.Errorf("auth-user-pass names a file Hostbay never reads, so the password is asked for: %+v", rep.VPNs)
	}
}

// A profile with no inline CA cannot connect (Build refuses it), so the
// review says so now; one with its CA does not carry the note.
func TestOVPNWithoutACAWarnsAtImport(t *testing.T) {
	st, v, _ := importertest.Store(t)
	rep := importertest.Import(t, st, v, parse(t, "full.ovpn"))
	warned := func(name string) bool {
		return slices.ContainsFunc(importertest.Ignored(rep, name), func(s string) bool { return strings.HasPrefix(s, "no CA certificate") })
	}
	if !warned("full") {
		t.Errorf("no warning for a profile whose CA file was outside the folder: %q", importertest.Ignored(rep, "full"))
	}
	rep = importertest.Import(t, st, v, parse(t, "office.ovpn"))
	if warned("office") {
		t.Errorf("warned about a profile with its CA: %q", importertest.Ignored(rep, "office"))
	}
}

func askPassword(rep importer.Report, name string) bool {
	for _, p := range rep.VPNs {
		if p.Name == name {
			return p.AskPassword
		}
	}
	return false
}

// A file reference is read only as a bare name beside the configuration:
// one in a subfolder (such as .ssh/id_rsa from a profile saved in the home
// folder) or a symlink out of the folder is ignored and listed.
func TestOVPNReadsOnlyFilesBesideIt(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".ssh"), 0o700)
	os.WriteFile(filepath.Join(dir, ".ssh", "id_rsa"), []byte("Synthetic-Home-Key"), 0o600)
	os.WriteFile(filepath.Join(outside, "other.key"), []byte("Synthetic-Outside-Key"), 0o600)
	if err := os.Symlink(filepath.Join(outside, "other.key"), filepath.Join(dir, "link.key")); err != nil {
		t.Skip("no symlinks here:", err)
	}
	os.WriteFile(filepath.Join(dir, "home.ovpn"), []byte("remote h.example.test\nkey .ssh/id_rsa\ncert link.key\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "noremote.ovpn"), []byte("client\n"), 0o600)
	res, err := Parse(filepath.Join(dir, "home.ovpn"), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.VPNs) != 1 || !slices.Contains(res.VPNs[0].Ignored, "key .ssh/id_rsa") || !slices.Contains(res.VPNs[0].Ignored, "cert link.key") {
		t.Fatalf("VPNs = %+v", res.VPNs)
	}
	if s := res.Bundle.VPNs[0].Secret; strings.Contains(s, "Synthetic-") {
		t.Fatalf("read a file outside the folder: %s", s)
	}
	res, err = Parse(filepath.Join(dir, "noremote.ovpn"), "")
	if err != nil || len(res.VPNs) != 0 || len(res.Skipped) != 1 {
		t.Fatalf("a configuration without a server: %v %+v %+v", err, res.VPNs, res.Skipped)
	}
}
