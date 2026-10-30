package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"hostbay/internal/container"
)

// Outcomes of TunnelHubService.Remove.
const (
	TunnelHubRemoved   = "removed"   // the removal ran; Removed and Failed say what happened to each item
	TunnelHubDeclined  = "declined"  // the user said no in Hostbay's dialog
	TunnelHubCancelled = "cancelled" // the user cancelled the administrator prompt; nothing changed
)

// tunnelHubAdminPrompt is what macOS's administrator prompt says.
const tunnelHubAdminPrompt = "Hostbay wants to remove TunnelHub's network helper."

// TunnelHubItem is one TunnelHub file on this computer. Admin: removing it
// needs an administrator.
type TunnelHubItem struct {
	Kind  string `json:"kind"` // daemon, helper, socket, agent; config and app are never removed
	Path  string `json:"path"`
	Admin bool   `json:"admin"`
}

// TunnelHubFound is what Detect found. Items are what Remove would remove.
// The config is kept for import (R41), and the app bundle is shown only:
// removing it stays with the user.
type TunnelHubFound struct {
	Items      []TunnelHubItem `json:"items"`
	ConfigPath string          `json:"configPath,omitempty"`
	AppPath    string          `json:"appPath,omitempty"`
}

// TunnelHubRemoval is what Remove did.
type TunnelHubRemoval struct {
	Status  string   `json:"status"`
	Removed []string `json:"removed"`
	Failed  []string `json:"failed"`
	Error   string   `json:"error,omitempty"` // why a removal step failed
}

// TunnelHubService finds the user's TunnelHub install and removes its
// privileged helper and launch-at-login agent once Hostbay replaces it
// (R45). Only macOS had TunnelHub; elsewhere it finds nothing.
type TunnelHubService struct {
	goos    string
	root    string // prefixed to the system paths; tests point it at a temp dir
	home    string
	uid     int
	confirm container.Confirm
	run     func(name string, args ...string) (int, string, error) // tests record instead of running
}

func NewTunnelHubService() *TunnelHubService {
	home, _ := os.UserHomeDir()
	return &TunnelHubService{goos: runtime.GOOS, home: home, uid: os.Getuid(), confirm: container.NativeConfirm, run: tunnelHubRun}
}

// tunnelHubRun runs a program and reports its exit code and stderr.
func tunnelHubRun(name string, args ...string) (int, string, error) {
	var stderr strings.Builder
	cmd := exec.Command(name, args...)
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), stderr.String(), nil
	}
	return 0, stderr.String(), err
}

// spots are every place TunnelHub keeps something, fixed paths only.
func (s *TunnelHubService) spots() []TunnelHubItem {
	if s.goos != "darwin" {
		return nil
	}
	sys := func(p string) string { return s.root + p }
	l := []TunnelHubItem{
		{"daemon", sys("/Library/LaunchDaemons/com.santhosh.tunnelhub.helper.plist"), true},
		{"helper", sys("/usr/local/libexec/tunnelhub-helper"), true},
		{"socket", sys("/var/run/tunnelhub.sock"), true},
		{"app", sys("/Applications/TunnelHub.app"), false},
	}
	if s.home != "" {
		l = append(l,
			TunnelHubItem{"agent", filepath.Join(s.home, "Library/LaunchAgents/com.santhosh.tunnelhub.autostart.plist"), false},
			TunnelHubItem{"config", filepath.Join(s.home, ".config/tunnelhub/vpn-config.json"), false},
			TunnelHubItem{"app", filepath.Join(s.home, "Applications/TunnelHub.app"), false},
		)
	}
	return l
}

func tunnelHubPresent(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// Detect reports the TunnelHub items on this computer, reading nothing but
// whether each fixed path exists.
func (s *TunnelHubService) Detect() TunnelHubFound {
	f := TunnelHubFound{Items: []TunnelHubItem{}}
	for _, sp := range s.spots() {
		if !tunnelHubPresent(sp.Path) {
			continue
		}
		switch sp.Kind {
		case "config":
			f.ConfigPath = sp.Path
		case "app":
			if f.AppPath == "" {
				f.AppPath = sp.Path
			}
		default:
			f.Items = append(f.Items, sp)
		}
	}
	return f
}

// tunnelHubPathSafe admits paths that need no escaping inside a single-quoted shell
// word inside an AppleScript string (as netd's osascriptSafe).
func tunnelHubPathSafe(p string) bool {
	return path.IsAbs(p) && !strings.ContainsAny(p, `'"\`) && !strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// Remove asks in a native dialog, then removes TunnelHub's helper (its
// LaunchDaemon, binary and socket) through one administrator prompt, and
// its launch-at-login agent without one. The config and the app bundle
// stay. The page names nothing: Remove acts only on what it detects at the
// fixed paths.
func (s *TunnelHubService) Remove(ctx context.Context) (TunnelHubRemoval, error) {
	items := s.Detect().Items
	r := TunnelHubRemoval{Status: TunnelHubRemoved, Removed: []string{}, Failed: []string{}}
	if len(items) == 0 {
		return r, nil
	}
	var root []string
	lines := make([]string, len(items))
	for i, it := range items {
		lines[i] = "• " + it.Path
		if it.Admin {
			if !tunnelHubPathSafe(it.Path) {
				return r, fmt.Errorf("Hostbay cannot remove %q: its path has a quote or backslash", it.Path)
			}
			root = append(root, "'"+it.Path+"'")
		}
	}
	msg := "Hostbay will remove these TunnelHub items:\n\n" + strings.Join(lines, "\n") +
		"\n\nHostbay's routes apply only while Hostbay runs, unlike TunnelHub's, which stayed after TunnelHub quit." +
		" TunnelHub's config file stays. Moving the TunnelHub app to the Trash is up to you."
	if len(root) > 0 {
		msg += "\n\nmacOS will ask once for an administrator to remove the helper."
	}
	if !s.confirm(ctx, "Remove TunnelHub's helper and agent?", msg) {
		r.Status = TunnelHubDeclined
		return r, nil
	}
	var fails []string
	// The admin step runs first, so a cancel there leaves everything as it was.
	if len(root) > 0 {
		var script strings.Builder
		for _, it := range items {
			if it.Kind == "daemon" {
				script.WriteString("/bin/launchctl bootout system '" + it.Path + "'; ")
			}
		}
		script.WriteString("/bin/rm -f " + strings.Join(root, " "))
		code, stderr, err := s.run("/usr/bin/osascript", "-e",
			fmt.Sprintf(`do shell script "%s" with prompt "%s" with administrator privileges`, script.String(), tunnelHubAdminPrompt))
		switch {
		case err == nil && strings.Contains(stderr, "(-128)"): // userCanceledErr
			r.Status = TunnelHubCancelled
			return r, nil
		case err != nil:
			fails = append(fails, err.Error())
		case code != 0:
			fails = append(fails, strings.TrimSpace(stderr))
		}
	}
	for _, it := range items {
		if it.Kind == "agent" {
			s.run("/bin/launchctl", "bootout", "gui/"+strconv.Itoa(s.uid), it.Path) // fails when it is not loaded
			if err := os.Remove(it.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
				fails = append(fails, err.Error())
			}
		}
	}
	// What is gone is what the filesystem says, not what the tools reported.
	for _, it := range items {
		if tunnelHubPresent(it.Path) {
			r.Failed = append(r.Failed, it.Path)
		} else {
			r.Removed = append(r.Removed, it.Path)
		}
	}
	if len(r.Failed) > 0 {
		r.Error = strings.Join(fails, "; ")
	}
	return r, nil
}
