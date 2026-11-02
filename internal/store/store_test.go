package store

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

// testHost mirrors the shape of SSH Manager's Host: camelCase JSON plus sync metadata.
type testHost struct {
	Meta
	Alias    string   `json:"alias"`
	Hostname string   `json:"hostname"`
	Port     int      `json:"port"`
	Username string   `json:"username"`
	FolderID *string  `json:"folderId"`
	Tags     []string `json:"tags"`
}

func openT(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func readT(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSavedHostsReadBackAfterReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Hostbay")
	s := openT(t, dir)
	folder := "f1"
	h := testHost{Meta: Meta{ID: "h1"}, Alias: "web", Hostname: "10.0.0.5", Port: 2222, Username: "ops", FolderID: &folder, Tags: []string{"prod"}}
	s.Touch(&h.Meta)
	if err := Save(s, "hosts.json", []testHost{h}); err != nil {
		t.Fatal(err)
	}
	deviceID := s.DeviceID()
	s.Close()

	s = openT(t, dir)
	defer s.Close()
	got, err := Load[[]testHost](s, "hosts.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].ModifiedAt.Equal(h.ModifiedAt) {
		t.Fatalf("got %+v, want %+v", got, h)
	}
	got[0].ModifiedAt = h.ModifiedAt // drop the monotonic clock reading before DeepEqual
	if !reflect.DeepEqual(got[0], h) {
		t.Fatalf("got %+v, want %+v", got[0], h)
	}
	if h.Revision != 1 || h.DeviceID != deviceID || h.ModifiedAt.IsZero() {
		t.Fatalf("Touch set %+v, want revision 1 by %s", h.Meta, deviceID)
	}
	raw := readT(t, filepath.Join(dir, "hosts.json"))
	for _, want := range []string{`"schemaVersion": 4`, `"folderId": "f1"`, `"deviceId": "` + deviceID + `"`, `"modifiedAt"`, `"revision": 1`} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("hosts.json lacks %s:\n%s", want, raw)
		}
	}
}

func TestMissingFileLoadsAsZero(t *testing.T) {
	s := openT(t, t.TempDir())
	defer s.Close()
	got, err := Load[[]testHost](s, "hosts.json")
	if err != nil || got != nil {
		t.Fatalf("got %v, %v; want nil, nil", got, err)
	}
}

func TestCrashBeforeRenameKeepsPreviousFile(t *testing.T) {
	dir := t.TempDir()
	s := openT(t, dir)
	if err := Save(s, "hosts.json", []testHost{{Alias: "old"}}); err != nil {
		t.Fatal(err)
	}
	before := readT(t, filepath.Join(dir, "hosts.json"))
	s.Close()
	// What a crash after the temp write but before the rename leaves behind.
	if err := os.WriteFile(filepath.Join(dir, ".hosts.json.tmp-123"), []byte(`{"schemaVersion": 1, "data": [{"alias": "ne`), 0o600); err != nil {
		t.Fatal(err)
	}

	s = openT(t, dir)
	defer s.Close()
	got, err := Load[[]testHost](s, "hosts.json")
	if err != nil || len(got) != 1 || got[0].Alias != "old" {
		t.Fatalf("got %+v, %v; want the old host", got, err)
	}
	if !bytes.Equal(readT(t, filepath.Join(dir, "hosts.json")), before) {
		t.Fatal("hosts.json changed")
	}
}

func TestNewerSchemaIsRefusedAndNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	s := openT(t, dir)
	defer s.Close()
	path := filepath.Join(dir, "hosts.json")
	newer := []byte(fmt.Sprintf(`{"schemaVersion": %d, "data": {"hosts": []}}`, SchemaVersion+1))
	if err := os.WriteFile(path, newer, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Load[[]testHost](s, "hosts.json"); !errors.Is(err, ErrNewerSchema) {
		t.Fatalf("Load err = %v, want ErrNewerSchema", err)
	}
	if err := Save(s, "hosts.json", []testHost{{Alias: "x"}}); !errors.Is(err, ErrNewerSchema) {
		t.Fatalf("Save err = %v, want ErrNewerSchema", err)
	}
	if !bytes.Equal(readT(t, path), newer) {
		t.Fatal("newer file was overwritten")
	}
}

