package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/cloudsoda/go-smb2"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"hostbay/internal/apperr"
	"hostbay/internal/secrets"
	"hostbay/internal/store"
)

// Engine runs sync for one store against one target. Calls must not
// overlap; the app serialises them.
type Engine struct {
	Store   *store.Store
	Vault   *secrets.Vault
	Backend Backend
	Key     []byte // the sync key
	KDF     KDF
	KeyID   string
	// PrevKey and PrevKeyID read snapshots written before a passphrase
	// change, for the one sync that re-encrypts them under Key.
	PrevKey   []byte
	PrevKeyID string
	State     *State
	Now       func() time.Time
}

const maxRetries = 5

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now().UTC()
	}
	return time.Now().UTC()
}

// Sync downloads, merges, writes the merge locally and uploads it,
// retrying from the download when another computer uploaded first.
// choice answers the first sync on a computer that already has data:
// "remote" takes the target's data, "merge" matches items under KTD12.
func (e *Engine) Sync(ctx context.Context, choice string) (Report, error) {
	var rep Report
	var err error
	for {
		err = e.once(ctx, choice, &rep)
		if !errors.Is(err, ErrConflict) || rep.Retries >= maxRetries {
			break
		}
		rep.Retries++
	}
	e.State.LastError, e.State.Paused = "", ""
	if err != nil {
		e.State.LastError = err.Error()
		e.State.Paused = PauseCode(err)
	} else {
		e.State.LastSync = e.now()
	}
	if serr := SaveState(e.Store, e.State); serr != nil && err == nil {
		err = serr
	}
	return rep, err
}

// PauseCode is the code err pauses sync with, or "" when the next attempt
// may simply try again.
func PauseCode(err error) string {
	switch {
	case errors.Is(err, ErrDecrypt):
		return CodeDecrypt
	case errors.Is(err, ErrUpdateNeeded):
		return CodeUpdateNeeded
	case errors.Is(err, ErrRollback):
		return CodeRollback
	case errors.Is(err, ErrPassphraseChanged):
		return CodePassphraseChange
	case apperr.CodeOf(err) == apperr.HostKeyChanged:
		return CodeHostKeyChanged
	case signInRefused(err):
		return CodeAuth
	case errors.Is(err, ErrClock):
		return CodeClock
	}
	return ""
}

// signInRefused reports whether the target refused the credentials. Many
// servers lock an account after repeated failures, so this pauses sync
// instead of letting the background tick retry every minute.
func signInRefused(err error) bool {
	if err == nil {
		return false
	}
	var smb *smb2.ResponseError
	if errors.As(err, &smb) {
		switch smb.Code {
		case 0xC000006D, // STATUS_LOGON_FAILURE
			0xC0000234, // STATUS_ACCOUNT_LOCKED_OUT
			0xC0000072, // STATUS_ACCOUNT_DISABLED
			0xC0000071: // STATUS_PASSWORD_EXPIRED
			return true
		}
	}
	// x/crypto/ssh reports a refused sign-in only as text.
	return errors.Is(err, ErrAuth) ||
		errors.Is(err, transport.ErrAuthenticationRequired) || errors.Is(err, transport.ErrAuthorizationFailed) ||
		strings.Contains(err.Error(), "ssh: unable to authenticate")
}

// remote is one decrypted snapshot.
type remote struct {
	m       Manifest
	payload map[string]json.RawMessage
	sets    map[string]Set
}

