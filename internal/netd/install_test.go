package netd

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

// fakeRunner records the one command it is asked to run and answers as told.
type fakeRunner struct {
	argv   []string
	code   int
	stderr string
	err    error
}

func (f *fakeRunner) install(t *testing.T) {
	t.Helper()
	old := run
	run = func(name string, args ...string) (int, string, error) {
		f.argv = append([]string{name}, args...)
		return f.code, f.stderr, f.err
	}
	t.Cleanup(func() { run = old })
}

const helperPath = "/Applications/Hostbay.app/Contents/MacOS/hostbay-netd"

func TestDarwinInstallScript(t *testing.T) {
	f := &fakeRunner{}
	f.install(t)
	if err := installDarwin(helperPath, 501); err != nil {
		t.Fatal(err)
	}
	want := `do shell script "'` + helperPath + `' install 501" with prompt "` + installPrompt + `" with administrator privileges`
	if !slices.Equal(f.argv, []string{"/usr/bin/osascript", "-e", want}) {
		t.Errorf("argv = %q\nwant %q", f.argv, want)
	}

	for _, p := range []string{`/Apps/Bob's/hostbay-netd`, `/Apps/a"b/hostbay-netd`, `/Apps/a\b/hostbay-netd`, "/Apps/a\nb/hostbay-netd", "relative/hostbay-netd"} {
		f.argv = nil
		if err := installDarwin(p, 501); err == nil || f.argv != nil {
			t.Errorf("%q: err %v, ran %q", p, err, f.argv)
		}
	}
}

func TestDarwinUninstallScript(t *testing.T) {
	f := &fakeRunner{}
	f.install(t)
	if err := uninstallDarwin(); err != nil {
		t.Fatal(err)
	}
	want := `do shell script "'` + darwinDir + `/hostbay-netd' uninstall" with prompt "` + uninstallPrompt + `" with administrator privileges`
	if !slices.Equal(f.argv, []string{"/usr/bin/osascript", "-e", want}) {
		t.Errorf("argv = %q\nwant %q", f.argv, want)
	}
}

func TestInstallCancelledAtThePrompt(t *testing.T) {
	f := &fakeRunner{code: 1, stderr: "0:180: execution error: User canceled. (-128)\n"}
	f.install(t)
	if err := installDarwin(helperPath, 501); errCode(err) != CodeCancelled {
		t.Errorf("macOS: %v, want %s", err, CodeCancelled)
	}

	f.code, f.stderr = 126, "Error executing command as another user: Request dismissed"
	if err := installLinux("/usr/local/bin/hostbay-netd"); errCode(err) != CodeCancelled {
		t.Errorf("Linux: %v, want %s", err, CodeCancelled)
	}
	if !slices.Equal(f.argv, []string{"/usr/bin/pkexec", "/usr/local/bin/hostbay-netd", "install"}) {
		t.Errorf("Linux argv = %q", f.argv)
	}

	old := elevate
	var argv []string
	elevate = func(file string, args ...string) (int, error) {
		argv = append([]string{file}, args...)
		return 0, errElevationCancelled
	}
	defer func() { elevate = old }()
	const sid = "S-1-5-21-1004336348-1177238915-682003330-1001"
	if err := installWindows(`C:\Program Files\Hostbay\hostbay-netd.exe`, sid); errCode(err) != CodeCancelled {
		t.Errorf("Windows: %v, want %s", err, CodeCancelled)
	}
	if !slices.Equal(argv, []string{`C:\Program Files\Hostbay\hostbay-netd.exe`, "install", sid}) {
		t.Errorf("Windows argv = %q", argv)
	}

	// A failure that is not a cancel stays a failure.
	f.code, f.stderr = 1, "launchctl: Bootstrap failed"
	if err := installDarwin(helperPath, 501); err == nil || errCode(err) == CodeCancelled {
		t.Errorf("a failed install = %v", err)
	}
}

func TestLinuxInstallRefusedFromAppImage(t *testing.T) {
	f := &fakeRunner{}
	f.install(t)
	t.Setenv("APPIMAGE", "/home/me/Hostbay-x86_64.AppImage")
	if err := installLinux("/tmp/.mount_Hostba/usr/bin/hostbay-netd"); errCode(err) != CodeUnavailable || f.argv != nil {
		t.Errorf("err %v, ran %q", err, f.argv)
	}
}

