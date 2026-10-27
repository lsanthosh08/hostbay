package sync

import (
	"bytes"
	"encoding/json"
	"errors"
	"hostbay/internal/store"
	gosync "sync"
	"testing"
)

const testPassphrase = "correct horse battery staple"

var (
	keyOnce gosync.Once
	testKDF KDF
	testKey []byte
)

// derived returns one KDF and key for the package's tests: Argon2id at the
// production cost is too slow to repeat per test.
func derived(t *testing.T) (KDF, []byte) {
	t.Helper()
	keyOnce.Do(func() {
		var err error
		if testKDF, err = NewKDF(); err == nil {
			testKey, err = testKDF.Derive(testPassphrase)
		}
		if err != nil {
			panic(err)
		}
	})
	return testKDF, testKey
}

func testManifest(t *testing.T) Manifest {
	k, _ := derived(t)
	return Manifest{KeyID: "k1", KDF: k, DeviceID: "dev-a", Generation: 3}
}

func TestSnapshotRoundTripHidesHostNames(t *testing.T) {
	_, key := derived(t)
	plain := []byte(`{"hosts":{"items":[{"alias":"prod-db","hostname":"db.internal.example"}]}}`)
	blob, err := Seal(testManifest(t), plain, key)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"prod-db", "db.internal.example", testPassphrase} {
		if bytes.Contains(blob, []byte(s)) {
			t.Fatalf("snapshot contains %q in readable form", s)
		}
	}
	m, got, err := Open(blob, key)
	if err != nil || !bytes.Equal(got, plain) || m.DeviceID != "dev-a" || m.Generation != 3 {
		t.Fatalf("Open = %+v, %q, %v", m, got, err)
	}
}

func TestSnapshotWrongPassphraseFails(t *testing.T) {
	k, key := derived(t)
	blob, err := Seal(testManifest(t), []byte("{}"), key)
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := k.Derive("not the passphrase at all")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open(blob, wrong); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("Open with wrong key = %v, want ErrDecrypt", err)
	}
}

func TestSnapshotRefusesASwappedManifest(t *testing.T) {
	_, key := derived(t)
	m1, m2 := testManifest(t), testManifest(t)
	m2.Generation = 9
	a, _ := Seal(m1, []byte(`{"a":1}`), key)
	b, _ := Seal(m2, []byte(`{"b":2}`), key)
	// b's manifest in front of a's ciphertext.
	hb, ha := headerLen(t, b), headerLen(t, a)
	swapped := append(append([]byte{}, b[:hb]...), a[ha:]...)
	if _, _, err := Open(swapped, key); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("Open swapped = %v, want ErrDecrypt", err)
	}
}

func headerLen(t *testing.T, blob []byte) int {
	t.Helper()
	n, err := manifestEnd(blob)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSnapshotFromANewerFormatIsRefused(t *testing.T) {
	_, key := derived(t)
	m := testManifest(t)
	m.MinReaderVersion = ReaderVersion + 1
	blob, err := Seal(m, []byte("{}"), key)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open(blob, key); !errors.Is(err, ErrUpdateNeeded) {
		t.Fatalf("Open = %v, want ErrUpdateNeeded", err)
	}
}

func TestManifestKeepsUnknownFields(t *testing.T) {
	_, key := derived(t)
	var m Manifest
	if err := json.Unmarshal([]byte(`{"keyId":"k1","generation":2,"futureField":{"x":1}}`), &m); err != nil {
		t.Fatal(err)
	}
	m.KDF, _ = derived(t)
	blob, err := Seal(m, []byte("{}"), key)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := Open(blob, key)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(got)
	if !bytes.Contains(out, []byte(`"futureField":{"x":1}`)) {
		t.Fatalf("manifest lost an unknown field: %s", out)
	}
}

func TestKDFRefusesWeakParameters(t *testing.T) {
	k, _ := derived(t)
	k.Memory = 1024
	if _, err := k.Derive(testPassphrase); err == nil {
		t.Fatal("derived a key below the Argon2id floor")
	}
}

func TestPassphraseStrength(t *testing.T) {
	if CheckPassphrase("short") == nil || CheckPassphrase("aaaaaaaaaaaaaaaa") == nil {
		t.Fatal("weak passphrase accepted")
	}
	if err := CheckPassphrase(testPassphrase); err != nil {
		t.Fatal(err)
	}
}

// A Release 1 build (reader version 1) would drop file hosts' fields on its
// next edit, and a reader of version 2 the proxies and connection extras, so
// every snapshot written now shuts both out (KTD22). A reader of version 3
// would drop database hosts' fields the same way, and a reader of version 4
// Release 4's settings, so each stops and asks for an update.
func TestSnapshotsShutOutReadersThatWouldDropFileHostFields(t *testing.T) {
	_, key := derived(t)
	blob, err := Seal(testManifest(t), []byte("{}"), key)
	if err != nil {
		t.Fatal(err)
	}
	m, err := ReadManifest(blob)
	if err != nil || m.MinReaderVersion != 5 {
		t.Fatalf("minReaderVersion = %d, %v; a reader of version 4 would accept it", m.MinReaderVersion, err)
	}
	if _, _, err := Open(blob, key); err != nil {
		t.Fatalf("this build cannot read its own snapshot: %v", err)
	}
}

// Device-local settings never travel: local.json's content is not in the
// decrypted snapshot (KTD37).
func TestLocalSettingsNeverReachTheSnapshot(t *testing.T) {
	remote := &memBackend{}
	a := newDevice(t, remote)
	a.addHost(t, "web", "")
	if err := a.st.EditLocal(func(l *store.Local) error {
		l.HostLogging = map[string]bool{"local-only-sentinel": true}
		l.LogFolder = "/log-folder-sentinel"
		l.MCP = store.MCPSettings{Enabled: true, Folder: "/mcp-folder-sentinel",
			Allowlist: map[string]map[string][]store.AllowEntry{"mcp-token-sentinel": {"web": {{Argv: []string{"uptime"}}}}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// MCP token hashes live in their own file in the data folder (KTD35).
	if err := store.Save(a.st, "mcp-tokens.json", []map[string]string{{"name": "mcp-token-sentinel", "hash": "sentinel"}}); err != nil {
		t.Fatal(err)
	}
	if err := a.st.AddHistory("web", "history-sentinel"); err != nil {
		t.Fatal(err)
	}
	a.sync(t)
	_, plain, err := Open(remote.data, a.eng.Key)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(plain, []byte("local-only-sentinel")) || bytes.Contains(plain, []byte("hostLogging")) ||
		bytes.Contains(plain, []byte("sentinel")) {
		t.Fatalf("snapshot carries local settings:\n%s", plain)
	}
	if !bytes.Contains(plain, []byte(`"settings"`)) {
		t.Fatalf("snapshot has no settings collection:\n%s", plain)
	}
}
