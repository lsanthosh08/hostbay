package backup

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"errors"

	"golang.org/x/crypto/argon2"
)

// SSH Manager's .sshmbak layout (src-tauri/src/crypto.rs):
// "SSHMBAK1" | salt(16) | nonce(12) | AES-256-GCM ciphertext+tag, keyed by
// Argon2id with the Rust argon2 crate's defaults (m=19456 KiB, t=2, p=1).
const (
	sshmMagic   = "SSHMBAK1"
	sshmSaltLen = 16
	nonceLen    = 12
)

func decryptSSHM(data []byte, passphrase string) ([]byte, error) {
	header := len(sshmMagic) + sshmSaltLen + nonceLen
	if len(data) < header || string(data[:len(sshmMagic)]) != sshmMagic {
		return nil, errors.New("not a valid .sshmbak file")
	}
	salt := data[len(sshmMagic) : len(sshmMagic)+sshmSaltLen]
	key := argon2.IDKey([]byte(passphrase), salt, 2, 19456, 1, 32)
	return openGCM(key, data[len(sshmMagic)+sshmSaltLen:header], data[header:], nil)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func openGCM(key, nonce, ciphertext, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, errors.New("wrong passphrase or corrupt backup")
	}
	return plain, nil
}

// obfKey is SSH Manager's password obfuscation key (src-tauri/src/hosts.rs).
var obfKey = []byte("remote-ssh-manager-obfuscation-key-v1")

// Deobfuscate reverses SSH Manager's "obf:" XOR+base64 password form. A value
// without the prefix is a legacy plaintext password and comes back as is;
// an undecodable one comes back empty, as in SSH Manager.
func Deobfuscate(stored string) string {
	if len(stored) < 4 || stored[:4] != "obf:" {
		return stored
	}
	raw, err := base64.StdEncoding.DecodeString(stored[4:])
	if err != nil {
		return ""
	}
	for i := range raw {
		raw[i] ^= obfKey[i%len(obfKey)]
	}
	return string(raw)
}
