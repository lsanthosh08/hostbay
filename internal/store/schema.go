package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// SchemaVersion is the newest file layout this build reads and writes.
// Layout 2 (Release 2) adds proxies.json and per-host proxy, algorithm,
// X11, TLS and URL settings, which a layout 1 build would drop on rewrite.
// Layout 3 (Release 4) adds settings.json, postlogin.json, local.json,
// history.json and workspaces.json.
const SchemaVersion = 3

// ErrNewerSchema means a file was written by a newer Hostbay. Such a file is
// never overwritten.
var ErrNewerSchema = errors.New("written by a newer Hostbay; update Hostbay to use it")

type envelope[T any] struct {
	SchemaVersion int `json:"schemaVersion"`
	Data          T   `json:"data"`
}

// checkVersion is called before decoding data, so a newer layout is refused
// rather than half-decoded.
func checkVersion(name string, b []byte) error {
	var v struct {
		SchemaVersion int `json:"schemaVersion"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if v.SchemaVersion > SchemaVersion {
		return fmt.Errorf("%s has schema version %d, this build reads up to %d: %w", name, v.SchemaVersion, SchemaVersion, ErrNewerSchema)
	}
	return nil
}

// Load reads the document name. A missing file loads as the zero value.
func Load[T any](s *Store, name string) (T, error) {
	var env envelope[T]
	b, err := os.ReadFile(filepath.Join(s.dir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return env.Data, nil
	}
	if err != nil {
		return env.Data, err
	}
	if err := checkVersion(name, b); err != nil {
		return env.Data, err
	}
	if err := json.Unmarshal(b, &env); err != nil {
		return env.Data, fmt.Errorf("%s: %w", name, err)
	}
	return env.Data, nil
}

// Save atomically writes v as the document name. It refuses to replace a
// file written by a newer Hostbay.
func Save[T any](s *Store, name string, v T) error {
	b, err := os.ReadFile(filepath.Join(s.dir, name))
	if err == nil {
		// Only a newer version blocks the write; a corrupt file may be replaced.
		if err := checkVersion(name, b); errors.Is(err, ErrNewerSchema) {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	out, err := json.MarshalIndent(envelope[T]{SchemaVersion, v}, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.dir, name, out)
}

// upgrade rewrites the store files of an older layout at SchemaVersion,
// after Migrate backs them up, so an older Hostbay refuses them rather than
// drop what it does not know. Every older layout decodes as the current one.
func (s *Store) upgrade() error {
	names, err := storeFiles(s.dir)
	if err != nil {
		return err
	}
	from := SchemaVersion
	var old []string
	for _, name := range names {
		b, err := os.ReadFile(filepath.Join(s.dir, name))
		if err != nil {
			return err
		}
		var v struct {
			SchemaVersion int `json:"schemaVersion"`
		}
		if json.Unmarshal(b, &v) != nil || v.SchemaVersion < 1 || v.SchemaVersion >= SchemaVersion {
			continue // current, newer or damaged: Load and Save decide
		}
		old = append(old, name)
		from = min(from, v.SchemaVersion)
	}
	if len(old) == 0 {
		return nil
	}
	return s.Migrate(from, func() error {
		for _, name := range old {
			data, err := Load[json.RawMessage](s, name)
			if err != nil {
				return err
			}
			if err := Save(s, name, data); err != nil {
				return err
			}
		}
		return nil
	})
}

// Migrate copies every store file into backups/v<from>-<time>/ and then runs
// migrate. If migrate fails, the store files are put back as they were and the
// backup is kept.
func (s *Store) Migrate(from int, migrate func() error) error {
	saved, err := storeFiles(s.dir)
	if err != nil {
		return err
	}
	backup := filepath.Join(s.dir, "backups", fmt.Sprintf("v%d-%s", from, time.Now().UTC().Format("20060102T150405Z")))
	if err := os.MkdirAll(backup, 0o700); err != nil {
		return err
	}
	if err := copyFiles(s.dir, backup, saved); err != nil {
		return fmt.Errorf("back up store before migration: %w", err)
	}
	if err := migrate(); err != nil {
		if rerr := s.restore(backup, saved); rerr != nil {
			return errors.Join(err, fmt.Errorf("restore store from %s: %w", backup, rerr))
		}
		return err
	}
	return nil
}

// restore puts the files saved in backup back and removes store files a
// failed migration added.
func (s *Store) restore(backup string, saved []string) error {
	now, err := storeFiles(s.dir)
	if err != nil {
		return err
	}
	for _, name := range now {
		if !slices.Contains(saved, name) {
			if err := os.Remove(filepath.Join(s.dir, name)); err != nil {
				return err
			}
		}
	}
	return copyFiles(backup, s.dir, saved)
}

// storeFiles lists the documents in dir, leaving out temp files, the lock
// and backups.
func storeFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if n := e.Name(); e.Type().IsRegular() && strings.HasSuffix(n, ".json") && !strings.HasPrefix(n, ".") {
			names = append(names, n)
		}
	}
	return names, nil
}

func copyFiles(from, to string, names []string) error {
	for _, name := range names {
		b, err := os.ReadFile(filepath.Join(from, name))
		if err != nil {
			return err
		}
		if err := writeFileAtomic(to, name, b); err != nil {
			return err
		}
	}
	return nil
}
