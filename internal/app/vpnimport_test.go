package app

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"hostbay/internal/secrets"
	"hostbay/internal/store"
)

const ovpnFixture = "../../testdata/importers/vpn/office.ovpn"

// The review flow (R42): a preview carries no secret and saves nothing;
// a skipped draft leaves nothing behind, not even a tombstone, and a kept
// one saves with the user's edits and the file's sealed secrets.
func TestVPNImportReviewSavesOnlyKeptDraftsWithEdits(t *testing.T) {
	s, _, dir := newHostService(t, fakeVerifier(secrets.Passed))
	pre, err := s.PreviewVPNImport(ovpnFixture)
	if err != nil {
		t.Fatal(err)
	}
	if len(pre.Drafts) != 1 {
		t.Fatalf("drafts = %+v", pre.Drafts)
	}
	dr := pre.Drafts[0]
	if !dr.Profile.HasSecret || dr.Profile.PrivateKey != nil || dr.Profile.Password != nil || !dr.AskPassword || !slices.Contains(dr.Ignored, "up /tmp/x.sh") || !slices.Contains(dr.Ignored, "script-security 2") {
		t.Fatalf("draft = %+v", dr)
	}
	if raw, _ := json.Marshal(pre); strings.Contains(string(raw), "FakeOvpnClientKey") {
		t.Fatal("the preview carries the private key")
	}
	d, _ := s.st.Data()
	if len(d.VPNs.Items) != 0 {
		t.Fatal("preview saved a profile")
	}

	rep, err := s.CommitVPNImport(pre.Token, nil)
	if err != nil {
		t.Fatal(err)
	}
	d, _ = s.st.Data()
	if len(rep.VPNs) != 0 || len(d.VPNs.Items) != 0 || len(d.VPNs.Tombstones) != 0 {
		t.Fatalf("skipping saved something: report %+v, %d profiles, %d tombstones", rep.VPNs, len(d.VPNs.Items), len(d.VPNs.Tombstones))
	}
	if _, err := s.CommitVPNImport(pre.Token, nil); err == nil {
		t.Fatal("a finished import's token still works")
	}

	pre, _ = s.PreviewVPNImport(ovpnFixture)
	keep := pre.Drafts[0].Profile
	keep.Name, keep.Tags, keep.Routes = "Office", []string{"atlas"}, []string{"10.1.0.0/16"}
	keep.Gateway = "elsewhere.example" // not editable in review
	keep.RoutingMode, keep.AutoApply = "split", flag(true)
	keep.Domains = []string{"Wiki.Corp.Example"}
	rep, err = s.CommitVPNImport(pre.Token, []VPN{keep})
	if err != nil {
		t.Fatal(err)
	}
	d, _ = s.st.Data()
	if len(rep.VPNs) != 1 || len(d.VPNs.Items) != 1 {
		t.Fatalf("report %+v, profiles %+v", rep.VPNs, d.VPNs.Items)
	}
	p := d.VPNs.Items[0]
	if p.RoutingMode != "split" || p.AutoApply == nil || !*p.AutoApply || !slices.Equal(p.Domains, []string{"wiki.corp.example"}) {
		t.Fatalf("the routing mode chosen in review was lost: %+v", p)
	}
	if p.Name != "Office" || !slices.Equal(p.Tags, []string{"atlas"}) || !slices.Equal(p.Routes, []string{"10.1.0.0/16"}) || p.Gateway != "vpn1.example.test" {
		t.Fatalf("saved = %+v", p)
	}
	raw, err := s.vault.Open(p.Secret, p.SecretAAD())
	var sec store.VPNSecrets
	if err != nil || json.Unmarshal(raw, &sec) != nil || !strings.Contains(sec.PrivateKey, "FakeOvpnClientKey") {
		t.Fatalf("secret = %v %+v", err, sec)
	}
	_ = dir
}

// A kept draft edited into something the profile form refuses is refused
// here too, and the import stays open to fix it.
func TestVPNImportCommitChecksEdits(t *testing.T) {
	s, _, _ := newHostService(t, fakeVerifier(secrets.Passed))
	pre, err := s.PreviewVPNImport(ovpnFixture)
	if err != nil {
		t.Fatal(err)
	}
	bad := pre.Drafts[0].Profile
	bad.Routes = []string{"not a network"}
	if _, err := s.CommitVPNImport(pre.Token, []VPN{bad}); err == nil {
		t.Fatal("a bad route was saved")
	}
	if _, err := s.CommitVPNImport(pre.Token, nil); err != nil {
		t.Fatalf("the import closed on a refused edit: %v", err)
	}
}

// An imported profile may not list a domain a saved profile already
// routes, as the profile form refuses it.
func TestVPNImportKeepsEachDomainInOneProfile(t *testing.T) {
	s, _, _ := newHostService(t, fakeVerifier(secrets.Passed))
	for i, d := range []string{"corp.example", "wiki.corp.example"} {
		pre, err := s.PreviewVPNImport(ovpnFixture)
		if err != nil {
			t.Fatal(err)
		}
		keep := pre.Drafts[0].Profile
		keep.Domains = []string{d}
		_, err = s.CommitVPNImport(pre.Token, []VPN{keep})
		if i == 0 && err != nil {
			t.Fatal(err)
		}
		if i == 1 && (err == nil || !strings.Contains(err.Error(), "already routed")) {
			t.Fatalf("an overlapping domain was imported: %v", err)
		}
	}
}

// A previewed file holds its secrets in memory until it is committed, so a
// closed review forgets them, and one nobody closed expires.
func TestVPNImportIsForgottenWhenCancelledOrExpired(t *testing.T) {
	s, _, _ := newHostService(t, fakeVerifier(secrets.Passed))
	cancelled, err := s.PreviewVPNImport(ovpnFixture)
	if err != nil {
		t.Fatal(err)
	}
	s.CancelVPNImport(cancelled.Token)
	if _, err := s.CommitVPNImport(cancelled.Token, nil); err == nil {
		t.Fatal("a cancelled import's token still works")
	}

	stale, err := s.PreviewVPNImport(ovpnFixture)
	if err != nil {
		t.Fatal(err)
	}
	vpnImports.Lock()
	p := vpnImports.m[stale.Token]
	p.at = p.at.Add(-2 * vpnImportTTL)
	vpnImports.m[stale.Token] = p
	vpnImports.Unlock()
	if _, err := s.CommitVPNImport(stale.Token, nil); err == nil {
		t.Fatal("an expired import's token still works")
	}
}
