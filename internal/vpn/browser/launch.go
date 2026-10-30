// Package browser finds the Chromium-family browsers installed on this
// computer and their profiles, and builds the command that launches one
// profile through a VPN's loopback SOCKS5 listener (R33, KTD11).
//
// Browsers are found only at their installers' fixed paths, never on PATH,
// and run by absolute path with arguments only, so nothing the user or a
// synced file sets chooses what Hostbay runs.
package browser

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
)

// Browser is one installed browser. Exe and UserData are this computer's
// own and never leave the Go side.
type Browser struct {
	ID       string `json:"id"` // chrome, chromium, edge or brave
	Name     string `json:"name"`
	Exe      string `json:"-"` // absolute
	UserData string `json:"-"` // the folder holding "Local State" and the profiles
}

// Profile is one browser profile: Dir is its folder in the user data
// folder, what --profile-directory takes; Name is what the browser shows.
type Profile struct {
	Dir  string `json:"dir"`
	Name string `json:"name"`
}

// roots are the folders the install paths hang off.
type roots struct {
	home, config, localAppData, programFiles, programFilesX86 string
}

type spec struct {
	id, name string
	exes     []string // the first that exists is used
	userData string
}

// known lists where each browser installs on goos.
func known(goos string, r roots) []spec {
	j := filepath.Join
	switch goos {
	case "darwin":
		app := func(bundle, exe string) []string {
			return []string{j("/Applications", bundle, "Contents/MacOS", exe), j(r.home, "Applications", bundle, "Contents/MacOS", exe)}
		}
		return []spec{
			{"chrome", "Google Chrome", app("Google Chrome.app", "Google Chrome"), j(r.config, "Google/Chrome")},
			{"chromium", "Chromium", app("Chromium.app", "Chromium"), j(r.config, "Chromium")},
			{"edge", "Microsoft Edge", app("Microsoft Edge.app", "Microsoft Edge"), j(r.config, "Microsoft Edge")},
			{"brave", "Brave", app("Brave Browser.app", "Brave Browser"), j(r.config, "BraveSoftware/Brave-Browser")},
		}
	case "windows":
		in := func(rel string, bases ...string) []string {
			var out []string
			for _, b := range bases {
				if b != "" {
					out = append(out, j(b, rel))
				}
			}
			return out
		}
		return []spec{
			{"chrome", "Google Chrome", in(`Google/Chrome/Application/chrome.exe`, r.programFiles, r.programFilesX86, r.localAppData), j(r.localAppData, "Google/Chrome/User Data")},
			{"chromium", "Chromium", in(`Chromium/Application/chrome.exe`, r.localAppData), j(r.localAppData, "Chromium/User Data")},
			{"edge", "Microsoft Edge", in(`Microsoft/Edge/Application/msedge.exe`, r.programFilesX86, r.programFiles), j(r.localAppData, "Microsoft/Edge/User Data")},
			{"brave", "Brave", in(`BraveSoftware/Brave-Browser/Application/brave.exe`, r.programFiles, r.programFilesX86, r.localAppData), j(r.localAppData, "BraveSoftware/Brave-Browser/User Data")},
		}
	default: // linux and the other Unixes
		// shortcut: snap and flatpak installs keep their profiles elsewhere and are not found; add them if asked for.
		return []spec{
			{"chrome", "Google Chrome", []string{"/opt/google/chrome/chrome", "/usr/bin/google-chrome-stable", "/usr/bin/google-chrome"}, j(r.config, "google-chrome")},
			{"chromium", "Chromium", []string{"/usr/lib/chromium/chromium", "/usr/bin/chromium", "/usr/bin/chromium-browser"}, j(r.config, "chromium")},
			{"edge", "Microsoft Edge", []string{"/opt/microsoft/msedge/msedge", "/usr/bin/microsoft-edge"}, j(r.config, "microsoft-edge")},
			{"brave", "Brave", []string{"/opt/brave.com/brave/brave", "/usr/bin/brave-browser"}, j(r.config, "BraveSoftware/Brave-Browser")},
		}
	}
}

func installed(goos string, r roots, exists func(string) bool) []Browser {
	out := []Browser{}
	for _, s := range known(goos, r) {
		if i := slices.IndexFunc(s.exes, exists); i >= 0 {
			out = append(out, Browser{ID: s.id, Name: s.name, Exe: s.exes[i], UserData: s.userData})
		}
	}
	return out
}

// Installed lists the Chromium-family browsers on this computer.
func Installed() []Browser {
	home, _ := os.UserHomeDir()
	config, _ := os.UserConfigDir()
	r := roots{home: home, config: config, localAppData: os.Getenv("LOCALAPPDATA"),
		programFiles: os.Getenv("ProgramFiles"), programFilesX86: os.Getenv("ProgramFiles(x86)")}
	return installed(runtime.GOOS, r, func(p string) bool {
		fi, err := os.Stat(p)
		return err == nil && !fi.IsDir() && filepath.IsAbs(p)
	})
}

// Profiles lists b's profiles from its "Local State", by folder.
func Profiles(b Browser) ([]Profile, error) {
	raw, err := os.ReadFile(filepath.Join(b.UserData, "Local State"))
	if err != nil {
		return nil, fmt.Errorf("cannot read %s's profiles: %w", b.Name, err)
	}
	var ls struct {
		Profile struct {
			InfoCache map[string]struct {
				Name string `json:"name"`
			} `json:"info_cache"`
		} `json:"profile"`
	}
	if err := json.Unmarshal(raw, &ls); err != nil {
		return nil, fmt.Errorf("cannot read %s's profiles: %w", b.Name, err)
	}
	out := []Profile{}
	for dir, p := range ls.Profile.InfoCache {
		out = append(out, Profile{Dir: dir, Name: p.Name})
	}
	slices.SortFunc(out, func(a, b Profile) int { return cmp.Compare(a.Dir, b.Dir) })
	return out, nil
}

// Running reports whether b is running, from the lock in its user data
// folder. The lock covers every profile: a launch while any is open goes to
// the running browser, which would open the profile without the proxy.
func Running(b Browser) bool { return locked(b.UserData) }

// Command launches profile dir of b through the SOCKS5 listener at
// 127.0.0.1:port. A socks5 proxy also takes the browser's DNS, the WebRTC
// policy keeps WebRTC's UDP from going around the proxy, and no flag
// bypasses it.
func Command(b Browser, dir string, port int) *exec.Cmd {
	return exec.Command(b.Exe,
		"--profile-directory="+dir,
		"--proxy-server=socks5://127.0.0.1:"+strconv.Itoa(port),
		"--force-webrtc-ip-handling-policy=disable_non_proxied_udp")
}
