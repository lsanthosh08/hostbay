package openconnect

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// The Windows installer's OpenConnect is built by
// build/windows/bundle-openconnect.sh from the files openconnect-packages.txt
// pins; these tests check what can be checked off the Windows runner.

const buildWindows = "../../../build/windows"

// lines returns the non-comment lines of a build/windows list.
func lines(t *testing.T, name string) [][]string {
	t.Helper()
	f, err := os.Open(filepath.Join(buildWindows, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out [][]string
	s := bufio.NewScanner(f)
	for s.Scan() {
		if l := strings.TrimSpace(s.Text()); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, strings.SplitN(l, " ", 2))
		}
	}
	return out
}

// The pinned tarball is a release Hostbay accepts, so the installer never
// ships an openconnect that checkVersion would refuse.
func TestBundledOpenConnectIsNewEnough(t *testing.T) {
	re := regexp.MustCompile(`/openconnect-(\d+)\.(\d+)\.tar\.gz$`)
	var found bool
	for _, l := range lines(t, "openconnect-packages.txt") {
		m := re.FindStringSubmatch(l[0])
		if m == nil {
			continue
		}
		found = true
		major, _ := strconv.Atoi(m[1])
		minor, _ := strconv.Atoi(m[2])
		if major < minMajor || major == minMajor && minor < minMinor {
			t.Errorf("openconnect-packages.txt pins OpenConnect %d.%02d; Hostbay needs %d.%02d or newer", major, minor, minMajor, minMinor)
		}
		if len(l) != 2 || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(l[1]) {
			t.Errorf("the OpenConnect tarball has no SHA-256: %q", l)
		}
	}
	if !found {
		t.Fatal("openconnect-packages.txt pins no OpenConnect tarball")
	}
}

// Every library the coverage gate admits is one NOTICE names where it says
// what the Windows installer ships, so the gate passing means NOTICE covers
// what the installer ships. Many of the names also appear in the macOS text,
// which does not count.
func TestBundledDLLsAreInNotice(t *testing.T) {
	notice, err := os.ReadFile("../../../NOTICE")
	if err != nil {
		t.Fatal(err)
	}
	var windows strings.Builder
	for _, p := range strings.Split(strings.ReplaceAll(string(notice), "\r\n", "\n"), "\n\n") {
		if i := strings.Index(p, "The Windows installer"); i >= 0 {
			windows.WriteString(p[i:])
		}
	}
	for _, l := range lines(t, "openconnect-dlls.txt") {
		if len(l) != 2 || l[0] != strings.ToLower(l[0]) {
			t.Errorf("openconnect-dlls.txt: want a lower-case pattern and a name, got %q", l)
			continue
		}
		if !strings.Contains(windows.String(), l[1]) {
			t.Errorf("NOTICE's Windows installer text does not name %s, which openconnect-dlls.txt admits for %s", l[1], l[0])
		}
	}
}

// The gate passes a folder whose DLLs are all covered, whatever their case,
// and fails one with a DLL NOTICE does not cover.
func TestBundleCoverageGate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the gate runs in MSYS2's bash, which go test cannot find reliably on Windows")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	dir := t.TempDir()
	for _, f := range []string{"openconnect.exe", "wintun.dll", "wintun-LICENSE.txt", "libnettle-9.dll", "ZLIB1.DLL", "libxml2-16.dll"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gate := func() (string, error) {
		out, err := exec.Command(bash, filepath.Join(buildWindows, "bundle-openconnect.sh"), "--check", dir).CombinedOutput()
		return string(out), err
	}
	if out, err := gate(); err != nil {
		t.Fatalf("covered DLLs failed the gate: %v\n%s", err, out)
	}
	// p11-kit loads PKCS#11 modules from a folder any user may create, and
	// GnuTLS is linked in without it: neither DLL may ship.
	for _, f := range []string{"libstoken-1.dll", "libp11-kit-0.dll", "libgnutls-30.dll"} {
		p := filepath.Join(dir, f)
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := gate(); err == nil || !strings.Contains(out, f) {
			t.Fatalf("%s passed the gate (err %v):\n%s", f, err, out)
		}
		os.Remove(p)
	}
}
