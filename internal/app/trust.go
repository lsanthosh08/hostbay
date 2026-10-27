package app

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strconv"
	"sync"
	"time"

	"hostbay/internal/apperr"
)

// pendingTrustTTL is how long the user has to answer a trust prompt.
const pendingTrustTTL = 5 * time.Minute

// HostKeyTrust holds host keys captured during a handshake that the user has
// not answered yet. The UI only says yes or no to an opaque request ID and
// never supplies host or key material, so a script in the page cannot plant a
// key or a host name in known_hosts (KTD2).
//
// It is not a Wails service: binding Request would let the page register keys.
type HostKeyTrust struct {
	// appendKnownHost writes an accepted key to known_hosts.
	appendKnownHost func(host string, port int, keyType string, key []byte) error

	mu       sync.Mutex
	pending  map[string]pendingKey
	removals map[string]pendingKey // changed keys the user may remove (R13); no key bytes
	now      func() time.Time
}

type pendingKey struct {
	host    string
	port    int
	keyType string
	key     []byte
	expires time.Time
}

func NewHostKeyTrust(appendKnownHost func(host string, port int, keyType string, key []byte) error) *HostKeyTrust {
	return &HostKeyTrust{appendKnownHost: appendKnownHost, pending: map[string]pendingKey{}, removals: map[string]pendingKey{}, now: time.Now}
}

// Request stores a key the user must decide on and returns the HostKeyUnknown
// error that carries its request ID, host, port, key type and fingerprint to
// the UI. The key bytes stay here.
func (t *HostKeyTrust) Request(host string, port int, keyType string, key []byte, fingerprint string) *apperr.Error {
	id := t.add(t.pending, pendingKey{host: host, port: port, keyType: keyType, key: key})
	return apperr.New(apperr.HostKeyUnknown, "The authenticity of "+host+" cannot be established").
		With("host", host).
		With("port", strconv.Itoa(port)).
		With("keyType", keyType).
		With("fingerprint", fingerprint).
		With("requestId", id)
}

// RemovalRequest records the host and port of a changed key and returns the
// ID the UI passes back to ConnService.RemoveHostKey, so the page never names
// the known_hosts entry that gets removed.
func (t *HostKeyTrust) RemovalRequest(host string, port int) string {
	return t.add(t.removals, pendingKey{host: host, port: port})
}

// removal returns an unexpired removal request without using it up, so a
// mistyped name or a cancelled verification can be tried again.
func (t *HostKeyTrust) removal(id string) (pendingKey, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.removals[id]
	return p, ok && !t.now().After(p.expires)
}

// removed uses up a removal request once its key is gone.
func (t *HostKeyTrust) removed(id string) {
	t.mu.Lock()
	delete(t.removals, id)
	t.mu.Unlock()
}

// add stores p in m under a new random ID that expires after pendingTrustTTL.
func (t *HostKeyTrust) add(m map[string]pendingKey, p pendingKey) string {
	id := newRequestID()

	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	for k, old := range m { // prompts never answered would pile up
		if now.After(old.expires) {
			delete(m, k)
		}
	}
	p.expires = now.Add(pendingTrustTTL)
	m[id] = p
	return id
}

// newRequestID returns an opaque random ID for a prompt the UI answers.
func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails on supported platforms
	return hex.EncodeToString(b[:])
}

// Answer resolves a pending request. Each ID works once; unknown and expired
// IDs are refused.
func (t *HostKeyTrust) Answer(requestID string, accept bool) error {
	t.mu.Lock()
	p, ok := t.pending[requestID]
	delete(t.pending, requestID)
	t.mu.Unlock()

	if !ok || t.now().After(p.expires) {
		return errors.New("this host key prompt has expired; connect again")
	}
	if !accept {
		return nil
	}
	return t.appendKnownHost(p.host, p.port, p.keyType, p.key)
}
