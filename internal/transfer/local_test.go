package transfer_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"hostbay/internal/transfer"
	"hostbay/internal/transfer/sourcetest"
)

// tempDir is a fresh folder in slash form, with symlinks in its path
// resolved so remote and local views of it agree.
func tempDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return filepath.ToSlash(d)
}

func writeFile(t *testing.T, p, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(filepath.FromSlash(p), []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.FromSlash(p), mode); err != nil { // past the umask
		t.Fatal(err)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash(p))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(filepath.FromSlash(dir))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

func TestLocalPassesTheSourceSuite(t *testing.T) {
	sourcetest.Run(t, transfer.Local{}, tempDir(t))
}

func TestLocalWritesOnlyInsideTheDestination(t *testing.T) {
	parent := tempDir(t)
	dir := parent + "/dest"
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, rel := range []string{"../evil", "/abs/evil", "a/../../evil", ""} {
		if w, err := (transfer.Local{}).Create(ctx, dir, rel, 1); err == nil {
			w.Abort()
			t.Errorf("Create accepted %q", rel)
		}
		if err := (transfer.Local{}).Mkdir(ctx, dir, rel); err == nil {
			t.Errorf("Mkdir accepted %q", rel)
		}
	}
	if got := dirNames(t, parent); len(got) != 1 {
		t.Fatalf("parent holds %v", got)
	}
	if got := dirNames(t, dir); len(got) != 0 {
		t.Fatalf("destination holds %v", got)
	}
}

func TestLocalOverwriteFollowsASymlinkAndKeepsTheMode(t *testing.T) {
	dir := tempDir(t)
	writeFile(t, dir+"/run.sh", "old", 0o755)
	if err := os.Symlink("run.sh", filepath.FromSlash(dir+"/link.sh")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	w, err := (transfer.Local{}).Create(context.Background(), dir, "link.sh", 3)
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("new"))
	if err := w.Commit(); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(filepath.FromSlash(dir + "/link.sh")); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("link replaced by a file")
	}
	fi, _ := os.Stat(filepath.FromSlash(dir + "/run.sh"))
	if readFile(t, dir+"/run.sh") != "new" || fi.Mode().Perm() != 0o755 {
		t.Fatalf("target %q mode %v", readFile(t, dir+"/run.sh"), fi.Mode())
	}
}
