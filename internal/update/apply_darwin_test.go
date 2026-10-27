package update

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// fakeBundle writes dir/Hostbay.app with an app binary and RDP helper
// carrying body.
func fakeBundle(t *testing.T, dir, body string) string {
	t.Helper()
	app := filepath.Join(dir, "Hostbay.app")
	macos := filepath.Join(app, "Contents", "MacOS")
	if err := os.MkdirAll(macos, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Hostbay", "hostbay-rdp-helper"} {
		if err := os.WriteFile(filepath.Join(macos, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return app
}

// zipBundle packs a bundle the way the release workflow does.
func zipBundle(t *testing.T, app string) string {
	t.Helper()
	zip := filepath.Join(t.TempDir(), "Hostbay-macos-universal.zip")
	if out, err := exec.Command("ditto", "-c", "-k", "--keepParent", app, zip).CombinedOutput(); err != nil {
		t.Fatalf("ditto: %v %s", err, out)
	}
	return zip
}

func TestSwapBundleReplacesTheWholeApp(t *testing.T) {
	apps := t.TempDir()
	installed := fakeBundle(t, apps, "old")
	// A file only the old bundle has must not survive the swap.
	stale := filepath.Join(installed, "Contents", "Resources", "stale")
	os.MkdirAll(filepath.Dir(stale), 0o755)
	os.WriteFile(stale, nil, 0o644)
	zip := zipBundle(t, fakeBundle(t, t.TempDir(), "new"))

	if err := swapBundle(zip, installed); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Hostbay", "hostbay-rdp-helper"} {
		p := filepath.Join(installed, "Contents", "MacOS", name)
		b, err := os.ReadFile(p)
		if err != nil || string(b) != "new" {
			t.Fatalf("%s = %q, %v", name, b, err)
		}
		if fi, _ := os.Stat(p); fi.Mode()&0o111 == 0 {
			t.Fatalf("%s lost its execute bit", name)
		}
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("a file from the old bundle survived")
	}
	if left, _ := os.ReadDir(apps); len(left) != 1 {
		t.Fatalf("the swap left %d entries beside the bundle", len(left))
	}
}

func TestSwapBundleWithoutAnAppKeepsTheOldOne(t *testing.T) {
	apps := t.TempDir()
	installed := fakeBundle(t, apps, "old")
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "readme"), []byte("x"), 0o644)
	zip := filepath.Join(t.TempDir(), "bad.zip")
	if out, err := exec.Command("ditto", "-c", "-k", src, zip).CombinedOutput(); err != nil {
		t.Fatalf("ditto: %v %s", err, out)
	}
	if err := swapBundle(zip, installed); err == nil {
		t.Fatal("swapped in a zip with no Hostbay.app")
	}
	if b, _ := os.ReadFile(filepath.Join(installed, "Contents", "MacOS", "Hostbay")); string(b) != "old" {
		t.Fatalf("the installed app is now %q", b)
	}
	if left, _ := os.ReadDir(apps); len(left) != 1 {
		t.Fatalf("the failed swap left %d entries beside the bundle", len(left))
	}
}
