package vpnconf

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestScanListsConfigsWithoutReadingUnreadableOnes(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string, mode os.FileMode) string {
		p := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ovpn := write("tb/Office.tblk/Contents/Resources/config.ovpn", "remote x 1194\n", 0o600)
	write("nm/wifi.nmconnection", "[connection]\ntype=wifi\n", 0o600)
	vpn := write("nm/corp.nmconnection", "[connection]\ntype=vpn\n", 0o600)
	locked := write("nm/locked.nmconnection", "[connection]\ntype=vpn\n", 0o000)
	got := scan([]location{
		{Client: "Tunnelblick", Dir: filepath.Join(root, "tb"), Match: matchExt(".tblk")},
		{Client: "NetworkManager", Dir: filepath.Join(root, "nm"), Match: nmVPN},
		{Client: "Missing", Dir: filepath.Join(root, "nope"), Match: matchExt(".ovpn")},
	})
	want := map[string]bool{filepath.Dir(filepath.Dir(filepath.Dir(ovpn))): true, vpn: true}
	// Root and Windows read a mode-000 file anyway.
	want[locked] = runtime.GOOS == "windows" || os.Geteuid() == 0
	if len(got) != len(want) {
		t.Fatalf("found %+v", got)
	}
	for _, f := range got {
		if r, ok := want[f.Path]; !ok || r != f.Readable {
			t.Errorf("unexpected %+v", f)
		}
	}
}
