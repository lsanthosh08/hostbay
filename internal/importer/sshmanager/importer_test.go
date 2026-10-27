package sshmanager

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"hostbay/internal/secrets"
	"hostbay/internal/store"
)

// testdata/sshmanager/remote-ssh-manager was written by SSH Manager's own
// Rust code, not by Go: a temporary #[ignore]d test appended to
// src-tauri/src/hosts.rs built the hosts, folders and scripts with its serde
// structs (passwords through obfuscate(), one left as a legacy plaintext
// value), wrote each with serde_json::to_string_pretty as write_hosts() does,
// and wrote three files under keys/. It ran as
//
//	HOSTBAY_FIXTURE_OUT=$PWD/../testdata/sshmanager/remote-ssh-manager \
//	  cargo test --lib hostbay_import_fixture -- --ignored --nocapture
//
// from src-tauri, and was then removed. h-db and h-ops point at keys/ by
// absolute macOS and Windows paths, as SSH Manager's save_host stores them.
const fixtureDir = "../../../testdata/sshmanager/remote-ssh-manager"

// Plaintext passwords in the fixture, and the form hosts.json stores them in.
var fixturePasswords = map[string][2]string{
	"h-web-prod":  {"Prod-Pass-1!", "obf:IhcCC1k1TAAARRxM"},
	"h-web-stage": {"Stage-Pass-2!", "obf:IREMCBFIfRIAGwBfQA=="},
	"h-legacy":    {"Legacy-Plain-4!", "Legacy-Plain-4!"},
	"h-desk":      {"Rdp-Pass-5!", "obf:IAEdQiQEXgBeXQw="},
}

func newStore(t *testing.T) (*store.Store, *secrets.Vault, string) {
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

// source copies the fixture so a test may damage it.
func source(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "remote-ssh-manager")
	if err := os.CopyFS(dir, os.DirFS(fixtureDir)); err != nil {
		t.Fatal(err)
	}
	return dir
}

