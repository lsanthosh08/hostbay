// Package importertest holds what the importer tests share: a sealing
// store, the fixtures folder, and the scan that proves no imported secret
// reached disk in clear.
package importertest

import (
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"hostbay/internal/importer"
	"hostbay/internal/secrets"
	"hostbay/internal/store"
)

// Fixtures is testdata/importers, from a tool package's directory.
const Fixtures = "../../../testdata/importers"

// Store opens an empty store in a temporary folder with a vault that seals.
func Store(t *testing.T) (*store.Store, *secrets.Vault, string) {
	t.Helper()
	keyring.MockInit()
	dir := filepath.Join(t.TempDir(), "Hostbay")
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	v := secrets.New(st, nil)
	if err := v.SetMasterPassword("test master password"); err != nil { // Linux needs one before sealing
		t.Fatal(err)
	}
	return st, v, dir
}

// Import restores res and fails the test on error.
func Import(t *testing.T, st *store.Store, v *secrets.Vault, res importer.Result) importer.Report {
	t.Helper()
	rep, err := importer.Import(st, v, res)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// NoSecrets fails if any file under dir holds one of secrets in clear or
// base64.
func NoSecrets(t *testing.T, dir string, secrets ...string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		for _, s := range secrets {
			if strings.Contains(string(b), s) || strings.Contains(string(b), base64.StdEncoding.EncodeToString([]byte(s))) {
				t.Errorf("%s holds %q", p, s)
			}
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Host returns the saved host with alias, failing if there is not exactly
// one.
func Host(t *testing.T, st *store.Store, alias string) store.Host {
	t.Helper()
	d, err := st.Data()
	if err != nil {
		t.Fatal(err)
	}
	var out []store.Host
	for _, h := range d.Hosts.Items {
		if h.Alias == alias {
			out = append(out, h)
		}
	}
	if len(out) != 1 {
		t.Fatalf("%d hosts named %q, want 1", len(out), alias)
	}
	return out[0]
}

// Password opens h's sealed password.
func Password(t *testing.T, v *secrets.Vault, h store.Host) string {
	t.Helper()
	if h.Password == "" {
		return ""
	}
	pw, err := v.Open(h.Password, h.SecretAAD())
	if err != nil {
		t.Fatalf("%s: %v", h.Alias, err)
	}
	return string(pw)
}

// FolderPath names h's folder from the root, "/"-joined.
func FolderPath(t *testing.T, st *store.Store, h store.Host) string {
	t.Helper()
	d, err := st.Data()
	if err != nil {
		t.Fatal(err)
	}
	var segs []string
	for id := h.FolderID; id != nil; {
		f := d.Folder(*id)
		if f == nil {
			t.Fatalf("%s: folder %s missing", h.Alias, *id)
		}
		segs = append([]string{f.Name}, segs...)
		id = f.ParentID
	}
	return strings.Join(segs, "/")
}

// Skipped returns the report's skip reason for name, or "".
func Skipped(rep importer.Report, name string) string {
	for _, s := range rep.Skipped {
		if s.File == name {
			return s.Reason
		}
	}
	return ""
}

// Noted reports whether a note about name contains text.
func Noted(rep importer.Report, name, text string) bool {
	for _, n := range rep.Notes {
		if strings.HasPrefix(n, name+": ") && strings.Contains(n, text) {
			return true
		}
	}
	return false
}

// Reimport imports res again and fails if it adds anything.
func Reimport(t *testing.T, st *store.Store, v *secrets.Vault, res importer.Result) {
	t.Helper()
	rep := Import(t, st, v, res)
	if rep.Hosts+rep.Folders+rep.Keys+len(rep.VPNs) != 0 {
		t.Fatalf("second import added %d hosts, %d folders, %d keys, %d VPN profiles", rep.Hosts, rep.Folders, rep.Keys, len(rep.VPNs))
	}
}

// VPN returns the saved VPN profile named name, failing if there is not
// exactly one.
func VPN(t *testing.T, st *store.Store, name string) store.VPN {
	t.Helper()
	d, err := st.Data()
	if err != nil {
		t.Fatal(err)
	}
	var out []store.VPN
	for _, p := range d.VPNs.Items {
		if p.Name == name {
			out = append(out, p)
		}
	}
	if len(out) != 1 {
		t.Fatalf("%d VPN profiles named %q, want 1", len(out), name)
	}
	return out[0]
}

// VPNSecrets opens p's sealed secrets.
func VPNSecrets(t *testing.T, v *secrets.Vault, p store.VPN) store.VPNSecrets {
	t.Helper()
	var s store.VPNSecrets
	if p.Secret == "" {
		return s
	}
	raw, err := v.Open(p.Secret, p.SecretAAD())
	if err != nil {
		t.Fatalf("%s: %v", p.Name, err)
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// Ignored returns the directives the report lists as dropped from the VPN
// profile named name.
func Ignored(rep importer.Report, name string) []string {
	for _, p := range rep.VPNs {
		if p.Name == name {
			return p.Ignored
		}
	}
	return nil
}
