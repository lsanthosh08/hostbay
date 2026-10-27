package secrets

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"hostbay/internal/apperr"
	"hostbay/internal/store"
)

type fakeVerifier Result

func (f fakeVerifier) Verify(context.Context, string) Result { return Result(f) }

func TestGateRefusesAnythingButPassed(t *testing.T) {
	ctx := context.Background()
	if err := Gate(ctx, fakeVerifier(Passed), "reveal"); err != nil {
		t.Fatalf("Gate(Passed) = %v", err)
	}
	refused := map[string]Verifier{
		"cancelled":      fakeVerifier(Cancelled),
		"failed":         fakeVerifier(Failed),
		"not configured": fakeVerifier(NotConfigured),
		"zero result":    fakeVerifier(0),
		"no verifier":    nil,
	}
	for name, ver := range refused {
		if err := Gate(ctx, ver, "reveal"); apperr.CodeOf(err) != apperr.VerifyFailed {
			t.Errorf("%s: Gate = %v; want VerifyFailed", name, err)
		}
	}
	var e *apperr.Error
	if err := Gate(ctx, fakeVerifier(NotConfigured), "reveal"); !errors.As(err, &e) || e.Message != notConfiguredMessage {
		t.Fatalf("Gate(NotConfigured) = %v; want message %q", err, notConfiguredMessage)
	}
}

func TestRevealGatedReturnsPlaintextOnlyAfterPassing(t *testing.T) {
	v, st := newTestVault(t, nil)
	sealed, err := v.Seal([]byte("pw"), webAAD)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []Result{Cancelled, Failed, NotConfigured} {
		got, err := New(st, fakeVerifier(r)).RevealGated(context.Background(), sealed, webAAD, "Reveal the password for web")
		if apperr.CodeOf(err) != apperr.VerifyFailed || got != nil {
			t.Errorf("RevealGated after result %d = %q, %v; want nil, VerifyFailed", r, got, err)
		}
	}
	got, err := New(st, fakeVerifier(Passed)).RevealGated(context.Background(), sealed, webAAD, "Reveal the password for web")
	if err != nil || string(got) != "pw" {
		t.Fatalf("RevealGated after Passed = %q, %v; want pw", got, err)
	}
}

func TestPasswordVerifierWithoutPasswordRefusesAndOffersNoSetup(t *testing.T) {
	_, st := newTestVault(t, nil)
	asked := false
	ver := passwordVerifier{st, func(context.Context, string) (string, bool) { asked = true; return "anything", true }}
	if r := ver.Verify(context.Background(), "reveal"); r != NotConfigured {
		t.Fatalf("Verify = %d, want NotConfigured", r)
	}
	got, err := New(st, ver).RevealGated(context.Background(), "enc:v1:AAAA", webAAD, "reveal")
	if apperr.CodeOf(err) != apperr.VerifyFailed || got != nil {
		t.Fatalf("RevealGated = %q, %v; want nil, VerifyFailed", got, err)
	}
	if asked {
		t.Fatal("verification asked for a password when none is set")
	}
	if mp, _ := store.Load[masterPassword](st, masterPasswordFile); mp.Verifier != "" {
		t.Fatal("verification created a master password")
	}
}

func TestPasswordVerifierChecksTheMasterPassword(t *testing.T) {
	v, st := newTestVault(t, nil)
	if err := v.SetMasterPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		typed string
		ok    bool
		want  Result
	}{
		{"correct horse", true, Passed},
		{"wrong horse", true, Failed},
		{"", false, Cancelled},
	}
	for _, c := range cases {
		ver := passwordVerifier{st, func(context.Context, string) (string, bool) { return c.typed, c.ok }}
		if got := ver.Verify(context.Background(), "reveal"); got != c.want {
			t.Errorf("typed %q ok=%v: Verify = %d, want %d", c.typed, c.ok, got, c.want)
		}
	}
}

func TestSetMasterPasswordRefusesShortPasswords(t *testing.T) {
	v, st := newTestVault(t, nil)
	if err := v.SetMasterPassword("1234567"); err == nil {
		t.Fatal("SetMasterPassword accepted a 7-character password")
	}
	if mp, _ := store.Load[masterPassword](st, masterPasswordFile); mp.set() {
		t.Fatal("a refused password was saved")
	}
}

func TestNeedsMasterPasswordOnlyWhereRequiredAndUnset(t *testing.T) {
	v, _ := newTestVault(t, nil)
	if need, err := v.NeedsMasterPassword(); err != nil || need {
		t.Fatalf("NeedsMasterPassword without the Linux rule = %v, %v; want false", need, err)
	}
	v.requirePassword = true
	if need, err := v.NeedsMasterPassword(); err != nil || !need {
		t.Fatalf("NeedsMasterPassword before setting = %v, %v; want true", need, err)
	}
	if err := v.SetMasterPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	if need, err := v.NeedsMasterPassword(); err != nil || need {
		t.Fatalf("NeedsMasterPassword after setting = %v, %v; want false", need, err)
	}
}

