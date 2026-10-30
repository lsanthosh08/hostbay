package netd

import (
	"errors"
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