// Release 2 keeps proxies and new host settings, which a Release 1 build
// would drop when it rewrites the files, and layout 4 keeps VPN profiles
// and host VPN bindings, which a layout 3 build would drop. Opening an older
// store backs it up and stamps it the current layout, which the older build
// refuses.
func TestOpenUpgradesOlderLayoutsAndKeepsABackup(t *testing.T) {
	for _, from := range []int{1, 3} {
		dir := t.TempDir()
		old := []byte(fmt.Sprintf(`{"schemaVersion": %d, "data": [{"alias": "web"}]}`, from))
		if err := os.WriteFile(filepath.Join(dir, "hosts.json"), old, 0o600); err != nil {
			t.Fatal(err)
		}
		s := openT(t, dir)
		if raw := readT(t, filepath.Join(dir, "hosts.json")); !bytes.Contains(raw, []byte(`"schemaVersion": 4`)) {
			t.Fatalf("layout %d: hosts.json not upgraded:\n%s", from, raw)
		}
		if got, err := Load[[]testHost](s, "hosts.json"); err != nil || len(got) != 1 || got[0].Alias != "web" {
			t.Fatalf("layout %d: got %+v, %v; want the host kept", from, got, err)
		}
		backups, _ := filepath.Glob(filepath.Join(dir, "backups", fmt.Sprintf("v%d-*", from), "hosts.json"))
		if len(backups) != 1 || !bytes.Equal(readT(t, backups[0]), old) {
			t.Fatalf("backup copies = %v, want one copy of the layout %d hosts.json", backups, from)
		}
		s.Close()
	}
}

func TestFailedMigrationRestoresFilesAndKeepsBackup(t *testing.T) {
	dir := t.TempDir()
	s := openT(t, dir)
	defer s.Close()
	if err := Save(s, "hosts.json", []testHost{{Alias: "web"}}); err != nil {
		t.Fatal(err)
	}
	before := readT(t, filepath.Join(dir, "hosts.json"))

	boom := errors.New("boom")
	err := s.Migrate(1, func() error {
		if err := Save(s, "hosts.json", []testHost{{Alias: "half-migrated"}}); err != nil {
			return err
		}
		if err := Save(s, "folders.json", []string{"new"}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Migrate err = %v, want boom", err)
	}
	if !bytes.Equal(readT(t, filepath.Join(dir, "hosts.json")), before) {
		t.Fatal("hosts.json not restored")
	}
	if _, err := os.Stat(filepath.Join(dir, "folders.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("folders.json written by the failed migration still exists: %v", err)
	}
	backups, _ := filepath.Glob(filepath.Join(dir, "backups", "v1-*", "hosts.json"))
	if len(backups) != 1 || !bytes.Equal(readT(t, backups[0]), before) {
		t.Fatalf("backup copies = %v, want one copy of the pre-migration hosts.json", backups)
	}
}

func TestDirAndFileModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no Unix permission bits")
	}
	dir := filepath.Join(t.TempDir(), "Hostbay")
	s := openT(t, dir)
	defer s.Close()
	if err := Save(s, "hosts.json", []testHost{}); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{
		dir:                              0o700,
		filepath.Join(dir, "hosts.json"): 0o600,
		filepath.Join(dir, "state.json"): 0o600,
	} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s mode %o, want %o", path, fi.Mode().Perm(), want)
		}
	}
}

func TestDeviceIDAndImportMarkerPersist(t *testing.T) {
	dir := t.TempDir()
	s := openT(t, dir)
	id := s.DeviceID()
	if id == "" {
		t.Fatal("empty device ID")
	}
	if s.SettingsImported() {
		t.Fatal("fresh store reports an import")
	}
	if err := s.MarkSettingsImported(); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s = openT(t, dir)
	defer s.Close()
	if s.DeviceID() != id {
		t.Fatalf("device ID %q after reopen, want %q", s.DeviceID(), id)
	}
	if !s.SettingsImported() {
		t.Fatal("import marker lost after reopen")
	}
}
