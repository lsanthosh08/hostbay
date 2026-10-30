package netd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// Where the helper lives once installed. Each directory is root-owned (on
// Windows, under Program Files) and holds the helper binary, its pairing
// (config.json), its route journal and the VPN client copies it may start.
const (
	helperName = "hostbay-netd"

	darwinDir   = "/Library/PrivilegedHelperTools/dev.hostbay.netd"
	darwinLabel = "dev.hostbay.netd"
	darwinPlist = "/Library/LaunchDaemons/dev.hostbay.netd.plist"

	linuxDir    = "/var/lib/hostbay-netd"
	linuxSocket = "/run/hostbay-netd/netd.sock" // systemd's RuntimeDirectory, root-owned
	linuxUnit   = "/etc/systemd/system/hostbay-netd.service"

	windowsService = "hostbay-netd"
	windowsPipe    = `\\.\pipe\hostbay-netd`
)

// The admin prompt says what the helper may do (R30).
const (
	installPrompt   = "Hostbay wants to install its network helper. It runs as an administrator and lets only Hostbay add and remove routes through VPN tunnels and the default gateway, and set tunnel MTUs."
	uninstallPrompt = "Hostbay wants to remove its network helper."
)

// vpnClients are copied into the helper's directory at install when they sit
// beside Hostbay, so the helper starts only copies nobody else can replace.
var vpnClients = []string{"openvpn", "openconnect"}

func exeName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// run runs an elevation tool and reports its exit code; tests replace it.
var run = func(name string, args ...string) (int, string, error) {
	var stderr bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), stderr.String(), nil
	}
	return 0, stderr.String(), err
}

// elevate starts a program elevated through UAC and waits for it (Windows);
// tests replace it.
var elevate = elevateNative

var errElevationCancelled = errors.New("elevation cancelled")

var (
	errCancelled = &Error{CodeCancelled, "The helper was not installed: the administrator prompt was cancelled."}
	errAppImage  = &Error{CodeUnavailable, "System-wide routing cannot run from the AppImage; install Hostbay from the deb or rpm package."}
)

func toolFailed(what string, code int, stderr string, err error) error {
	if err != nil {
		return &Error{CodeFailed, fmt.Sprintf("%s: %v", what, err)}
	}
	return &Error{CodeFailed, fmt.Sprintf("%s failed (exit %d): %s", what, code, strings.TrimSpace(stderr))}
}

