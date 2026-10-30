// Package sync keeps two computers' Hostbay data in step through a Git
// repo, WebDAV, a folder, S3, SFTP or SMB (R35–R37, KTD13). Everything that
// leaves the computer is encrypted under a key derived from the sync
// passphrase; the target only ever holds ciphertext.
//
// The backend interface and per-target layout follow uniterm's
// backend/sync (github.com/ys-ll/uniterm, Apache-2.0); the snapshot format,
// merge and safety rules are Hostbay's own.
package sync

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// ReaderVersion is the newest snapshot format this build reads; a manifest
// whose minReaderVersion is higher stops sync and asks for an update.
const ReaderVersion = 6

// minReaderVersion is what every snapshot this build writes demands. 2
// brought file hosts (FTP, SMB, WebDAV, S3) and their fields, which a
// Release 1 build would drop on its next edit of such a host; 3 brought
// proxies and the connection extras (proxy, agent forwarding, algorithms,
// keepalive, raw TCP), which a version 2 build would drop the same way
// (KTD22); 4 brought Release 3's database and Kubernetes hosts and their
// fields, which a version 3 build would drop or connect without TLS; 5
// brought Release 4's collections, settings first (KTD37), which a version 4
// build would drop; 6 brings VPN profiles and host VPN bindings (KTD4),
// which a version 5 build would drop and then connect around.
const minReaderVersion = 6

var (
	ErrDecrypt      = errors.New("the sync snapshot cannot be decrypted: the passphrase is wrong or the data was changed")
	ErrUpdateNeeded = errors.New("the sync data was written by a newer Hostbay; update Hostbay to keep syncing")
	ErrRollback     = errors.New("the sync target holds an older snapshot than one already seen; sync is paused")
	ErrConflict     = errors.New("the sync target changed during the upload")
)

// KDF is the Argon2id setting that turns the sync passphrase into the sync
// key. It travels in the manifest so a second computer derives the same key.
type KDF struct {
	Salt    []byte `json:"salt"`
	Time    uint32 `json:"time"`
	Memory  uint32 `json:"memory"` // KiB
	Threads uint8  `json:"threads"`
}

// The write setting and floor match backups (KTD12): RFC 9106's second
// recommended setting. A manifest asking for less is refused, so a
// tampered target cannot make this computer encrypt under a weak key.
const (
	kdfTime, kdfMemory, kdfThreads = 3, 64 * 1024, 4
	maxKDFMemory, maxKDFTime       = 1024 * 1024, 16
)

// NewKDF returns the write setting with a fresh salt.
func NewKDF() (KDF, error) {
	k := KDF{Salt: make([]byte, 16), Time: kdfTime, Memory: kdfMemory, Threads: kdfThreads}
	_, err := rand.Read(k.Salt)
	return k, err
}

// Derive returns the 256-bit sync key for passphrase.
func (k KDF) Derive(passphrase string) ([]byte, error) {
	if k.Time < kdfTime || k.Memory < kdfMemory || k.Threads == 0 || len(k.Salt) < 16 ||
		k.Time > maxKDFTime || k.Memory > maxKDFMemory {
		return nil, errors.New("sync key parameters are outside the allowed range")
	}
	return argon2.IDKey([]byte(passphrase), k.Salt, k.Time, k.Memory, k.Threads, 32), nil
}

// CheckPassphrase enforces the minimum strength of a sync passphrase: at
// least 12 characters, not one character repeated, and some variety.
func CheckPassphrase(p string) error {
	if utf8.RuneCountInString(p) < 12 {
		return errors.New("use a sync passphrase of at least 12 characters")
	}
	distinct := map[rune]bool{}
	classes := map[string]bool{}
	for _, r := range p {
		distinct[r] = true
		switch {
		case unicode.IsLower(r):
			classes["lower"] = true
		case unicode.IsUpper(r):
			classes["upper"] = true
		case unicode.IsDigit(r):
			classes["digit"] = true
		default:
			classes["other"] = true
		}
	}
	if len(distinct) < 6 || (len(classes) < 2 && utf8.RuneCountInString(p) < 20) {
		return errors.New("the sync passphrase is too easy to guess; use more varied characters or several words")
	}
	return nil
}

// DeviceMark is what a snapshot records of one computer's syncs.
type DeviceMark struct {
	Generation uint64    `json:"generation"`
	SyncedAt   time.Time `json:"syncedAt"`
}

// Manifest describes a snapshot. It is readable on the target but bound
// into the encryption as additional data, so it cannot be swapped or edited.
// It holds no host names or aliases.
type Manifest struct {
	Format           string                `json:"format"`
	SchemaVersion    int                   `json:"schemaVersion"`
	MinReaderVersion int                   `json:"minReaderVersion"`
	KeyID            string                `json:"keyId"` // changes with the passphrase
	KDF              KDF                   `json:"kdf"`
	DeviceID         string                `json:"deviceId"`   // the writer
	Generation       uint64                `json:"generation"` // the writer's, only goes up
	Devices          map[string]DeviceMark `json:"devices"`
	CreatedAt        time.Time             `json:"createdAt"`

	extra map[string]json.RawMessage // fields from a newer Hostbay, kept on round trip
}

