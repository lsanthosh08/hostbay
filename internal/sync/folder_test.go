package sync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	gosync "sync"
	"testing"

	"hostbay/internal/store"
)

func TestFolderNeverOverwritesAFile(t *testing.T) {
	f := NewFolder(t.TempDir())
	ctx := context.Background()
	if err := f.Store(ctx, deviceFileName("dev", 1), []byte("one"), ""); err != nil {
		t.Fatal(err)
	}
	if err := f.Store(ctx, deviceFileName("dev", 1), []byte("two"), ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("second write = %v, want ErrConflict", err)
	}
	files, _, err := f.Fetch(ctx)
	if err != nil || len(files) != 1 || string(files[0].Data) != "one" {
		t.Fatalf("Fetch = %+v, %v", files, err)
	}
}

func TestFolderFetchesTheLatestFilePerDeviceAndPrunesOlder(t *testing.T) {
	dir := t.TempDir()
	f := NewFolder(dir)
	ctx := context.Background()
	for _, n := range []string{deviceFileName("a", 1), deviceFileName("a", 2), deviceFileName("b", 7)} {
		if err := f.Store(ctx, n, []byte(n), ""); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not ours"), 0o600)
	files, v1, err := f.Fetch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, file := range files {
		got[file.Name] = true
	}
	if len(got) != 2 || !got[deviceFileName("a", 2)] || !got[deviceFileName("b", 7)] {
		t.Fatalf("Fetch returned %v", got)
	}
	if err := f.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, deviceFileName("a", 1))); !os.IsNotExist(err) {
		t.Fatal("superseded file was kept")
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.txt")); err != nil {
		t.Fatal("a file that is not Hostbay's was removed")
	}
	if v2, _ := f.Version(ctx); v2 != v1 {
		t.Fatal("pruning a superseded file changed the version")
	}
}

func folderDevices(t *testing.T) (*device, *device, string) {
	dir := t.TempDir()
	return newDevice(t, NewFolder(dir)), newDevice(t, NewFolder(dir)), dir
}

// Two computers writing to one Dropbox-style folder in the same window, with
// no working lock, both keep their changes.
func TestFolderSharedByTwoDevicesWithoutALockKeepsBothChanges(t *testing.T) {
	a, b, _ := folderDevices(t)
	a.sync(t)
	b.sync(t)
	ida, idb := a.addHost(t, "from-a", ""), b.addHost(t, "from-b", "")
	var wg gosync.WaitGroup
	errs := make([]error, 2)
	for i, d := range []*device{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = d.eng.Sync(context.Background(), "")
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	a.sync(t)
	b.sync(t)
	for _, d := range []*device{a, b} {
		got := aliases(t, d)
		if got[ida] != "from-a" || got[idb] != "from-b" {
			t.Fatalf("got %v, want both hosts", got)
		}
	}
}

func TestTwoFolderStoresConvergeAfterAlternatingEdits(t *testing.T) {
	a, b, dir := folderDevices(t)
	id := a.addHost(t, "v0", "pw")
	a.sync(t)
	b.sync(t)
	for i, d := range []*device{b, a, b, a} {
		alias := "v" + string(rune('1'+i))
		d.edit(t, func(data *store.Data) error { return data.EditHost(id, func(h *store.Host) { h.Alias = alias }) })
		d.sync(t)
		other := a
		if d == a {
			other = b
		}
		other.sync(t)
		if aliases(t, a)[id] != alias || aliases(t, b)[id] != alias {
			t.Fatalf("round %d: A has %q, B has %q, want %q", i, aliases(t, a)[id], aliases(t, b)[id], alias)
		}
	}
	if pw, err := b.password(t, id); pw != "pw" || err != nil {
		t.Fatalf("B's password = %q, %v", pw, err)
	}
	entries, _ := os.ReadDir(dir)
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), deviceFileSuffix) {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("%d snapshot files left; old ones should be pruned to one per device", n)
	}
}