func hashTree(t *testing.T, dir string) map[string][32]byte {
	t.Helper()
	out := map[string][32]byte{}
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		out[p] = sha256.Sum256(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// assertNoSecrets fails if any file under dir holds a fixture password in
// plaintext, obf: or base64 form, or a fixture key in readable form.
func assertNoSecrets(t *testing.T, dir string) {
	t.Helper()
	var needles []string
	for _, p := range fixturePasswords {
		needles = append(needles, p[0], p[1], base64.StdEncoding.EncodeToString([]byte(p[0])))
	}
	needles = append(needles, "obf:", "hostbay-import-deploy-key", "hostbay-import-ops-key", "hostbay-import-spare-key")
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		for _, n := range needles {
			if strings.Contains(string(b), n) {
				t.Errorf("%s holds %q", p, n)
			}
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func data(t *testing.T, st *store.Store) *store.Data {
	t.Helper()
	d, err := st.Data()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func importOnce(t *testing.T, st *store.Store, v *secrets.Vault, src string) Report {
	t.Helper()
	rep, err := ImportOnce(st, v, src)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// Covers AE5.
func TestNoSSHManagerFolderStartsEmpty(t *testing.T) {
	st, v, dir := newStore(t)
	rep := importOnce(t, st, v, filepath.Join(t.TempDir(), "remote-ssh-manager"))
	if rep.Ran {
		t.Fatalf("report = %+v, want none", rep)
	}
	if _, err := os.Stat(filepath.Join(dir, "hosts.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("hosts.json written: %v", err)
	}
	if _, done := st.SSHManagerImportedAt(); done {
		t.Fatal("marker set though nothing was imported")
	}
}

// Covers F1, AE6, and the private-key and same-alias edge cases.
func TestImportBringsEverythingOverSealed(t *testing.T) {
	st, v, dir := newStore(t)
	src := source(t)
	before := hashTree(t, src)

	rep := importOnce(t, st, v, src)
	want := Report{Ran: true, Hosts: 8, Folders: 2, Scripts: 2, Keys: 3}
	if rep.Ran != want.Ran || rep.Hosts != want.Hosts || rep.Folders != want.Folders || rep.Scripts != want.Scripts || rep.Keys != want.Keys || len(rep.Skipped) != 0 {
		t.Fatalf("report = %+v, want %+v", rep, want)
	}
	if after := hashTree(t, src); len(after) != len(before) || !mapsEqual(before, after) {
		t.Fatal("SSH Manager's files changed")
	}
	if _, done := st.SSHManagerImportedAt(); !done {
		t.Fatal("marker not set")
	}
	assertNoSecrets(t, dir)

	d := data(t, st)
	for id, pw := range fixturePasswords {
		h := d.Host(id)
		got, err := v.Open(h.Password, h.SecretAAD())
		if err != nil || string(got) != pw[0] {
			t.Errorf("%s password = %q, %v; want %q", id, got, err, pw[0])
		}
	}
	if f := d.Host("h-web-prod").FolderID; f == nil || *f != "f-prod" {
		t.Errorf("h-web-prod folder = %v", f)
	}
	if f := d.Host("h-web-stage").FolderID; f == nil || *f != "f-stage" {
		t.Errorf("h-web-stage folder = %v", f)
	}
	if p := d.Folder("f-stage").ParentID; p == nil || *p != "f-prod" {
		t.Errorf("Staging parent = %v", p)
	}
	if h := d.Host("h-old"); h.DeletedAt == "" || h.FolderID != nil {
		t.Errorf("trashed host with missing folder = deletedAt %q folder %v", h.DeletedAt, h.FolderID)
	}
	if h := d.Host("h-desk"); h.Kind == nil || *h.Kind != "rdp" || *h.RdpSecurity != "tls" || *h.Mac != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("rdp host = %+v", h)
	}
	for id, ref := range map[string]string{"h-db": "keystore:deploy_key", "h-ops": "keystore:ops.pem", "h-bastion": "~/.ssh/id_ed25519"} {
		if got := d.Host(id).IdentityFile; got != ref {
			t.Errorf("%s identity = %q, want %q", id, got, ref)
		}
	}
	for _, name := range []string{"deploy_key", "ops.pem", "spare_key"} {
		k := d.KeyByName(name)
		if k == nil {
			t.Fatalf("key %s not imported", name)
		}
		raw, _ := os.ReadFile(filepath.Join(src, "keys", name))
		if got, err := v.Open(k.Data, store.KeyAAD(k.ID)); err != nil || string(got) != string(raw) {
			t.Errorf("key %s opens to %q, %v", name, got, err)
		}
	}
	if got := d.BookmarkPaths(); !slices.Equal(got, []string{"/etc/nginx", "/srv/app"}) {
		t.Errorf("bookmarks = %v", got)
	}
	var labels []string
	for _, s := range d.ScriptList() {
		labels = append(labels, s.Label)
	}
	if !slices.Equal(labels, []string{"nginx -t", "Uptime"}) {
		t.Errorf("scripts = %v", labels)
	}
}

func mapsEqual(a, b map[string][32]byte) bool {
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// Covers AE8.
func TestImportDoesNotRunAgainAfterHostsDeleted(t *testing.T) {
	st, v, dir := newStore(t)
	src := source(t)
	importOnce(t, st, v, src)
	if err := st.Edit(func(d *store.Data) error {
		for len(d.Hosts.Items) > 0 {
			if err := d.PurgeHost(d.Hosts.Items[0].ID); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	st, err := store.Open(dir) // a restart
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if rep := importOnce(t, st, secrets.New(st, nil), src); rep.Ran {
		t.Fatalf("second import ran: %+v", rep)
	}
	if n := len(data(t, st).Hosts.Items); n != 0 {
		t.Fatalf("%d hosts after restart, want 0", n)
	}
}

// Covers AE7's damaged file: the rest imports and the file is named.
func TestImportSkipsCorruptFileAndKeepsTheRest(t *testing.T) {
	st, v, _ := newStore(t)
	src := source(t)
	p := filepath.Join(src, "folders.json")
	raw, _ := os.ReadFile(p)
	if err := os.WriteFile(p, raw[:len(raw)/2], 0o600); err != nil {
		t.Fatal(err)
	}

	rep := importOnce(t, st, v, src)
	if rep.Hosts != 8 || rep.Folders != 0 || len(rep.Skipped) != 1 || rep.Skipped[0].File != "folders.json" {
		t.Fatalf("report = %+v", rep)
	}
	for _, h := range data(t, st).Hosts.Items {
		if h.FolderID != nil {
			t.Errorf("%s kept folder %s with no folders imported", h.ID, *h.FolderID)
		}
	}
	if _, done := st.SSHManagerImportedAt(); !done {
		t.Fatal("marker not set after an import that skipped one file")
	}
}

// A keychain that refuses access still imports every host, stores no
// password in any form, records the import, and leaves those hosts asking
// for their passwords at connect (AE7).
func TestKeychainDeniedImportsHostsWithoutPasswords(t *testing.T) {
	st, _, dir := newStore(t)
	src := source(t)
	keyring.MockInitWithError(errors.New("user denied access"))
	v := secrets.New(st, nil)
	rep, err := ImportOnce(st, v, src)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Hosts != 8 || rep.Keys != 0 || rep.PasswordsNeeded == 0 {
		t.Fatalf("report = %+v", rep)
	}
	if _, done := st.SSHManagerImportedAt(); !done {
		t.Fatal("marker not set after the hosts imported")
	}
	assertNoSecrets(t, dir)
	d := data(t, st)
	for _, h := range d.Hosts.Items {
		if h.Password != "" {
			t.Errorf("%s kept a password the keychain could not seal", h.ID)
		}
		if strings.HasPrefix(h.IdentityFile, store.KeyRefPrefix) {
			t.Errorf("%s points at a key-store key that was not imported: %s", h.ID, h.IdentityFile)
		}
	}
	if len(d.Keys.Items) != 0 {
		t.Fatalf("%d keys stored without sealing", len(d.Keys.Items))
	}
}

// A crash part way through saving, then a relaunch, adds nothing twice.
func TestRerunAfterPartialImportAddsNoDuplicates(t *testing.T) {
	st, v, dir := newStore(t)
	src := source(t)
	if _, err := Import(st, v, src); err != nil { // no marker, as if it crashed
		t.Fatal(err)
	}
	for _, name := range []string{"folders.json", "keys.json", "scripts.json", "bookmarks.json"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}

	rep := importOnce(t, st, v, src)
	if rep.Hosts != 0 || rep.Folders != 2 || rep.Keys != 3 {
		t.Fatalf("rerun report = %+v", rep)
	}
	if rep, err := Import(st, v, src); err != nil || rep.Hosts+rep.Folders+rep.Keys+rep.Scripts != 0 {
		t.Fatalf("third run = %+v, %v", rep, err)
	}
	d := data(t, st)
	if len(d.Hosts.Items) != 8 || len(d.Folders.Items) != 2 || len(d.Keys.Items) != 3 || len(d.Scripts.Items) != 2 {
		t.Fatalf("store has %d hosts, %d folders, %d keys, %d scripts", len(d.Hosts.Items), len(d.Folders.Items), len(d.Keys.Items), len(d.Scripts.Items))
	}
	if f := d.Host("h-web-stage").FolderID; f == nil || d.Folder(*f) == nil {
		t.Fatal("h-web-stage lost its folder")
	}
}

// Existing Hostbay hosts merge under KTD12: an address match is not added
// again, an alias alone is not a match.
func TestImportMergesWithExistingHosts(t *testing.T) {
	st, v, _ := newStore(t)
	if err := st.Edit(func(d *store.Data) error {
		d.AddHost(store.Host{Alias: "db", Hostname: "10.0.1.20", Port: 22, User: "postgres"})
		d.AddHost(store.Host{Alias: "web", Hostname: "192.0.2.1", Port: 22, User: "deploy"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if rep := importOnce(t, st, v, source(t)); rep.Hosts != 7 {
		t.Fatalf("report = %+v, want 7 hosts (db already here)", rep)
	}
	if n := len(data(t, st).Hosts.Items); n != 9 {
		t.Fatalf("%d hosts, want 9", n)
	}
}
