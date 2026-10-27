package app

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"hostbay/internal/apperr"
	"hostbay/internal/rdp"
	"hostbay/internal/secrets"
	"hostbay/internal/tlstrust"
)

// fakeRdpHelper builds internal/rdp's stand-in helper.
func fakeRdpHelper(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fakehelper")
	if runtime.GOOS == "windows" {
		path += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", path, "../rdp/testdata/fakehelper").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return path
}

func newRdpService(t *testing.T) (*RdpService, string) {
	t.Helper()
	hosts, creds, dir := newHostService(t, fakeVerifier(secrets.Passed))
	kind, pw := "rdp", "Rdp-Secret-42!"
	h, err := hosts.SaveHost(Host{Alias: "win", Hostname: "win.lan", User: "me", Kind: &kind, Password: &pw})
	if err != nil {
		t.Fatal(err)
	}
	s := NewRdpService(creds, nil, tlstrust.New(dir))
	s.emit = func(string, any) {}
	return s, h.ID
}

func TestRdpConnectAsksForTheCertificateThenGivesEachTabItsOwnSession(t *testing.T) {
	t.Setenv("FAKE_RDP_MODE", "")
	s, hostID := newRdpService(t)
	helper := fakeRdpHelper(t)
	s.helper = func() (string, error) { return helper, nil }

	// An RDP-only host needs no SSH lease: the nil ConnService is never used.
	_, err := s.RdpConnect(context.Background(), hostID, 1280, 800)
	ae, ok := err.(*apperr.Error)
	if !ok || ae.Code != apperr.HostKeyUnknown || ae.Details["kind"] != "rdpCert" {
		t.Fatalf("err = %v", err)
	}
	if err := s.RdpTrustCert(ae.Details["requestId"], true); err != nil {
		t.Fatal(err)
	}

	a, err := s.RdpConnect(context.Background(), hostID, 1280, 800)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.RdpConnect(context.Background(), hostID, 1280, 800)
	if err != nil {
		t.Fatal(err)
	}
	if a == b || len(a) != 32 {
		t.Fatalf("rdpIds %q and %q", a, b)
	}
	if err := s.RdpInput(a, "key", 0, 0, 0, true, 0x1e, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.RdpInput(a, "move", 9999, 0, 0, false, 0, 0); err == nil {
		t.Fatal("a point off the desktop was forwarded")
	}
	s.RdpClose(a)
	s.RdpClose(b)
	if err := s.RdpInput(a, "key", 0, 0, 0, true, 0x1e, 0); err == nil {
		t.Fatal("a closed session took input")
	}
}

func TestRdpConnectWithoutTheHelperIsHelperMissing(t *testing.T) {
	s, hostID := newRdpService(t)
	s.helper = func() (string, error) { return filepath.Join(t.TempDir(), rdp.HelperName()), nil }
	if _, err := s.RdpConnect(context.Background(), hostID, 1280, 800); apperr.CodeOf(err) != apperr.HelperMissing {
		t.Fatalf("err = %v", err)
	}
}

func TestRdpConnectRefusesSillySizes(t *testing.T) {
	s, hostID := newRdpService(t)
	if _, err := s.RdpConnect(context.Background(), hostID, 0, 800); err == nil {
		t.Fatal("width 0 was accepted")
	}
}
