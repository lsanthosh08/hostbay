package mcp

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The data folder, ~/.ssh, a folder inside either and a parent of either
// are refused as the MCP folder; a sibling is fine.
func TestCheckFolder(t *testing.T) {
	base := t.TempDir()
	data := filepath.Join(base, "home", "Hostbay")
	ssh := filepath.Join(base, "home", ".ssh")
	for _, d := range []string{data, ssh, filepath.Join(data, "sub"), filepath.Join(base, "mcp")} {
		os.MkdirAll(d, 0o700)
	}
	for _, refused := range []string{data, ssh, filepath.Join(data, "sub"), filepath.Join(base, "home"), base} {
		if err := CheckFolder(refused, data, ssh); err == nil {
			t.Errorf("%s accepted", refused)
		}
	}
	if err := CheckFolder(filepath.Join(base, "mcp"), data, ssh); err != nil {
		t.Errorf("sibling refused: %v", err)
	}
	link := filepath.Join(base, "link")
	if os.Symlink(ssh, link) == nil {
		if err := CheckFolder(link, data, ssh); err == nil {
			t.Error("a symlink to ~/.ssh was accepted")
		}
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		if err := CheckFolder(strings.ToUpper(data[:1])+data[1:], data, strings.ToUpper(ssh)); err == nil {
			t.Error("a different case of the data folder was accepted")
		}
	}
}

// A client that can write in the MCP folder may swap a folder on a
// download's path for a link elsewhere before the download fails. The
// cleanup must stay inside the MCP folder and leave the link's target alone.
func TestDiscardedDownloadNeverLeavesTheFolder(t *testing.T) {
	folder, outside := t.TempDir(), t.TempDir()
	w, discard, err := createLocal(folder, "plain")
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	discard()
	if _, err := os.Stat(filepath.Join(folder, "plain")); err == nil {
		t.Fatal("discard left the failed download")
	}
	if err := os.Mkdir(filepath.Join(folder, "d"), 0o700); err != nil {
		t.Fatal(err)
	}
	w, discard, err = createLocal(folder, filepath.Join("d", "known_hosts"))
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	victim := filepath.Join(outside, "known_hosts")
	if err := os.WriteFile(victim, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(folder, "d")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(folder, "d")); err != nil {
		t.Skip("no symlinks here:", err)
	}
	discard()
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("discard removed a file outside the MCP folder: %v", err)
	}
}

func TestLocalName(t *testing.T) {
	for _, bad := range []string{"", "../x", "a/../../x", "/etc/passwd", `C:\x`} {
		if _, err := localName(bad); err == nil && !(runtime.GOOS != "windows" && bad == `C:\x`) {
			t.Errorf("%q accepted", bad)
		}
	}
	if n, err := localName("out/report.txt"); err != nil || n != filepath.FromSlash("out/report.txt") {
		t.Errorf("good name: %q %v", n, err)
	}
}