// osascriptSafe admits paths that need no escaping inside a single-quoted
// shell word inside an AppleScript string, so the script is the fixed text
// plus the path verbatim.
func osascriptSafe(p string) error { // p is a macOS path on every OS the tests run on
	if !path.IsAbs(p) || strings.ContainsAny(p, `'"\`) || strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return &Error{CodeInvalid, fmt.Sprintf("Hostbay cannot install its helper from %q; move Hostbay to a folder whose path has no quotes or backslashes.", p)}
	}
	return nil
}

func osascript(script string) error {
	code, stderr, err := run("/usr/bin/osascript", "-e", script)
	switch {
	case err == nil && code == 0:
		return nil
	case err == nil && strings.Contains(stderr, "(-128)"): // userCanceledErr
		return errCancelled
	}
	return toolFailed("osascript", code, stderr, err)
}

// installDarwin asks once for an administrator and runs the helper beside
// Hostbay as root with "install", which copies it into darwinDir, records
// the pairing and loads the LaunchDaemon (SetUp).
func installDarwin(helper string, uid int) error {
	if err := osascriptSafe(helper); err != nil {
		return err
	}
	return osascript(fmt.Sprintf(`do shell script "'%s' install %d" with prompt "%s" with administrator privileges`, helper, uid, installPrompt))
}

func uninstallDarwin() error {
	return osascript(fmt.Sprintf(`do shell script "'%s/%s' uninstall" with prompt "%s" with administrator privileges`, darwinDir, helperName, uninstallPrompt))
}

func pkexec(args ...string) error {
	code, stderr, err := run("/usr/bin/pkexec", args...)
	switch {
	case err == nil && code == 0:
		return nil
	case err == nil && code == 126: // the user dismissed the authentication dialog
		return errCancelled
	}
	return toolFailed("pkexec", code, stderr, err)
}

// installLinux runs the helper beside Hostbay as root through polkit; the
// helper reads the installing user from PKEXEC_UID. An AppImage cannot: its
// mount vanishes when Hostbay quits, and the helper ships only in the deb
// and rpm packages.
func installLinux(helper string) error {
	if os.Getenv("APPIMAGE") != "" {
		return errAppImage
	}
	return pkexec(helper, "install")
}

func uninstallLinux() error {
	return pkexec(filepath.Join(linuxDir, helperName), "uninstall")
}

var sidPattern = regexp.MustCompile(`^S-1-[0-9]+(-[0-9]+)+$`)

func elevated(file string, args ...string) error {
	code, err := elevate(file, args...)
	switch {
	case errors.Is(err, errElevationCancelled):
		return errCancelled
	case err == nil && code == 0:
		return nil
	}
	return toolFailed(filepath.Base(file), code, "", err)
}

// installWindows runs the helper beside Hostbay elevated with "install" and
// the installing user's SID; it copies itself under Program Files and
// registers and starts the service (SetUp).
func installWindows(helper, sid string) error {
	if !sidPattern.MatchString(sid) {
		return &Error{CodeInvalid, fmt.Sprintf("%q is not a Windows user SID.", sid)}
	}
	return elevated(helper, "install", sid)
}

func uninstallWindows(installed string) error {
	return elevated(installed, "uninstall")
}

// besideSelf is the path of name in the running executable's directory, where
// packaging puts the helper.
func besideSelf(name string) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(self), exeName(name)), nil
}

// Status is what Hostbay shows about the helper.
type Status struct {
	// Unavailable says why this build cannot install the helper at all.
	Unavailable string `json:"unavailable,omitempty"`
	Installed   bool   `json:"installed"`
	// Paired is false when the helper belongs to another user or another
	// Hostbay copy: Hostbay offers to re-pair.
	Paired bool `json:"paired"`
	// Running is true when the helper answered.
	Running bool `json:"running"`
	// Current is false when the helper speaks another protocol version:
	// Hostbay offers to reinstall.
	Current bool `json:"current"`
}

// GetStatus reads the pairing and tries the helper. Hostbay asks before it
// holds a connection: the helper serves one at a time.
func GetStatus(ctx context.Context) Status {
	if runtime.GOOS == "linux" && os.Getenv("APPIMAGE") != "" {
		return Status{Unavailable: errAppImage.Reason}
	}
	var st Status
	b, err := os.ReadFile(filepath.Join(stateDir(), "config.json"))
	var cfg Config
	if err != nil || json.Unmarshal(b, &cfg) != nil {
		return st
	}
	st.Installed = true
	app, _ := besideSelf("hostbay")
	uid, _ := currentUID()
	st.Paired = authorize(Peer{UID: uid, Exe: app}, cfg) == nil
	c, err := Dial(ctx)
	switch code := errCode(err); {
	case err == nil:
		c.Close()
		st.Running, st.Current = true, true
	case code == CodeBusy:
		st.Running, st.Current = true, true
	case code == CodeVersion:
		st.Running = true
	case code == CodeDenied:
		st.Running, st.Current, st.Paired = true, true, false
	}
	return st
}

func errCode(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// setUpDir is the root side of install shared by every platform: it copies
// the running helper and any VPN client beside Hostbay into dir and records
// the pairing. Hostbay is the "hostbay" executable beside the helper.
func setUpDir(dir, uid string) error {
	self, err := os.Executable()
	if err == nil {
		self, err = filepath.EvalSymlinks(self)
	}
	if err != nil {
		return err
	}
	app := filepath.Join(filepath.Dir(self), exeName("hostbay"))
	if _, err := os.Stat(app); err != nil {
		return fmt.Errorf("Hostbay is not beside the helper: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		return err
	}
	if err := copyFile(self, filepath.Join(dir, exeName(helperName)), 0o755); err != nil {
		return err
	}
	for _, name := range vpnClients {
		src := filepath.Join(filepath.Dir(app), exeName(name))
		if _, err := os.Stat(src); err == nil {
			if err := copyFile(src, filepath.Join(dir, exeName(name)), 0o755); err != nil {
				return err
			}
		}
	}
	b, err := json.Marshal(Config{UID: uid, App: app})
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(dir, "config.json"), b, 0o644)
}

func copyFile(src, dst string, mode os.FileMode) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return writeFile(dst, b, mode)
}

// writeFile replaces path whole with a file of the given mode.
func writeFile(path string, b []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".install-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(b)
	if err == nil {
		err = tmp.Chmod(mode)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func validUID(s string) error {
	if n, err := strconv.Atoi(s); err != nil || n < 0 || strconv.Itoa(n) != s {
		return fmt.Errorf("%q is not a user id", s)
	}
	return nil
}
