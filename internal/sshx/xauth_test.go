package sshx

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// xauthEntry is one entry in the .Xauthority format libXau writes: a
// big-endian family, then address, display number, auth name and data,
// each a big-endian length and its bytes.
func xauthEntry(family uint16, addr, number, name string, data []byte) []byte {
	b := binary.BigEndian.AppendUint16(nil, family)
	for _, f := range [][]byte{[]byte(addr), []byte(number), []byte(name), data} {
		b = binary.BigEndian.AppendUint16(b, uint16(len(f)))
		b = append(b, f...)
	}
	return b
}

func writeXauth(t *testing.T, entries ...[]byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".Xauthority")
	if err := os.WriteFile(p, bytes.Join(entries, nil), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustDisplay(t *testing.T, s, goos string) xDisplay {
	t.Helper()
	d, err := parseDisplay(s, goos)
	if err != nil {
		t.Fatalf("parseDisplay(%q): %v", s, err)
	}
	return d
}

// The parser picks the entry for the display, as xauth list would: the
// local entries for a socket display, by display number, MIT cookies only.
func TestReadCookiePicksTheEntryForTheDisplay(t *testing.T) {
	c0, c1, cWild, cTCP := []byte("cookie-for-disp0"), []byte("cookie-for-disp1"), []byte("cookie-wildcard!"), []byte("cookie-for-10.1!")
	path := writeXauth(t,
		xauthEntry(familyLocal, "mybox", "0", "XDM-AUTHORIZATION-1", []byte("not-mit")),
		xauthEntry(familyLocal, "mybox", "0", x11Proto, c0),
		xauthEntry(familyLocal, "mybox", "1", x11Proto, c1),
		xauthEntry(familyInternet, string([]byte{10, 0, 0, 1}), "10", x11Proto, cTCP),
		xauthEntry(familyWild, "", "", x11Proto, cWild),
	)
	for _, tc := range []struct {
		display string
		want    []byte
	}{
		{":0", c0},
		{"unix:1.0", c1},
		{"localhost:1", c1},
		{"/private/tmp/com.apple.launchd.abc/org.xquartz:0", c0},
		{"10.0.0.1:10", cTCP},
		{":7", cWild},
	} {
		got, err := readCookie(path, mustDisplay(t, tc.display, "linux"))
		if err != nil || !bytes.Equal(got, tc.want) {
			t.Errorf("%s: cookie %q, %v; want %q", tc.display, got, err, tc.want)
		}
	}

	if got, err := readCookie(filepath.Join(t.TempDir(), "none"), mustDisplay(t, ":0", "linux")); got != nil || err != nil {
		t.Fatalf("missing file: %q, %v", got, err)
	}
	cut := writeXauth(t, xauthEntry(familyLocal, "mybox", "0", x11Proto, c0)[:12])
	if _, err := readCookie(cut, mustDisplay(t, ":0", "linux")); err == nil {
		t.Fatal("a truncated file parsed")
	}
}

// Display 10 is TCP port 6010; a bare :N is the local socket, except on
// Windows, whose X servers listen on loopback TCP.
func TestParseDisplay(t *testing.T) {
	for _, tc := range []struct {
		display, goos, network, addr, number string
		screen                               int
	}{
		{"localhost:10.0", "linux", "tcp", "localhost:6010", "10", 0},
		{"10.0.0.1:10.2", "darwin", "tcp", "10.0.0.1:6010", "10", 2},
		{":0", "linux", "unix", "/tmp/.X11-unix/X0", "0", 0},
		{"unix:3.1", "darwin", "unix", "/tmp/.X11-unix/X3", "3", 1},
		{":0", "windows", "tcp", "127.0.0.1:6000", "0", 0},
		{"/private/tmp/com.apple.launchd.abc/org.xquartz:0", "darwin", "unix", "/private/tmp/com.apple.launchd.abc/org.xquartz:0", "0", 0},
	} {
		d := mustDisplay(t, tc.display, tc.goos)
		if d.network != tc.network || d.addr != tc.addr || d.number != tc.number || d.screen != tc.screen {
			t.Errorf("%s on %s: %+v", tc.display, tc.goos, d)
		}
	}
	for _, bad := range []string{"", "nocolon", ":x", ":-1", "host:1.y"} {
		if _, err := parseDisplay(bad, "linux"); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}