// pull downloads and checks the target's snapshots. It returns the ones to
// merge, the generations they show, and the highest generation this
// device already used there.
func (e *Engine) pull(ctx context.Context) ([]remote, string, map[string]uint64, uint64, error) {
	files, version, err := e.Backend.Fetch(ctx)
	if err != nil {
		return nil, "", nil, 0, err
	}
	me := e.Store.DeviceID()
	seen := maps.Clone(e.State.Seen)
	if seen == nil {
		seen = map[string]uint64{}
	}
	// A file under another key that is older than the newest file under
	// this computer's key was written before the passphrase changed. Its
	// device is paused until it rejoins with the new passphrase and writes
	// a newer file, so skip it rather than pause here too; only a newer
	// file under another key means the passphrase changed again.
	var current time.Time
	for _, f := range files {
		if m, err := ReadManifest(f.Data); err == nil && m.KeyID == e.KeyID && m.CreatedAt.After(current) {
			current = m.CreatedAt
		}
	}
	var mine uint64
	var out []remote
	for _, f := range files {
		if e.Backend.PerDevice() {
			dev, gen, ok := parseDeviceFileName(f.Name)
			if !ok {
				continue
			}
			if dev == me {
				mine = max(mine, gen)
				continue // this computer's own data is the local store
			}
			if gen < e.State.Seen[dev] {
				return nil, "", nil, 0, ErrRollback
			}
			if gen == e.State.Seen[dev] {
				continue // merged already
			}
		}
		pm, err := ReadManifest(f.Data)
		if err != nil {
			return nil, "", nil, 0, fmt.Errorf("%s: %w", f.Name, err)
		}
		if pm.MinReaderVersion > ReaderVersion {
			return nil, "", nil, 0, ErrUpdateNeeded
		}
		key := e.Key
		switch {
		case pm.KeyID == e.KeyID:
		case e.PrevKey != nil && pm.KeyID == e.PrevKeyID:
			key = e.PrevKey
		case pm.CreatedAt.Before(current):
			continue
		default:
			return nil, "", nil, 0, ErrPassphraseChanged
		}
		m, plain, err := Open(f.Data, key)
		if err != nil {
			return nil, "", nil, 0, err
		}
		if err := checkGenerations(m, e.State.Seen, e.Backend.PerDevice()); err != nil {
			return nil, "", nil, 0, err
		}
		r := remote{m: m, sets: map[string]Set{}}
		if err := json.Unmarshal(plain, &r.payload); err != nil {
			return nil, "", nil, 0, fmt.Errorf("sync snapshot: %w", err)
		}
		for _, c := range collections {
			var w wireSet
			if raw, ok := r.payload[c.name]; ok {
				if err := json.Unmarshal(raw, &w); err != nil {
					return nil, "", nil, 0, fmt.Errorf("sync snapshot %s: %w", c.name, err)
				}
			}
			if r.sets[c.name], err = w.set(); err != nil {
				return nil, "", nil, 0, err
			}
		}
		seen[m.DeviceID] = max(seen[m.DeviceID], m.Generation)
		if !e.Backend.PerDevice() {
			for d, mark := range m.Devices {
				seen[d] = max(seen[d], mark.Generation)
			}
			mine = max(mine, m.Devices[me].Generation)
		}
		out = append(out, r)
	}
	return out, version, seen, mine, nil
}

// checkGenerations refuses a snapshot older than one already seen. A
// shared snapshot accumulates every device's marks, so all of them must
// hold; a per-device file only vouches for its writer.
func checkGenerations(m Manifest, seen map[string]uint64, perDevice bool) error {
	if m.Generation < seen[m.DeviceID] {
		return ErrRollback
	}
	if perDevice {
		return nil
	}
	for d, mark := range m.Devices {
		if mark.Generation < seen[d] {
			return ErrRollback
		}
	}
	return nil
}

// Due reports whether a sync has work: a local change since the last sync,
// or a target that changed since.
func (e *Engine) Due(ctx context.Context) (bool, error) {
	d, err := e.Store.Data()
	if err != nil {
		return false, err
	}
	for _, c := range collections {
		s, _ := c.get(d)
		now, base := metas(s), e.State.Base[c.name]
		if len(now) != len(base) {
			return true, nil
		}
		for id, m := range now {
			if b, ok := base[id]; !ok || !sameMeta(b, m) {
				return true, nil
			}
		}
	}
	v, err := e.Backend.Version(ctx)
	return v != e.State.Version, err
}