func TestUnlockingPromptRevealsInMasterPasswordMode(t *testing.T) {
	keyring.MockInitWithError(errors.New("no Secret Service"))
	st, err := store.Open(filepath.Join(t.TempDir(), "Hostbay"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	v := New(st, nil)
	v.requirePassword = true
	if err := v.SetMasterPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	sealed, err := v.Seal([]byte("pw"), webAAD)
	if err != nil {
		t.Fatal(err)
	}

	// A fresh vault, as after a restart: locked until the right password.
	locked := New(st, nil)
	locked.requirePassword = true
	typed := "wrong horse"
	locked.ver = NewPasswordVerifier(st, locked.UnlockingPrompt(func(context.Context, string) (string, bool) { return typed, true }))
	if got, err := locked.RevealGated(context.Background(), sealed, webAAD, "reveal"); apperr.CodeOf(err) != apperr.VerifyFailed || got != nil {
		t.Fatalf("RevealGated with a wrong password = %q, %v; want nil, VerifyFailed", got, err)
	}
	if _, err := locked.Open(sealed, webAAD); apperr.CodeOf(err) != apperr.SecretNeeded {
		t.Fatalf("Open after a wrong password: %v; want SecretNeeded (still locked)", err)
	}
	typed = "correct horse"
	if got, err := locked.RevealGated(context.Background(), sealed, webAAD, "reveal"); err != nil || string(got) != "pw" {
		t.Fatalf("RevealGated with the right password = %q, %v; want pw", got, err)
	}
}

// After a restart in master-password mode, connecting or saving a password
// unlocks through the prompt; cancelling or a wrong password leaves the
// vault locked, and an unlocked vault is never asked again.
func TestUnlockWithAsksOnlyWhenLocked(t *testing.T) {
	keyring.MockInitWithError(errors.New("no Secret Service"))
	st, err := store.Open(filepath.Join(t.TempDir(), "Hostbay"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	v := New(st, nil)
	v.requirePassword = true
	if err := v.SetMasterPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	sealed, err := v.Seal([]byte("pw"), webAAD)
	if err != nil {
		t.Fatal(err)
	}

	locked := New(st, nil)
	locked.requirePassword = true
	asked, typed, ok := 0, "", false
	ask := func(context.Context, string) (string, bool) { asked++; return typed, ok }
	ctx := context.Background()
	if err := locked.UnlockWith(ctx, ask); apperr.CodeOf(err) != apperr.VerifyFailed {
		t.Fatalf("cancelled UnlockWith = %v; want VerifyFailed", err)
	}
	typed, ok = "wrong horse", true
	if err := locked.UnlockWith(ctx, ask); apperr.CodeOf(err) != apperr.VerifyFailed {
		t.Fatalf("UnlockWith a wrong password = %v; want VerifyFailed", err)
	}
	if _, err := locked.Open(sealed, webAAD); apperr.CodeOf(err) != apperr.SecretNeeded {
		t.Fatalf("Open while locked: %v; want SecretNeeded", err)
	}
	typed = "correct horse"
	if err := locked.UnlockWith(ctx, ask); err != nil {
		t.Fatal(err)
	}
	if got, err := locked.Open(sealed, webAAD); err != nil || string(got) != "pw" {
		t.Fatalf("Open after unlocking = %q, %v", got, err)
	}
	if err := locked.UnlockWith(ctx, ask); err != nil || asked != 3 {
		t.Fatalf("UnlockWith when unlocked = %v after %d prompts; want no fourth prompt", err, asked)
	}

	// A vault whose key comes from the keychain is never locked.
	keyring.MockInit()
	st2, err := store.Open(filepath.Join(t.TempDir(), "Hostbay"))
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	open := New(st2, nil)
	if err := open.UnlockWith(ctx, func(context.Context, string) (string, bool) { t.Fatal("asked"); return "", false }); err != nil {
		t.Fatal(err)
	}
}

// Two calls that find the vault locked share one prompt: the second waits
// for the first prompt's answer instead of asking again, so cancelling a
// second prompt can never refuse a call the first one already unlocked.
func TestConcurrentUnlockWithAsksOnce(t *testing.T) {
	keyring.MockInitWithError(errors.New("no Secret Service"))
	st, err := store.Open(filepath.Join(t.TempDir(), "Hostbay"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	v := New(st, nil)
	v.requirePassword = true
	if err := v.SetMasterPassword("correct horse"); err != nil {
		t.Fatal(err)
	}

	locked := New(st, nil)
	locked.requirePassword = true
	var asked atomic.Int32
	asking, release := make(chan struct{}), make(chan struct{})
	ask := func(context.Context, string) (string, bool) {
		if asked.Add(1) > 1 {
			return "", false // a second prompt, cancelled
		}
		close(asking)
		<-release
		return "correct horse", true
	}
	errs := make(chan error, 2)
	go func() { errs <- locked.UnlockWith(context.Background(), ask) }()
	<-asking
	go func() { errs <- locked.UnlockWith(context.Background(), ask) }()
	time.Sleep(50 * time.Millisecond) // let the second call reach the vault while the first prompt is open
	close(release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Errorf("UnlockWith = %v; want both calls unlocked", err)
		}
	}
	if n := asked.Load(); n != 1 {
		t.Fatalf("asked %d times; want one prompt", n)
	}
}
