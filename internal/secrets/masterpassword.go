package secrets

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"

	"hostbay/internal/apperr"
	"hostbay/internal/store"
)

const masterPasswordFile = "masterpassword.json"

// MinMasterPasswordLen is the shortest master password SetMasterPassword
// accepts. Argon2id slows guessing but cannot save a very short password.
const MinMasterPasswordLen = 8

// verifierAAD binds the verifier so it can never pass as a secret field.
var verifierAAD = []byte("hostbay master password verifier")

// masterPassword is the stored Hostbay master password record. It holds no
// secret: only the Argon2id salt and parameters, and a value sealed under the
// derived key that tells a wrong password apart without touching any secret.
type masterPassword struct {
	Salt     []byte `json:"salt"`
	Time     uint32 `json:"time"`
	MemoryKB uint32 `json:"memoryKiB"`
	Threads  uint8  `json:"threads"`
	Verifier string `json:"verifier"`
	// UnlocksKey is true when no Secret Service answered at setup, so the
	// master key is derived from this password instead of kept in a keychain.
	UnlocksKey bool `json:"unlocksKey"`
}

func (mp masterPassword) set() bool { return mp.Verifier != "" }

func (mp masterPassword) derive(password string) []byte {
	return argon2.IDKey([]byte(password), mp.Salt, mp.Time, mp.MemoryKB, mp.Threads, 32)
}

// check returns the derived key when password is right.
func (mp masterPassword) check(password string) ([]byte, bool) {
	key := mp.derive(password)
	_, err := open(key, mp.Verifier, verifierAAD)
	return key, err == nil
}

// SetMasterPassword creates the Hostbay master password. It refuses to
// replace an existing one, so someone at an unlocked session cannot swap it
// for one they know. On Linux without a Secret Service the password also
// becomes the source of the master key.
func (v *Vault) SetMasterPassword(password string) error {
	if utf8.RuneCountInString(password) < MinMasterPasswordLen {
		return fmt.Errorf("the master password must be at least %d characters", MinMasterPasswordLen)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	mp, err := store.Load[masterPassword](v.st, masterPasswordFile)
	if err != nil {
		return err
	}
	if mp.set() {
		return errors.New("a Hostbay master password is already set")
	}
	// RFC 9106's second recommended Argon2id setting.
	mp = masterPassword{Salt: make([]byte, 16), Time: 3, MemoryKB: 64 * 1024, Threads: 4}
	if _, err := rand.Read(mp.Salt); err != nil {
		return err
	}
	key := mp.derive(password)
	if v.requirePassword && v.key == nil {
		if _, err := keychainKey(); err != nil {
			mp.UnlocksKey = true
		}
	}
	if mp.Verifier, err = seal(key, nil, verifierAAD); err != nil {
		return err
	}
	if err := store.Save(v.st, masterPasswordFile, mp); err != nil {
		return err
	}
	if mp.UnlocksKey {
		v.key = key
	}
	return nil
}

// Unlock checks password against the stored master password and, in
// master-password mode, unlocks the master key. A wrong password changes
// nothing on disk.
func (v *Vault) Unlock(password string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	mp, err := store.Load[masterPassword](v.st, masterPasswordFile)
	if err != nil {
		return err
	}
	if !mp.set() {
		return apperr.New(apperr.VerifyFailed, "No Hostbay master password is set.").With("result", "notConfigured")
	}
	key, ok := mp.check(password)
	if !ok {
		return apperr.New(apperr.VerifyFailed, "Wrong Hostbay master password.").With("result", "failed")
	}
	if mp.UnlocksKey {
		v.key = key
	}
	return nil
}

// UnlockWith unlocks a vault whose master key comes from the master
// password, asking for it through ask; Seal and Open cannot ask, so the
// connect and save paths call this when they find the vault locked after a
// restart. A vault that is already unlocked, or keeps its key in the OS
// keychain, is not asked. Cancelling or a wrong password leaves it locked.
// Calls that arrive while a prompt is open wait for its answer rather than
// raising a second prompt.
func (v *Vault) UnlockWith(ctx context.Context, ask PasswordPrompt) error {
	v.mu.Lock()
	if v.key != nil {
		v.mu.Unlock()
		return nil
	}
	f := v.unlocking
	if f != nil {
		v.mu.Unlock()
		select {
		case <-f.done:
			return f.err
		case <-ctx.Done():
			return notEntered()
		}
	}
	f = &unlockFlight{done: make(chan struct{})}
	v.unlocking = f
	v.mu.Unlock()

	f.err = v.askUnlock(ctx, ask)
	v.mu.Lock()
	v.unlocking = nil
	v.mu.Unlock()
	close(f.done)
	return f.err
}

// unlockFlight is the one master password prompt UnlockWith has open.
type unlockFlight struct {
	done chan struct{}
	err  error // set before done closes
}

func (v *Vault) askUnlock(ctx context.Context, ask PasswordPrompt) error {
	mp, err := store.Load[masterPassword](v.st, masterPasswordFile)
	if err != nil || !mp.UnlocksKey {
		return err
	}
	typed, ok := ask(ctx, "unlock your saved passwords")
	if !ok {
		return notEntered()
	}
	return v.Unlock(typed)
}

func notEntered() error {
	return apperr.New(apperr.VerifyFailed, "Hostbay stays locked: the master password was not entered.").With("result", "cancelled")
}

// NeedsMasterPassword reports whether this computer verifies the user with a
// Hostbay master password and none is set yet. Only Linux does; macOS and
// Windows verify through the OS and never need one.
func (v *Vault) NeedsMasterPassword() (bool, error) {
	if !v.requirePassword {
		return false, nil
	}
	mp, err := store.Load[masterPassword](v.st, masterPasswordFile)
	return !mp.set(), err
}

// UnlockingPrompt wraps ask so a master password typed at a verification
// prompt also unlocks the master key in master-password mode; otherwise a
// verified reveal would still find the vault locked after a restart.
func (v *Vault) UnlockingPrompt(ask PasswordPrompt) PasswordPrompt {
	return func(ctx context.Context, reason string) (string, bool) {
		typed, ok := ask(ctx, reason)
		if ok {
			_ = v.Unlock(typed) // a wrong password is refused by the verifier's own check
		}
		return typed, ok
	}
}

// NewPasswordVerifier returns a verifier that asks for the Hostbay master
// password through ask. Linux uses it as its OS verifier.
func NewPasswordVerifier(st *store.Store, ask PasswordPrompt) Verifier {
	return passwordVerifier{st, ask}
}

// passwordVerifier verifies the user by asking for the Hostbay master
// password. It never offers to create one (KTD7).
type passwordVerifier struct {
	st  *store.Store
	ask PasswordPrompt
}

func (p passwordVerifier) Verify(ctx context.Context, reason string) Result {
	mp, err := store.Load[masterPassword](p.st, masterPasswordFile)
	if err != nil {
		return Failed
	}
	if !mp.set() {
		return NotConfigured
	}
	if p.ask == nil {
		return NotConfigured
	}
	typed, ok := p.ask(ctx, reason)
	if !ok {
		return Cancelled
	}
	if _, ok := mp.check(typed); !ok {
		return Failed
	}
	return Passed
}
