package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"hostbay/internal/apperr"
	"hostbay/internal/secrets"
	"hostbay/internal/store"
)

// lateVerifier lets a test build the verifier after the store it reads.
type lateVerifier struct{ secrets.Verifier }

// newPromptedHostService returns a host service whose verification asks for
// the master password ("test master password") through a prompt, and the
// channel the prompt's events arrive on.
func newPromptedHostService(t *testing.T) (*HostService, *MasterPasswordService, *MasterPasswordPrompt, chan MasterPasswordRequest) {
	t.Helper()
	lv := &lateVerifier{}
	s, _, _ := newHostService(t, lv)
	p := NewMasterPasswordPrompt()
	reqs := make(chan MasterPasswordRequest, 1)
	p.emit = func(name string, data any) {
		if name == MasterPasswordEvent {
			reqs <- data.(MasterPasswordRequest)
		}
	}
	lv.Verifier = secrets.NewPasswordVerifier(s.st, s.vault.UnlockingPrompt(p.Ask))
	return s, NewMasterPasswordService(s.vault, p), p, reqs
}

func saveHostWithPassword(t *testing.T, s *HostService, pw string) Host {
	t.Helper()
	h, err := s.SaveHost(Host{Alias: "web", Hostname: "10.0.0.9", Port: 22, User: "root", Password: &pw})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestMasterPasswordIsSetOnceAndNeverReplaced(t *testing.T) {
	keyring.MockInit()
	st, err := store.Open(filepath.Join(t.TempDir(), "Hostbay"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := NewMasterPasswordService(secrets.New(st, nil), NewMasterPasswordPrompt())
	if err := svc.SetMasterPassword("short"); err == nil {
		t.Fatal("SetMasterPassword accepted a 5-character password")
	}
	if err := svc.SetMasterPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetMasterPassword("battery staple"); err == nil {
		t.Fatal("SetMasterPassword replaced an existing master password")
	}
	if need, err := svc.NeedsMasterPassword(); err != nil || need {
		t.Fatalf("NeedsMasterPassword after setting = %v, %v; want false", need, err)
	}
	// The original password still verifies.
	if err := secrets.New(st, nil).Unlock("correct horse"); err != nil {
		t.Fatalf("Unlock with the first password: %v", err)
	}
}

func TestRevealPassesWithTheMasterPasswordFromThePrompt(t *testing.T) {
	s, svc, _, reqs := newPromptedHostService(t)
	h := saveHostWithPassword(t, s, "Svc-Pass-7!")
	go func() {
		r := <-reqs
		if r.Reason == "" {
			t.Error("prompt carried no reason")
		}
		if err := svc.AnswerMasterPassword(r.RequestID, "test master password"); err != nil {
			t.Error(err)
		}
	}()
	got, err := s.RevealPassword(context.Background(), h.ID)
	if err != nil || got != "Svc-Pass-7!" {
		t.Fatalf("RevealPassword = %q, %v; want the saved password", got, err)
	}
}

func TestRevealFailsClosedWithoutTheRightMasterPassword(t *testing.T) {
	cases := map[string]struct {
		answer string // "" cancels
		skip   bool   // never answer
		ctx    func() context.Context
	}{
		"wrong password": {answer: "wrong master password"},
		"cancelled":      {answer: ""},
		"timed out":      {skip: true},
		"call cancelled": {skip: true, ctx: func() context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s, svc, p, reqs := newPromptedHostService(t)
			p.ttl = 20 * time.Millisecond
			h := saveHostWithPassword(t, s, "Svc-Pass-7!")
			done := make(chan string, 1)
			go func() {
				r := <-reqs
				if !c.skip {
					if err := svc.AnswerMasterPassword(r.RequestID, c.answer); err != nil {
						t.Error(err)
					}
				}
				done <- r.RequestID
			}()
			ctx := context.Background()
			if c.ctx != nil {
				ctx = c.ctx()
			}
			if got, err := s.RevealPassword(ctx, h.ID); apperr.CodeOf(err) != apperr.VerifyFailed || got != "" {
				t.Fatalf("RevealPassword = %q, %v; want empty, VerifyFailed", got, err)
			}
			// A prompt that has ended cannot be answered after the fact.
			if err := svc.AnswerMasterPassword(<-done, "test master password"); err == nil {
				t.Fatal("an ended prompt accepted an answer")
			}
		})
	}
}

func TestPromptNeverSetsUpAMasterPassword(t *testing.T) {
	keyring.MockInit()
	st, err := store.Open(filepath.Join(t.TempDir(), "Hostbay"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := NewMasterPasswordPrompt()
	asked := false
	p.emit = func(string, any) { asked = true }
	vault := secrets.New(st, nil)
	ver := secrets.NewPasswordVerifier(st, vault.UnlockingPrompt(p.Ask))
	if err := secrets.Gate(context.Background(), ver, "export saved hosts"); apperr.CodeOf(err) != apperr.VerifyFailed {
		t.Fatalf("Gate without a master password = %v; want VerifyFailed", err)
	}
	if asked {
		t.Fatal("verification prompted for a password when none is set")
	}
	if err := vault.Unlock("anything at all"); apperr.CodeOf(err) != apperr.VerifyFailed {
		t.Fatalf("Unlock after the refused gate = %v; want VerifyFailed (still no master password)", err)
	}
}

// A vault that keeps its key in the OS keychain is never locked, so
// UnlockVault answers at once without asking.
func TestUnlockVaultAsksNothingWhenNotLocked(t *testing.T) {
	_, mp, _, reqs := newPromptedHostService(t)
	if err := mp.UnlockVault(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-reqs:
		t.Fatalf("UnlockVault prompted: %+v", r)
	default:
	}
}

// After a restart without a Secret Service the vault is locked: UnlockVault
// raises the prompt, a wrong answer leaves it locked, and the right one
// unlocks it so a later call asks nothing.
func TestUnlockVaultUnlocksALockedVaultThroughThePrompt(t *testing.T) {
	keyring.MockInit()
	st, err := store.Open(filepath.Join(t.TempDir(), "Hostbay"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := secrets.New(st, nil).SetMasterPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	// Record the password as the key's source, as setup does on Linux when
	// no Secret Service answers; the mocked keychain always answers.
	mp, err := store.Load[map[string]any](st, "masterpassword.json")
	if err != nil {
		t.Fatal(err)
	}
	mp["unlocksKey"] = true
	if err := store.Save(st, "masterpassword.json", mp); err != nil {
		t.Fatal(err)
	}

	p := NewMasterPasswordPrompt()
	reqs := make(chan MasterPasswordRequest, 1)
	p.emit = func(name string, data any) {
		if name == MasterPasswordEvent {
			reqs <- data.(MasterPasswordRequest)
		}
	}
	svc := NewMasterPasswordService(secrets.New(st, nil), p) // a fresh vault, as after a restart
	unlock := func(answer string) error {
		go func() {
			r := <-reqs
			if err := svc.AnswerMasterPassword(r.RequestID, answer); err != nil {
				t.Error(err)
			}
		}()
		return svc.UnlockVault(context.Background())
	}
	if err := unlock("wrong horse"); apperr.CodeOf(err) != apperr.VerifyFailed {
		t.Fatalf("UnlockVault with a wrong password = %v; want VerifyFailed", err)
	}
	if err := unlock("correct horse"); err != nil {
		t.Fatalf("UnlockVault with the right password = %v", err)
	}
	if err := svc.UnlockVault(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-reqs:
		t.Fatalf("UnlockVault prompted again once unlocked: %+v", r)
	default:
	}
}
