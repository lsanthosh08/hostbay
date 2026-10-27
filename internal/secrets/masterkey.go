package secrets

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime"
	"sync"

	"github.com/zalando/go-keyring"

	"hostbay/internal/apperr"
	"hostbay/internal/store"
)

const (
	keyringService = "Hostbay"
	keyringAccount = "master-key"
)

// Vault seals and opens secret fields with the master key. Plaintext only
// leaves it through Open, for backend paths such as connect, and through
// RevealGated after OS verification passes.
type Vault struct {
	st  *store.Store
	ver Verifier
	// requirePassword is true on Linux, where KTD7 verifies with the Hostbay
	// master password, so one must exist before any secret is saved.
	requirePassword bool

	mu        sync.Mutex
	key       []byte        // nil until loaded or unlocked
	unlocking *unlockFlight // the open UnlockWith prompt, if any
}

// New returns a vault over st that verifies the user with ver.
func New(st *store.Store, ver Verifier) *Vault {
	return &Vault{st: st, ver: ver, requirePassword: runtime.GOOS == "linux"}
}

// Seal encrypts plaintext bound to aad. If the master key cannot be had it
// returns a SecretNeeded error and no value, so the caller records the secret
// as needing entry rather than as empty.
func (v *Vault) Seal(plaintext, aad []byte) (string, error) {
	if v.requirePassword {
		mp, err := store.Load[masterPassword](v.st, masterPasswordFile)
		if err != nil {
			return "", err
		}
		if !mp.set() {
			return "", apperr.New(apperr.VerifyFailed, "Set a Hostbay master password before saving passwords.").With("result", "notConfigured")
		}
	}
	key, err := v.masterKey()
	if err != nil {
		return "", err
	}
	return seal(key, plaintext, aad)
}

// Open decrypts a sealed value bound to aad. A missing, tampered or rebound
// value returns a SecretNeeded error, never an empty secret.
func (v *Vault) Open(sealed string, aad []byte) ([]byte, error) {
	key, err := v.masterKey()
	if err != nil {
		return nil, err
	}
	plain, err := open(key, sealed, aad)
	if err != nil {
		return nil, secretNeeded("decrypt", "The saved secret is missing or cannot be decrypted; enter it again.")
	}
	return plain, nil
}

// RevealGated returns the plaintext only after OS verification passes.
func (v *Vault) RevealGated(ctx context.Context, sealed string, aad []byte, reason string) ([]byte, error) {
	if err := Gate(ctx, v.ver, reason); err != nil {
		return nil, err
	}
	return v.Open(sealed, aad)
}

// Gate runs OS verification with the vault's verifier, for actions that
// need it but reveal no secret.
func (v *Vault) Gate(ctx context.Context, reason string) error { return Gate(ctx, v.ver, reason) }

// masterKey returns the cached key, or loads it from the keychain, creating
// it on first use. In master-password mode it stays locked until Unlock.
func (v *Vault) masterKey() ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.key != nil {
		return v.key, nil
	}
	mp, err := store.Load[masterPassword](v.st, masterPasswordFile)
	if err != nil {
		return nil, err
	}
	if mp.UnlocksKey {
		return nil, secretNeeded("locked", "Unlock Hostbay with its master password.")
	}
	key, err := keychainKey()
	if err != nil {
		return nil, secretNeeded("keychain", fmt.Sprintf("The OS keychain did not give Hostbay its key: %v", err))
	}
	v.key = key
	return key, nil
}

// keychainKey reads the master key, creating it when the keychain has none.
// A missing key with existing ciphertext only makes that ciphertext fail to
// open, which callers already handle as SecretNeeded.
func keychainKey() ([]byte, error) {
	s, err := keyring.Get(keyringService, keyringAccount)
	if errors.Is(err, keyring.ErrNotFound) {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := keyring.Set(keyringService, keyringAccount, hex.EncodeToString(key)); err != nil {
			return nil, err
		}
		return key, nil
	}
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(s)
	if err != nil || len(key) != 32 {
		return nil, errors.New("keychain item is not a Hostbay master key")
	}
	return key, nil
}

func secretNeeded(reason, message string) error {
	return apperr.New(apperr.SecretNeeded, message).With("reason", reason)
}