const manifestFormat = "hostbay-sync"

func (m *Manifest) UnmarshalJSON(b []byte) error {
	type plain Manifest
	if err := json.Unmarshal(b, (*plain)(m)); err != nil {
		return err
	}
	if err := json.Unmarshal(b, &m.extra); err != nil {
		return err
	}
	for _, k := range knownKeys(plain{}) {
		delete(m.extra, k)
	}
	return nil
}

func (m Manifest) MarshalJSON() ([]byte, error) {
	type plain Manifest
	b, err := json.Marshal(plain(m))
	if err != nil || len(m.extra) == 0 {
		return b, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return nil, err
	}
	for k, v := range m.extra {
		if _, ok := all[k]; !ok {
			all[k] = v
		}
	}
	return json.Marshal(all)
}

// knownKeys lists the JSON keys v marshals.
func knownKeys(v any) []string {
	b, _ := json.Marshal(v)
	var m map[string]json.RawMessage
	_ = json.Unmarshal(b, &m)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// A snapshot file is
//
//	"HBSYNC01" | manifest length (4, big endian) | manifest JSON |
//	nonce (12) | AES-256-GCM ciphertext+tag
//
// with everything before the nonce as the GCM additional data.
const snapshotMagic = "HBSYNC01"

// Seal encrypts plain under key with m as its manifest.
func Seal(m Manifest, plain, key []byte) ([]byte, error) {
	m.Format = manifestFormat
	if m.SchemaVersion == 0 {
		m.SchemaVersion = ReaderVersion
	}
	if m.MinReaderVersion == 0 {
		m.MinReaderVersion = minReaderVersion
	}
	mj, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	head := make([]byte, 0, len(snapshotMagic)+4+len(mj))
	head = append(head, snapshotMagic...)
	head = binary.BigEndian.AppendUint32(head, uint32(len(mj)))
	head = append(head, mj...)
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append(head, nonce...)
	return gcm.Seal(out, nonce, plain, head), nil
}

// manifestEnd returns where data's manifest ends.
func manifestEnd(data []byte) (int, error) {
	if len(data) < len(snapshotMagic)+4 || string(data[:len(snapshotMagic)]) != snapshotMagic {
		return 0, errors.New("not a Hostbay sync snapshot")
	}
	n := int(binary.BigEndian.Uint32(data[len(snapshotMagic):]))
	end := len(snapshotMagic) + 4 + n
	if n > 1<<20 || end > len(data) {
		return 0, errors.New("sync snapshot is damaged")
	}
	return end, nil
}

// ReadManifest returns data's manifest without checking it. Only use it to
// learn the KDF before the key exists; Open authenticates it.
func ReadManifest(data []byte) (Manifest, error) {
	var m Manifest
	end, err := manifestEnd(data)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(data[len(snapshotMagic)+4:end], &m); err != nil {
		return m, fmt.Errorf("sync manifest: %w", err)
	}
	return m, nil
}

// Open checks and decrypts a snapshot. A snapshot that needs a newer reader
// returns ErrUpdateNeeded; a wrong key or any change returns ErrDecrypt.
func Open(data, key []byte) (Manifest, []byte, error) {
	m, err := ReadManifest(data)
	if err != nil {
		return m, nil, err
	}
	if m.MinReaderVersion > ReaderVersion {
		return m, nil, ErrUpdateNeeded
	}
	end, _ := manifestEnd(data)
	gcm, err := newGCM(key)
	if err != nil {
		return m, nil, err
	}
	if len(data) < end+gcm.NonceSize() {
		return m, nil, ErrDecrypt
	}
	plain, err := gcm.Open(nil, data[end:end+gcm.NonceSize()], data[end+gcm.NonceSize():], data[:end])
	if err != nil {
		return m, nil, ErrDecrypt
	}
	return m, plain, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Secret fields inside a snapshot are sealed again under the sync key with
// their KTD6 binding as additional data, so a secret only opens for the
// host it was saved for (KTD13).
const (
	fieldPrefix = "hbsync:v1:"
	// unavailable marks a secret the sender has but could not decrypt; a
	// receiver keeps its own copy rather than treating it as cleared.
	unavailable = "hbsync:unavailable"
)

func sealField(key, plain, aad []byte) (string, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return fieldPrefix + base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, plain, aad)), nil
}

func openField(key []byte, sealed string, aad []byte) ([]byte, bool) {
	body, ok := strings.CutPrefix(sealed, fieldPrefix)
	if !ok {
		return nil, false
	}
	raw, err := base64.StdEncoding.DecodeString(body)
	gcm, gerr := newGCM(key)
	if err != nil || gerr != nil || len(raw) < gcm.NonceSize() {
		return nil, false
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], aad)
	return plain, err == nil
}
