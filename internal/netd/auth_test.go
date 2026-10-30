package netd

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Covers AE5: only the installing user's paired Hostbay gets through.
func TestAuthorize(t *testing.T) {
	cfg := Config{UID: "501", App: "/Applications/Hostbay.app/Contents/MacOS/hostbay"}
	if err := authorize(Peer{UID: "501", Exe: cfg.App}, cfg); err != nil {
		t.Errorf("the paired Hostbay was refused: %v", err)
	}
	refused := map[string]Peer{
		"other user":       {UID: "502", Exe: cfg.App},
		"root":             {UID: "0", Exe: cfg.App},
		"other program":    {UID: "501", Exe: "/usr/bin/python3"},
		"copy elsewhere":   {UID: "501", Exe: "/Users/me/Downloads/Hostbay.app/Contents/MacOS/hostbay"},
		"unknown exe":      {UID: "501"},
		"path with suffix": {UID: "501", Exe: cfg.App + " (deleted)"},
	}
	for name, p := range refused {
		if err := authorize(p, cfg); errCode(err) != CodeDenied {
			t.Errorf("%s: authorize = %v, want %s", name, err, CodeDenied)
		}
	}
	if err := authorize(Peer{UID: "501", Exe: cfg.App}, Config{}); errCode(err) != CodeDenied {
		t.Errorf("an empty config admitted a caller: %v", err)
	}
}

// Root cannot be had in a test, so the check runs against the test's own
// user standing in for root, and against a user the file does not belong to.
func TestCheckOwned(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows relies on the Program Files ACL")
	}
	me := os.Getuid()
	dir := t.TempDir()
	os.Chmod(dir, 0o755)
	bin := filepath.Join(dir, "openvpn")
	os.WriteFile(bin, []byte("#!"), 0o755)

	if err := checkOwned(bin, me); err != nil {
		t.Fatalf("a %d-owned 0755 file was refused: %v", me, err)
	}
	if err := checkOwned(bin, me+1); err == nil {
		t.Error("a file owned by another user passed")
	}
	os.Chmod(bin, 0o757)
	if err := checkOwned(bin, me); err == nil {
		t.Error("a world-writable file passed")
	}
	os.Chmod(bin, 0o775)
	if err := checkOwned(bin, me); err == nil {
		t.Error("a group-writable file passed")
	}
	os.Chmod(bin, 0o755)
	os.Chmod(dir, 0o777)
	if err := checkOwned(bin, me); err == nil {
		t.Error("a file in a world-writable directory passed")
	}
	os.Chmod(dir, 0o755)
	link := filepath.Join(dir, "link")
	os.Symlink(bin, link)
	if err := checkOwned(link, me); err == nil {
		t.Error("a symlink passed")
	}
}