func TestWindowsInstallRefusesBadSID(t *testing.T) {
	old := elevate
	elevate = func(string, ...string) (int, error) { return 0, errors.New("ran") }
	defer func() { elevate = old }()
	if err := installWindows(`C:\Hostbay\hostbay-netd.exe`, "S-1-5 & calc"); errCode(err) != CodeInvalid {
		t.Errorf("err = %v", err)
	}
}

// Install copies each VPN client beside Hostbay with its libraries: on
// macOS into bin beside Frameworks, the layout the bundled install names
// (@executable_path/../Frameworks) expect, on Windows beside the helper.
func TestCopyClientsKeepsTheirLibraries(t *testing.T) {
	for goos, want := range map[string][]string{
		"darwin":  {"bin/openvpn", "bin/openconnect", "Frameworks/libgnutls.30.dylib"},
		"windows": {"openvpn.exe", "openconnect.exe", "wintun.dll", "libopenconnect-5.dll"},
		"linux":   {"openvpn", "openconnect"},
	} {
		app := t.TempDir()
		contents := filepath.Join(app, "Contents")
		appDir := filepath.Join(contents, "MacOS")
		os.MkdirAll(filepath.Join(contents, "Frameworks"), 0o755)
		os.MkdirAll(appDir, 0o755)
		for _, f := range []string{"MacOS/openvpn", "MacOS/openconnect", "MacOS/openvpn.exe", "MacOS/openconnect.exe", "MacOS/wintun.dll",
			"MacOS/libopenconnect-5.dll", "Frameworks/libgnutls.30.dylib", "Frameworks/README"} {
			os.WriteFile(filepath.Join(contents, f), []byte(f), 0o644)
		}
		dir := t.TempDir()
		os.MkdirAll(filepath.Join(dir, "Frameworks"), 0o755)
		os.WriteFile(filepath.Join(dir, "Frameworks", "libstale.dylib"), nil, 0o644)
		if err := copyClients(dir, appDir, goos); err != nil {
			t.Fatal(err)
		}
		for _, f := range want {
			if b, err := os.ReadFile(filepath.Join(dir, f)); err != nil || len(b) == 0 {
				t.Errorf("%s: %s not copied: %v", goos, f, err)
			}
		}
		if goos == "darwin" {
			if _, err := os.Stat(filepath.Join(dir, "Frameworks", "libstale.dylib")); err == nil {
				t.Error("a reinstall kept a library the new Hostbay no longer ships")
			}
			if _, err := os.Stat(filepath.Join(dir, "Frameworks", "README")); err == nil {
				t.Error("a file that is no library was copied")
			}
		}
	}
}

// A client whose copy or any library beside it another user can replace is
// never started.
func TestCheckClientChecksItsLibraries(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0o755)
	app := filepath.Join(t.TempDir(), "MacOS")
	os.MkdirAll(app, 0o755)
	os.MkdirAll(filepath.Join(app, "..", "Frameworks"), 0o755)
	os.WriteFile(filepath.Join(app, exeName("openvpn")), []byte("x"), 0o755)
	lib := "libssl.3.dylib"
	if runtime.GOOS == "windows" {
		lib = "libssl-3.dll"
	}
	os.WriteFile(filepath.Join(app, "..", "Frameworks", lib), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(app, lib), []byte("x"), 0o644)
	if err := copyClients(dir, app, runtime.GOOS); err != nil {
		t.Fatal(err)
	}
	me := os.Getuid()
	if err := CheckClient(dir, "openvpn", me); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		return // mode bits mean nothing there
	}
	if err := CheckClient(dir, "openvpn", me+1); err == nil {
		t.Error("a copy owned by someone else passed")
	}
	if src, libs, _ := libDir(dir, app, runtime.GOOS); src != "" {
		os.Chmod(filepath.Join(libs, lib), 0o666)
		if err := CheckClient(dir, "openvpn", me); err == nil {
			t.Error("a library anyone can write passed")
		}
	}
}
