package update

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReplaceFileSwapsTheAppImage(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "Hostbay.AppImage")
	os.WriteFile(dst, []byte("old"), 0o755)
	src := filepath.Join(t.TempDir(), "new.AppImage")
	os.WriteFile(src, []byte("new"), 0o600)
	if err := replaceFile(src, dst); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(dst)
	fi, _ := os.Stat(dst)
	if string(b) != "new" || fi.Mode().Perm() != 0o755 {
		t.Fatalf("AppImage = %q mode %v", b, fi.Mode())
	}
	if left, _ := os.ReadDir(dir); len(left) != 1 {
		t.Fatalf("left %d entries beside the AppImage", len(left))
	}
}

func TestInstallOutsideAnAppImageRefuses(t *testing.T) {
	t.Setenv("APPIMAGE", "")
	if err := Install(filepath.Join(t.TempDir(), "x"), Asset{}); err == nil {
		t.Fatal("a deb or rpm install replaced something")
	}
}
