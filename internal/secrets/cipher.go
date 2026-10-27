// Package secrets keeps saved passwords and other secret fields encrypted at
// rest under a master key held in the OS keychain, and gates revealing them
// behind OS user verification that fails closed.
//
// The master key, enc:v1 field and keychain-lost handling follow uniterm's
// backend/credentials (github.com/ys-ll/uniterm, Apache-2.0).
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
)

// prefix marks a sealed field and its format version.
const prefix = "enc:v1:"

var errNotSealed = errors.New("not a sealed secret")

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// seal encrypts plaintext with AES-256-GCM, binding aad, as
// "enc:v1:" + base64(nonce|ciphertext).
func seal(key, plaintext, aad []byte) (string, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return prefix + base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, plaintext, aad)), nil
}

// open reverses seal. It fails on a malformed value, a wrong key or aad that
// differs from the one sealed.
func open(key []byte, sealed string, aad []byte) ([]byte, error) {
	body, ok := strings.CutPrefix(sealed, prefix)
	if !ok {
		return nil, errNotSealed
	}
	raw, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		return nil, err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(raw) < gcm.NonceSize() {
		return nil, errNotSealed
	}
	return gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], aad)
}
