package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"hostbay/internal/apperr"
	"hostbay/internal/secrets"
	"hostbay/internal/store"
	hbsync "hostbay/internal/sync"
)

const (
	syncPass   = "sync passphrase for tests 7"
	syncSecret = "Sync-Target-Token-93!"
)

func newSyncService(t *testing.T, ver secrets.Verifier) (*SyncService, *HostService, string) {
	t.Helper()
	hosts, _, dir := newHostService(t, ver)
	s := NewSyncService(hosts.st, hosts.vault, ver, nil, hbsync.Deps{})
	return s, hosts, dir
}

func folderTarget(t *testing.T) hbsync.Target {
	return hbsync.Target{Kind: "folder", Path: filepath.Join(t.TempDir(), "sync")}
}

func TestEnablingSyncWithVerificationCancelledChangesNothing(t *testing.T) {
	s, _, dir := newSyncService(t, fakeVerifier(secrets.Cancelled))
	target := folderTarget(t)
	err := s.Configure(context.Background(), SyncTargetInput{Target: target, Passphrase: syncPass})
	if apperr.CodeOf(err) != apperr.VerifyFailed {
		t.Fatalf("Configure = %v, want VerifyFailed", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sync.json")); !os.IsNotExist(err) {
		t.Fatal("sync settings were written")
	}
	if _, err := os.Stat(target.Path); !os.IsNotExist(err) {
		t.Fatal("something was uploaded")
	}
	if _, err := s.SyncNow(context.Background(), ""); err == nil {
		t.Fatal("sync ran while off")
	}
	if _, err := s.ChangePassphrase(context.Background(), syncPass+"x"); apperr.CodeOf(err) != apperr.VerifyFailed {
		t.Fatalf("ChangePassphrase = %v, want VerifyFailed", err)
	}
}

func TestSyncSettingsKeepNoReadableSecret(t *testing.T) {
	s, hosts, dir := newSyncService(t, fakeVerifier(secrets.Passed))
	pw := "Host-Pass-55"
	if _, err := hosts.SaveHost(Host{Alias: "web", Hostname: "web.example", Port: 22, User: "root", Password: &pw}); err != nil {
		t.Fatal(err)
	}
	ppw := "Proxy-Pass-66"
	if _, err := hosts.SaveProxy(Proxy{Name: "office", Type: "socks5", Host: "proxy.example", Port: 1080, User: "pu", Password: &ppw}); err != nil {
		t.Fatal(err)
	}
	target := folderTarget(t)
	if err := s.Configure(context.Background(), SyncTargetInput{Target: target, Secret: syncSecret, Passphrase: syncPass}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SyncNow(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	st, err := s.Status()
	if err != nil || !st.Enabled || st.LastSync == "" || st.LastError != "" {
		t.Fatalf("Status = %+v, %v", st, err)
	}
	for _, root := range []string{dir, target.Path} {
		filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			raw, _ := os.ReadFile(p)
			for _, secret := range []string{syncPass, syncSecret, pw, ppw} {
				if bytes.Contains(raw, []byte(secret)) {
					t.Errorf("%s holds %q in readable form", p, secret)
				}
			}
			return nil
		})
	}
}

func TestJoiningWithTheWrongPassphraseSavesNothing(t *testing.T) {
	a, _, _ := newSyncService(t, fakeVerifier(secrets.Passed))
	target := folderTarget(t)
	if err := a.Configure(context.Background(), SyncTargetInput{Target: target, Passphrase: syncPass}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SyncNow(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	b, _, dir := newSyncService(t, fakeVerifier(secrets.Passed))
	if err := b.Configure(context.Background(), SyncTargetInput{Target: target, Passphrase: "another passphrase 12"}); err == nil {
		t.Fatal("joined with the wrong passphrase")
	}
	if _, err := os.Stat(filepath.Join(dir, "sync.json")); !os.IsNotExist(err) {
		t.Fatal("settings were saved after a wrong passphrase")
	}
	if err := b.Configure(context.Background(), SyncTargetInput{Target: target, Passphrase: syncPass}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.SyncNow(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
}

// After a passphrase change only the changing computer's file is under the
// new key. A computer rejoining with the new passphrase must get in whichever
// device file sorts first, then sync past files still under the old key;
// the old passphrase must no longer get in.
func TestRejoiningAfterAPassphraseChangeOnAFolderTarget(t *testing.T) {
	const newPass = "a brand new sync passphrase 42"
	for _, changerSortsFirst := range []bool{false, true} {
		ctx := context.Background()
		target := folderTarget(t)
		var devs []*SyncService
		for range 3 {
			s, _, _ := newSyncService(t, fakeVerifier(secrets.Passed))
			if err := s.Configure(ctx, SyncTargetInput{Target: target, Passphrase: syncPass}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.SyncNow(ctx, ""); err != nil {
				t.Fatal(err)
			}
			devs = append(devs, s)
		}
		// b rejoins; a changes the passphrase; c keeps its old-key file.
		b, a, c := devs[0], devs[1], devs[2]
		if (a.st.DeviceID() < c.st.DeviceID()) != changerSortsFirst {
			a, c = c, a
		}
		if _, err := a.ChangePassphrase(ctx, newPass); err != nil {
			t.Fatal(err)
		}
		if err := b.Configure(ctx, SyncTargetInput{Target: target, Passphrase: syncPass}); err == nil {
			t.Fatalf("changerSortsFirst=%v: rejoined with the old passphrase", changerSortsFirst)
		}
		if err := b.Configure(ctx, SyncTargetInput{Target: target, Passphrase: newPass}); err != nil {
			t.Fatalf("changerSortsFirst=%v: rejoin with the new passphrase: %v", changerSortsFirst, err)
		}
		if _, err := b.SyncNow(ctx, ""); err != nil {
			t.Fatalf("changerSortsFirst=%v: sync after rejoining: %v", changerSortsFirst, err)
		}
	}
}

// A refused sign-in pauses background sync after one attempt; Sync now
// tries again and, once the sign-in works, clears the pause.
func TestRefusedSignInStopsBackgroundRetries(t *testing.T) {
	s, _, _ := newSyncService(t, fakeVerifier(secrets.Passed))
	ctx := context.Background()
	if err := s.Configure(ctx, SyncTargetInput{Target: folderTarget(t), Passphrase: syncPass}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SyncNow(ctx, ""); err != nil {
		t.Fatal(err)
	}
	real, dials := s.connect, 0
	s.connect = func(ctx context.Context, t hbsync.Target, secret string, deps hbsync.Deps) (hbsync.Backend, error) {
		dials++
		return nil, fmt.Errorf("sftp: %w", errors.New("ssh: unable to authenticate, attempted methods [none password], no supported methods remain"))
	}
	s.tick(ctx)
	s.tick(ctx)
	st, err := s.Status()
	if err != nil {
		t.Fatal(err)
	}
	if dials != 1 || st.Paused != hbsync.CodeAuth {
		t.Fatalf("dials = %d, paused %q; want 1 dial and %q", dials, st.Paused, hbsync.CodeAuth)
	}
	s.connect = real
	if _, err := s.SyncNow(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Status(); st.Paused != "" || st.LastError != "" {
		t.Fatalf("after Sync now: paused %q, error %q", st.Paused, st.LastError)
	}
}

// refusingVersion is a target that, like Git, WebDAV and S3, opens without
// a network call and refuses the sign-in on its first request.
type refusingVersion struct {
	hbsync.Backend
	calls *int
}

func (r refusingVersion) Version(context.Context) (string, error) {
	*r.calls++
	return "", fmt.Errorf("WebDAV check: %w: 401 Unauthorized", hbsync.ErrAuth)
}

func TestRefusedVersionCheckStopsBackgroundRetries(t *testing.T) {
	s, _, _ := newSyncService(t, fakeVerifier(secrets.Passed))
	ctx := context.Background()
	if err := s.Configure(ctx, SyncTargetInput{Target: folderTarget(t), Passphrase: syncPass}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SyncNow(ctx, ""); err != nil {
		t.Fatal(err)
	}
	real, calls := s.connect, 0
	s.connect = func(ctx context.Context, t hbsync.Target, secret string, deps hbsync.Deps) (hbsync.Backend, error) {
		b, err := real(ctx, t, secret, deps)
		return refusingVersion{b, &calls}, err
	}
	s.tick(ctx)
	s.tick(ctx)
	if st, _ := s.Status(); calls != 1 || st.Paused != hbsync.CodeAuth {
		t.Fatalf("version checks = %d, paused %q; want 1 and %q", calls, st.Paused, hbsync.CodeAuth)
	}
}

// A dial that fails for a reason other than a pause, such as a dropped
// network, must not clear a pause already recorded.
func TestNetworkErrorKeepsAPause(t *testing.T) {
	s, _, _ := newSyncService(t, fakeVerifier(secrets.Passed))
	ctx := context.Background()
	if err := s.Configure(ctx, SyncTargetInput{Target: folderTarget(t), Passphrase: syncPass}); err != nil {
		t.Fatal(err)
	}
	s.connect = func(context.Context, hbsync.Target, string, hbsync.Deps) (hbsync.Backend, error) {
		return nil, errors.New("ssh: unable to authenticate, attempted methods [password]")
	}
	_, _ = s.SyncNow(ctx, "")
	s.connect = func(context.Context, hbsync.Target, string, hbsync.Deps) (hbsync.Backend, error) {
		return nil, errors.New("dial tcp: i/o timeout")
	}
	_, _ = s.SyncNow(ctx, "")
	if st, _ := s.Status(); st.Paused != hbsync.CodeAuth {
		t.Fatalf("paused %q after a network error; want %q kept", st.Paused, hbsync.CodeAuth)
	}
}

// Fixing the credentials through Configure clears a refused sign-in's
// pause, so background sync resumes without a separate Sync now.
func TestConfigureClearsAnAuthPause(t *testing.T) {
	s, _, _ := newSyncService(t, fakeVerifier(secrets.Passed))
	ctx := context.Background()
	target := folderTarget(t)
	if err := s.Configure(ctx, SyncTargetInput{Target: target, Passphrase: syncPass}); err != nil {
		t.Fatal(err)
	}
	real := s.connect
	s.connect = func(context.Context, hbsync.Target, string, hbsync.Deps) (hbsync.Backend, error) {
		return nil, fmt.Errorf("sftp: %w", errors.New("ssh: unable to authenticate, attempted methods [password]"))
	}
	_, _ = s.SyncNow(ctx, "")
	if st, _ := s.Status(); st.Paused != hbsync.CodeAuth {
		t.Fatalf("paused %q after a refused sign-in; want %q", st.Paused, hbsync.CodeAuth)
	}
	s.connect = real
	if err := s.Configure(ctx, SyncTargetInput{Target: target, Secret: "fixed password", Passphrase: syncPass}); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Status(); st.Paused != "" || st.LastError != "" {
		t.Fatalf("after Configure: paused %q, error %q; want both cleared", st.Paused, st.LastError)
	}
}

// Accepting a synced hold that lets more happen without asking must ask as
// the same change made here does: a weaker AI mode needs Go's dialog and OS
// verification, a post-login rule that types a saved secret needs OS
// verification. Either failing leaves the hold; refusing needs neither.
func TestAcceptingAWideningHoldAsksAsALocalChangeWould(t *testing.T) {
	s, hosts, _ := newSyncService(t, fakeVerifier(secrets.Passed))
	ctx := context.Background()
	h, err := hosts.SaveHost(Host{Alias: "web", Hostname: "10.0.0.9", Port: 22, User: "root"})
	if err != nil {
		t.Fatal(err)
	}
	var ai store.SettingsGroup
	rules := map[string]store.PostLoginRule{}
	if err := s.st.Edit(func(d *store.Data) error {
		ai = d.PutSettings(store.SettingsGroup{Meta: store.Meta{ID: store.GroupAI}, ExecutionMode: store.AIConfirmAll})
		for _, kind := range []string{store.ReplySecret, store.ReplyPassword} {
			r := d.PutPostLogin(store.PostLoginRule{Meta: store.Meta{ID: store.NewID()}, HostID: h.ID,
				Steps: []store.PostLoginStep{{Expect: "Token:", Kind: kind}}})
			d.PostLoginRule(r.ID).Held = true
			rules[kind] = *d.PostLoginRule(r.ID)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st, err := s.load()
	if err != nil {
		t.Fatal(err)
	}
	st.Holds = []hbsync.Hold{
		{ID: ai.ID, Kind: "aimode", Meta: ai.Meta, Mode: store.AIDangerousOnly},
		{ID: rules[store.ReplySecret].ID, Kind: "postlogin", Meta: rules[store.ReplySecret].Meta},
		{ID: rules[store.ReplyPassword].ID, Kind: "postlogin", Meta: rules[store.ReplyPassword].Meta},
	}
	held := func(id string) bool {
		return slices.ContainsFunc(st.Holds, func(h hbsync.Hold) bool { return h.ID == id })
	}
	mode := func() string {
		d, err := s.st.Data()
		if err != nil {
			t.Fatal(err)
		}
		return d.AIMode()
	}
	dialogs, yes := 0, false
	s.confirm = func(context.Context, string, string) bool { dialogs++; return yes }

	if err := s.ConfirmHold(ctx, ai.ID, true); err == nil || mode() != store.AIConfirmAll || !held(ai.ID) {
		t.Fatalf("dialog said no: %v, mode %q, held %v", err, mode(), held(ai.ID))
	}
	yes, s.ver = true, fakeVerifier(secrets.Cancelled)
	if err := s.ConfirmHold(ctx, ai.ID, true); err == nil || mode() != store.AIConfirmAll || !held(ai.ID) {
		t.Fatalf("verification cancelled: %v, mode %q, held %v", err, mode(), held(ai.ID))
	}
	secret := rules[store.ReplySecret].ID
	if err := s.ConfirmHold(ctx, secret, true); err == nil || !held(secret) {
		t.Fatalf("secret rule with verification cancelled: %v, held %v", err, held(secret))
	}
	n := dialogs
	if err := s.ConfirmHold(ctx, rules[store.ReplyPassword].ID, false); err != nil || dialogs != n {
		t.Fatalf("refusing asked or failed: %v", err)
	}
	s.ver = fakeVerifier(secrets.Passed)
	if err := s.ConfirmHold(ctx, ai.ID, true); err != nil || mode() != store.AIDangerousOnly || held(ai.ID) {
		t.Fatalf("both passed: %v, mode %q, held %v", err, mode(), held(ai.ID))
	}
	if err := s.ConfirmHold(ctx, secret, true); err != nil || held(secret) {
		t.Fatalf("secret rule verified: %v, held %v", err, held(secret))
	}
	d, _ := s.st.Data()
	if r := d.PostLoginRule(secret); r == nil || r.Held {
		t.Fatalf("accepted rule = %+v, want it running", r)
	}
}
