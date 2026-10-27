// Package sshmanager imports SSH Manager's data folder into Hostbay's store
// on first launch (R25, R31). It only reads SSH Manager's files.
package sshmanager

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"hostbay/internal/backup"
	"hostbay/internal/importer"
	"hostbay/internal/secrets"
	"hostbay/internal/store"
)

// Report says what an import added; it is the shared import report.
type Report = importer.Report

// Skipped names a source file that could not be imported, and why.
type Skipped = importer.Skipped

// DefaultSourceDir is SSH Manager's data folder: Rust's dirs::config_dir()
// joined with "remote-ssh-manager", which is os.UserConfigDir() on macOS,
// Windows and Linux alike.
func DefaultSourceDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "remote-ssh-manager"), nil
}

// ImportOnce imports srcDir unless an import already ran on this store, and
// then records that it ran, even if the user later deletes every host (R31).
// A keychain that refuses the master key still imports the hosts, without
// their passwords (AE7). Any other failure writes nothing and is not
// recorded, so the next launch tries again.
func ImportOnce(st *store.Store, v *secrets.Vault, srcDir string) (Report, error) {
	if _, done := st.SSHManagerImportedAt(); done {
		return Report{}, nil
	}
	rep, err := Import(st, v, srcDir)
	if err != nil || !rep.Ran {
		return rep, err
	}
	return rep, st.MarkSSHManagerImported(time.Now().UTC())
}

// Import merges SSH Manager's hosts, folders, trash, scripts, bookmarks and
// key store from srcDir into st, whether or not it ran before. Each file is
// read on its own; one that cannot be read or parsed is skipped and named in
// the report. Merging follows backup.Restore (KTD12): SSH Manager's IDs are
// kept and matched first, so running it again adds nothing twice. Passwords,
// obf: or legacy plaintext, and key files are sealed under v. If v cannot
// seal, the import runs again with no passwords and no key-store copies:
// hosts keep pointing at SSH Manager's key files, which stay on disk, and
// ask for their passwords at connect (AE7). Nothing is ever stored unsealed.
func Import(st *store.Store, v *secrets.Vault, srcDir string) (Report, error) {
	rep := Report{Skipped: []Skipped{}, Notes: []string{}}
	if _, err := os.Stat(srcDir); errors.Is(err, fs.ErrNotExist) {
		return rep, nil
	} else if err != nil {
		return rep, err
	}
	rep.Ran = true

	b := backup.Bundle{
		Hosts:     readJSON[[]store.Host](&rep, srcDir, "hosts.json"),
		Folders:   readJSON[[]store.Folder](&rep, srcDir, "folders.json"),
		Scripts:   readJSON[[]store.Script](&rep, srcDir, "scripts.json"),
		Bookmarks: readJSON[[]string](&rep, srcDir, "bookmarks.json"),
		Keys:      map[string]string{},
	}
	keysDir := filepath.Join(srcDir, "keys")
	entries, err := os.ReadDir(keysDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		rep.Skipped = append(rep.Skipped, Skipped{File: "keys", Reason: err.Error()})
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(keysDir, e.Name()))
		if err != nil {
			rep.Skipped = append(rep.Skipped, Skipped{File: "keys/" + e.Name(), Reason: err.Error()})
			continue
		}
		b.Keys[e.Name()] = base64.StdEncoding.EncodeToString(raw)
	}
	if err := importer.Restore(st, v, linkKeyStore(b), importer.WithoutSecrets(b), &rep); err != nil {
		return Report{}, err
	}
	return rep, nil
}

// linkKeyStore points hosts that use a key in SSH Manager's key store at the
// copy b carries for Hostbay's key store; SSH Manager stores such a key as
// its absolute path. Other paths stay as they are. b's hosts are copied.
func linkKeyStore(b backup.Bundle) backup.Bundle {
	b.Hosts = append([]store.Host(nil), b.Hosts...)
	for i := range b.Hosts {
		if name := keyStoreName(b.Hosts[i].IdentityFile); b.Keys[name] != "" {
			b.Hosts[i].IdentityFile = store.KeyRefPrefix + name
		}
	}
	return b
}

// readJSON decodes srcDir/name. A missing file is empty; one that does not
// decode is reported and contributes nothing, not a partial decode.
func readJSON[T any](rep *Report, srcDir, name string) T {
	var v T
	raw, err := os.ReadFile(filepath.Join(srcDir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return v
	}
	if err == nil {
		err = json.Unmarshal(raw, &v)
	}
	if err != nil {
		rep.Skipped = append(rep.Skipped, Skipped{File: name, Reason: err.Error()})
		var zero T
		return zero
	}
	return v
}

// keyStoreName returns the key name an SSH Manager IdentityFile refers to in
// its key store: a path ending remote-ssh-manager/keys/<name>, written on any
// OS, or a bare name. Anything else returns "".
func keyStoreName(p string) string {
	parts := strings.Split(strings.ReplaceAll(p, `\`, "/"), "/")
	n := len(parts)
	if n == 1 {
		return parts[0]
	}
	if n >= 3 && parts[n-2] == "keys" && parts[n-3] == "remote-ssh-manager" {
		return parts[n-1]
	}
	return ""
}
