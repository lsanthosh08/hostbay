// Package store keeps Hostbay's own data files: versioned JSON documents in
// one per-user folder, written atomically and guarded by a lock file.
package store

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const stateName = "state.json"

// Meta is the sync metadata every stored item embeds.
type Meta struct {
	ID         string    `json:"id"`
	Revision   int64     `json:"revision"`
	DeviceID   string    `json:"deviceId"` // the device that last changed the item
	ModifiedAt time.Time `json:"modifiedAt"`
}

// state is the store's own bookkeeping.
type state struct {
	DeviceID         string `json:"deviceId"`
	SettingsImported bool   `json:"settingsImported,omitempty"` // the page's localStorage settings were handed over
}

// Store is an open, locked data folder.
type Store struct {
	dir    string // the data folder
	home   string // holds the lock and the pointer
	lock   *os.File
	state  state
	notice string
}

// DefaultDir returns the per-user folder that holds the lock and the
// pointer, and the data too until it is moved (KTD42).
func DefaultDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "Hostbay"), nil
}

// Open creates home if needed and locks it. It returns ErrLocked when
// another process holds the store. The data lives in home unless home's
// pointer names another folder; a pending move is carried out first, and
// a pointer naming a folder that holds no store is a *FolderMissingError.
func Open(home string) (*Store, error) {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	lock, err := acquireLock(home)
	if err != nil {
		return nil, err
	}
	s, err := open(home, lock)
	if err != nil {
		lock.Close()
		return nil, err
	}
	return s, nil
}

func open(home string, lock *os.File) (*Store, error) {
	p, err := readPointer(home)
	if err != nil {
		return nil, &FolderMissingError{Dir: home, Err: err}
	}
	s := &Store{dir: p.dir(home), home: home, lock: lock}
	if s.dir != home && !HoldsStore(s.dir) {
		return nil, &FolderMissingError{Dir: s.dir}
	}
	if p.CleanReady {
		p = s.cleanUp(p)
	}
	switch {
	case p.Pending == "":
	case p.CleanFrom != "" && !samePath(p.Pending, p.CleanFrom):
		// The pointer lists one old folder to clean; moving now would
		// forget the one still waiting, and the copy of the store in it.
		s.notice = fmt.Sprintf("Hostbay will move its data to %s once it has removed its old copy in %s. Reconnect that folder and restart Hostbay.", p.Pending, p.CleanFrom)
	default:
		p = s.carryOut(p)
	}
	err = s.upgrade()
	var st state
	if err == nil {
		st, err = Load[state](s, stateName)
	}
	if err == nil && st.DeviceID == "" {
		st.DeviceID = rand.Text()
		err = Save(s, stateName, st)
	}
	if err != nil {
		return nil, err
	}
	s.state = st
	if len(p.Clean) > 0 && !p.CleanReady {
		// The copy opened, so the next launch may clean up the old folder;
		// if this write fails, a later launch that opens it tries again.
		p.CleanReady = true
		_ = writePointer(home, p)
	}
	return s, nil
}

// Close releases the lock.
func (s *Store) Close() error { return s.lock.Close() }

// DeviceID identifies this install for sync.
func (s *Store) DeviceID() string { return s.state.DeviceID }

// Touch records a local change to m: one more revision, by this device, now.
func (s *Store) Touch(m *Meta) {
	m.Revision++
	m.DeviceID = s.state.DeviceID
	m.ModifiedAt = time.Now().UTC()
}

// SettingsImported reports whether the page's legacy settings were handed
// over, with or without anything to hand.
func (s *Store) SettingsImported() bool { return s.state.SettingsImported }

// MarkSettingsImported persists that the legacy settings were handed over.
func (s *Store) MarkSettingsImported() error {
	st := s.state
	st.SettingsImported = true
	if err := Save(s, stateName, st); err != nil {
		return err
	}
	s.state = st
	return nil
}
