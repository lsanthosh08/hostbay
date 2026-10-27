package backup

import (
	"bytes"
	"encoding/base64"
	"os"
	"strings"
	"testing"

	"hostbay/internal/secrets"
	"hostbay/internal/store"
)

// The fixture testdata/sshmanager/backup.sshmbak was written by SSH Manager's
// own Rust code, not by Go. To regenerate it, append a temporary #[ignore]d
// test to src-tauri/src/hosts.rs that builds the hosts below as read_hosts()
// returns them (passwords through obfuscate()), maps them through
// host_for_backup(), sets the db host to "keystore:deploy_key" with that key
// in StoreBundle.keys (as collect_backup_keys() does), and calls
// write_encrypted_bundle(&bundle, out, fixturePassphrase), which uses
// crypto::encrypt. Run it with
//
//	HOSTBAY_FIXTURE_OUT=$PWD/../testdata/sshmanager/backup.sshmbak \
//	  cargo test --lib hostbay_fixture -- --ignored --nocapture
//
// from src-tauri, then remove the test. The same run printed
// obfuscate("p@ss wörd") as rustObfuscated.
const (
	fixturePath       = "../../testdata/sshmanager/backup.sshmbak"
	fixturePassphrase = "hostbay fixture passphrase"
	fixtureKey        = "-----BEGIN OPENSSH PRIVATE KEY-----\nhostbay-fixture-deploy-key\n-----END OPENSSH PRIVATE KEY-----\n"
	rustObfuscated    = "obf:AiUeHFQS7sUBDA=="
)

var fixturePasswords = map[string]string{
	"h-web-1": "Fixture-Pass-1!",
	"h-web-2": "Fixture-Pass-2!",
	"h-desk":  "Rdp-Pass-3!",
}

func readFixture(t *testing.T) Bundle {
	t.Helper()
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Read(raw, fixturePassphrase)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func restore(t *testing.T, st *store.Store, v *secrets.Vault, b Bundle) int {
	t.Helper()
	var n int
	err := st.Edit(func(d *store.Data) error {
		var err error
		n, _, err = Restore(d, v, b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDeobfuscateMatchesSSHManager(t *testing.T) {
	if got := Deobfuscate(rustObfuscated); got != "p@ss wörd" {
		t.Fatalf("Deobfuscate = %q", got)
	}
	if got := Deobfuscate("legacy plain"); got != "legacy plain" {
		t.Fatalf("plaintext legacy value = %q", got)
	}
}

func TestSSHManagerBackupRestoresWithPasswordsResealed(t *testing.T) {
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Read(raw, "wrong"); err == nil {
		t.Fatal("fixture opened with a wrong passphrase")
	}
	st, v, dir := newEnvDir(t)
	if n := restore(t, st, v, readFixture(t)); n != 5 {
		t.Fatalf("restored %d hosts, want 5", n)
	}
	d := data(t, st)

	for id, want := range fixturePasswords {
		h := d.Host(id)
		if h == nil {
			t.Fatalf("host %s missing", id)
		}
		if !strings.HasPrefix(h.Password, "enc:v1:") {
			t.Fatalf("%s password stored as %q", id, h.Password)
		}
		if got, err := v.Open(h.Password, h.SecretAAD()); err != nil || string(got) != want {
			t.Fatalf("%s password = %q, %v", id, got, err)
		}
	}
	// Two hosts share the alias "web" in different folders; both arrive.
	web1, web2 := d.Host("h-web-1"), d.Host("h-web-2")
	if d.Folder(*web1.FolderID).Name != "Prod" || d.Folder(*web2.FolderID).Name != "DB" {
		t.Fatal("web hosts not in their folders")
	}
	if p := d.Folder(*web2.FolderID).ParentID; p == nil || *p != *web1.FolderID {
		t.Fatal("DB folder is not under Prod")
	}
	db := d.Host("h-db")
	k := d.KeyByName(strings.TrimPrefix(db.IdentityFile, store.KeyRefPrefix))
	if k == nil {
		t.Fatalf("db key %q not in the key store", db.IdentityFile)
	}
	if got, err := v.Open(k.Data, store.KeyAAD(k.ID)); err != nil || string(got) != fixtureKey {
		t.Fatalf("db key = %q, %v", got, err)
	}
	if d.Host("h-old").DeletedAt == "" {
		t.Fatal("trashed host was restored out of trash")
	}
	if h := d.Host("h-desk"); h.Kind == nil || *h.Kind != "rdp" || h.RdpPort == nil || *h.RdpPort != 3389 {
		t.Fatalf("rdp host = %+v", h)
	}

	// Secret scan: no password or key in readable, obf: or base64 form.
	for name, b := range storeFiles(t, dir) {
		for _, pw := range append(mapValues(fixturePasswords), fixtureKey, "hostbay-fixture-deploy-key") {
			for _, form := range []string{pw, obfuscate(pw), base64.StdEncoding.EncodeToString([]byte(pw))} {
				if bytes.Contains(b, []byte(form)) {
					t.Fatalf("%s contains a secret in the form %q", name, form)
				}
			}
		}
	}
}

func TestRestoreKeepsLaterEditAndAddsNothingTwice(t *testing.T) {
	st, v, dir := newEnvDir(t)
	restore(t, st, v, readFixture(t))
	err := st.Edit(func(d *store.Data) error {
		return d.EditHost("h-web-1", func(h *store.Host) { h.Alias = "web-edited" })
	})
	if err != nil {
		t.Fatal(err)
	}
	before := storeFiles(t, dir)

	if n := restore(t, st, v, readFixture(t)); n != 0 {
		t.Fatalf("second restore added %d hosts, want 0", n)
	}
	after := storeFiles(t, dir)
	for name, b := range before {
		if !bytes.Equal(after[name], b) {
			t.Errorf("%s changed on the second restore", name)
		}
	}
	d := data(t, st)
	if h := d.Host("h-web-1"); h.Alias != "web-edited" || h.Revision != 2 {
		t.Fatalf("edited host = %+v, want the edit kept at revision 2", h)
	}
	if len(d.Hosts.Items) != 5 || len(d.Folders.Items) != 2 || len(d.Keys.Items) != 1 || len(d.Scripts.Items) != 1 {
		t.Fatalf("duplicates: %d hosts, %d folders, %d keys, %d scripts", len(d.Hosts.Items), len(d.Folders.Items), len(d.Keys.Items), len(d.Scripts.Items))
	}
}

func TestRestoreMatchesHostByAddressWhenIDsDiffer(t *testing.T) {
	st, v := newEnv(t)
	err := st.Edit(func(d *store.Data) error {
		d.AddHost(store.Host{Alias: "web", Hostname: "10.0.0.1", Port: 22, User: "root"})
		d.AddFolder(store.Folder{Name: "Prod"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := restore(t, st, v, readFixture(t)); n != 4 {
		t.Fatalf("restored %d hosts, want 4 (web@10.0.0.1 already saved)", n)
	}
	d := data(t, st)
	if d.Host("h-web-1") != nil {
		t.Fatal("matching local host was duplicated")
	}
	if len(d.Folders.Items) != 2 {
		t.Fatalf("folders = %+v, want Prod reused and DB added", d.Folders.Items)
	}
}

func mapValues(m map[string]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// obfuscate is SSH Manager's XOR+base64 form, for the secret scan.
func obfuscate(plain string) string {
	b := []byte(plain)
	for i := range b {
		b[i] ^= obfKey[i%len(obfKey)]
	}
	return "obf:" + base64.StdEncoding.EncodeToString(b)
}
