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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
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
// bypasses it. When the VPN carries only some networks (nets, plus the
// names under domains), a proxy auto-config sends those through the
// listener and every other address straight out, since the VPN has no path
// for it; with no networks, or a default route among them, everything goes
// through the listener.
func Command(b Browser, dir string, port int, nets, domains []string) *exec.Cmd {
	return exec.Command(b.Exe, append([]string{"--profile-directory=" + dir}, proxyArgs(port, nets, domains)...)...)
}

// CommandSeparate is Command for a profile kept in its own user data folder,
// which opens beside a running browser: the browser's lock covers one user
// data folder, so a second folder is a second browser, with Hostbay's proxy.
func CommandSeparate(b Browser, userData string, port int, nets, domains []string) *exec.Cmd {
	return exec.Command(b.Exe, append([]string{"--user-data-dir=" + userData, "--no-first-run", "--no-default-browser-check"}, proxyArgs(port, nets, domains)...)...)
}

// RunningIn reports whether a browser is running on user data folder dir.
func RunningIn(dir string) bool { return locked(dir) }

func proxyArgs(port int, nets, domains []string) []string {
	proxy := "--proxy-server=socks5://127.0.0.1:" + strconv.Itoa(port)
	if pac := splitPAC(port, nets, domains); pac != "" {
		proxy = "--proxy-pac-url=data:application/x-ns-proxy-autoconfig;base64," + base64.StdEncoding.EncodeToString([]byte(pac))
	}
	return []string{proxy, "--force-webrtc-ip-handling-policy=disable_non_proxied_udp"}
}

// splitPAC is the proxy auto-config for a VPN that carries only nets and the
// names under domains, or "" when everything goes through it. An address in
// nets, a name under domains, a single-label name, and a name this computer
// cannot resolve (one only the VPN's DNS knows) take the listener alone, with
// no direct fallback; any other address goes direct. Only well-formed IPv4
// networks and plain domain names reach the script.
func splitPAC(port int, nets, domains []string) string {
	var checks strings.Builder
	n, seen := 0, map[netip.Prefix]bool{}
	for _, s := range nets {
		p, err := netip.ParsePrefix(s)
		if err != nil || !p.Addr().Is4() {
			continue
		}
		if p.Bits() < 2 {
			return "" // a default route: everything is the VPN's
		}
		if p = p.Masked(); seen[p] {
			continue
		}
		seen[p] = true
		var mask [4]byte
		for i := 0; i < p.Bits(); i++ {
			mask[i/8] |= 0x80 >> (i % 8)
		}
		fmt.Fprintf(&checks, "  if (isInNet(ip, %q, %q)) return P;\n", p.Addr(), netip.AddrFrom4(mask))
		n++
	}
	if n == 0 {
		return ""
	}
	var names strings.Builder
	for _, d := range domains {
		d = strings.ToLower(strings.Trim(strings.TrimSpace(d), "."))
		if d != "" && !strings.ContainsFunc(d, func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.') }) {
			fmt.Fprintf(&names, "  if (h === %q || dnsDomainIs(h, %q)) return P;\n", d, "."+d)
		}
	}
	return fmt.Sprintf(`function FindProxyForURL(url, host) {
  var P = "SOCKS5 127.0.0.1:%d";
  var h = host.toLowerCase();
  if (h === "localhost" || h === "127.0.0.1" || h === "[::1]") return "DIRECT";
  if (isPlainHostName(h)) return P;
%s  var ip = /^\d+\.\d+\.\d+\.\d+$/.test(h) ? h : dnsResolve(h);
  if (!ip) return P;
%s  return "DIRECT";
}
`, port, names.String(), checks.String())
}
