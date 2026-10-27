package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"hostbay/internal/update"
)

// fakeUpdates replaces the network and the installer behind an UpdateService.
type fakeUpdates struct {
	release           *update.Release
	err               error
	checks, downloads int
	installed, quits  int
}

func newUpdateService(dir string, f *fakeUpdates) *UpdateService {
	s := NewUpdateService(dir)
	s.status.Signed = true // the background check only runs for signed builds
	s.check = func(context.Context, bool) (*update.Release, error) {
		f.checks++
		return f.release, f.err
	}
	s.download = func(_ context.Context, _ *update.Release, dir string) (string, error) {
		f.downloads++
		p := filepath.Join(dir, "asset")
		return p, os.WriteFile(p, []byte("x"), 0o600)
	}
	s.install = func(path string, _ update.Asset) error {
		f.installed++
		return nil
	}
	s.quit = func() { f.quits++ }
	return s
}

func TestUpdateAutoCheckRunsAtMostDaily(t *testing.T) {
	f := &fakeUpdates{}
	dir := t.TempDir()
	s := newUpdateService(dir, f)
	s.autoCheck(context.Background())
	s.autoCheck(context.Background())
	if f.checks != 1 {
		t.Fatalf("two background checks in a day made %d requests", f.checks)
	}
	s = newUpdateService(dir, f) // a restart
	s.autoCheck(context.Background())
	if f.checks != 1 {
		t.Fatalf("a restart within a day checked again (%d)", f.checks)
	}
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, updateCheckedFile), old, old); err != nil {
		t.Fatal(err)
	}
	s.autoCheck(context.Background())
	if f.checks != 2 {
		t.Fatalf("a day later: %d checks, want 2", f.checks)
	}
}

// An unsigned build cannot install anything, so the background check
// never runs and About shows no "last checked" time.
func TestUpdateUnsignedBuildNeverChecksInTheBackground(t *testing.T) {
	dir := t.TempDir()
	f := &fakeUpdates{}
	s := newUpdateService(dir, f)
	s.status.Signed = false
	s.autoCheck(context.Background())
	if f.checks != 0 {
		t.Fatalf("unsigned build checked %d times", f.checks)
	}
	if st := s.Status(); st.LastCheck != "" {
		t.Fatalf("LastCheck = %q, want none", st.LastCheck)
	}
}

func TestUpdateWithoutAKeyNeverDownloads(t *testing.T) {
	f := &fakeUpdates{err: update.ErrNotSigned}
	s := newUpdateService(t.TempDir(), f)
	st, err := s.Check(context.Background(), false)
	if !errors.Is(err, update.ErrNotSigned) || st.Error != "updates are not signed yet" || st.Available != "" {
		t.Fatalf("Check = %+v, %v", st, err)
	}
	if _, err := s.Install(context.Background()); err == nil || f.downloads != 0 {
		t.Fatalf("Install = %v after %d downloads", err, f.downloads)
	}
}

func TestUpdateInstallsTheCheckedRelease(t *testing.T) {
	asset := &update.Asset{Name: "Hostbay-9.0.0-macos-universal.zip", OS: "darwin", Arch: "universal"}
	f := &fakeUpdates{release: &update.Release{Version: "9.0.0", PageURL: "https://github.com/x/releases/tag/v9.0.0", Asset: asset}}
	s := newUpdateService(t.TempDir(), f)
	st, err := s.Check(context.Background(), false)
	if err != nil || st.Available != "9.0.0" || !st.CanInstall || st.PageURL == "" || st.LastCheck == "" {
		t.Fatalf("Check = %+v, %v", st, err)
	}
	st, err = s.Install(context.Background())
	if err != nil || f.installed != 1 || !st.Installed {
		t.Fatalf("Install = %+v, %v (installed %d)", st, err, f.installed)
	}
	// Only Windows quits, so its installer can replace the running app.
	if (runtime.GOOS == "windows") != (f.quits == 1) {
		t.Fatalf("quit %d times on %s", f.quits, runtime.GOOS)
	}
}

func TestUpdateForADebInstallOnlyLinksTheRelease(t *testing.T) {
	f := &fakeUpdates{release: &update.Release{Version: "9.0.0", PageURL: "https://github.com/x/releases/tag/v9.0.0"}}
	s := newUpdateService(t.TempDir(), f)
	st, err := s.Check(context.Background(), false)
	if err != nil || st.Available != "9.0.0" || st.CanInstall || st.PageURL == "" {
		t.Fatalf("Check = %+v, %v", st, err)
	}
	if _, err := s.Install(context.Background()); err == nil || f.downloads != 0 {
		t.Fatalf("Install = %v after %d downloads", err, f.downloads)
	}
}
