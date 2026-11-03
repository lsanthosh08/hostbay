package browser

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Every platform finds all four browsers by their own executable, and the
// launch command is that absolute path with the profile, the loopback proxy
// and the WebRTC policy, and nothing that bypasses the proxy (KTD11).
func TestLaunchCommandPerPlatform(t *testing.T) {
	r := roots{home: "/home/u", config: "/home/u/.config", localAppData: "/lad", programFiles: "/pf", programFilesX86: "/pf86"}
	exes := map[string]map[string]string{
		"darwin":  {"chrome": "Google Chrome", "chromium": "Chromium", "edge": "Microsoft Edge", "brave": "Brave Browser"},
		"linux":   {"chrome": "chrome", "chromium": "chromium", "edge": "msedge", "brave": "brave"},
		"windows": {"chrome": "chrome.exe", "chromium": "chrome.exe", "edge": "msedge.exe", "brave": "brave.exe"},
	}
	for goos, want := range exes {
		got := installed(goos, r, func(string) bool { return true })
		if len(got) != len(want) {
			t.Fatalf("%s: found %v", goos, got)
		}
		for _, b := range got {
			if filepath.Base(b.Exe) != want[b.ID] || !strings.HasPrefix(filepath.ToSlash(b.Exe), "/") || b.UserData == "" || b.Name == "" {
				t.Errorf("%s %s: %+v", goos, b.ID, b)
			}
			cmd := Command(b, "Profile 2", 41081, nil, nil)
			if cmd.Path != b.Exe {
				t.Errorf("%s %s: runs %q, want %q", goos, b.ID, cmd.Path, b.Exe)
			}
			args := []string{"--profile-directory=Profile 2", "--proxy-server=socks5://127.0.0.1:41081", "--force-webrtc-ip-handling-policy=disable_non_proxied_udp"}
			if !slices.Equal(cmd.Args[1:], args) {
				t.Errorf("%s %s: args %q", goos, b.ID, cmd.Args[1:])
			}
			for _, a := range cmd.Args {
				if strings.Contains(a, "bypass") || strings.Contains(a, "no-proxy") {
					t.Errorf("%s %s: bypass flag %q", goos, b.ID, a)
				}
			}
		}
	}
}

// A browser counts as installed only when its executable is there.
func TestInstalledNeedsTheExecutable(t *testing.T) {
	r := roots{config: "/c", home: "/h"}
	got := installed("linux", r, func(p string) bool { return p == "/usr/bin/brave-browser" })
	if len(got) != 1 || got[0].ID != "brave" || got[0].Exe != "/usr/bin/brave-browser" || filepath.ToSlash(got[0].UserData) != "/c/BraveSoftware/Brave-Browser" {
		t.Fatalf("installed %+v", got)
	}
}

func TestProfilesReadLocalState(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Local State"), []byte(`{"profile":{"info_cache":{"Profile 3":{"name":"Work"},"Default":{"name":"Personal"}}}}`), 0o600)
	got, err := Profiles(Browser{UserData: dir})
	want := []Profile{{Dir: "Default", Name: "Personal"}, {Dir: "Profile 3", Name: "Work"}}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("Profiles %+v, %v", got, err)
	}
	if _, err := Profiles(Browser{UserData: t.TempDir()}); err == nil {
		t.Fatal("no Local State: no error")
	}
}

// A VPN that carries only some networks sends only those through the proxy:
// the browser gets an auto-config, not a blanket proxy, and the script has no
// direct fallback for what it proxies (KTD11).
func TestCommandSplitsTrafficForAVPNThatCarriesOnlySomeNetworks(t *testing.T) {
	b := Browser{Exe: "/b"}
	cmd := Command(b, "Default", 41081, []string{"10.212.0.0/16", "172.31.58.0/24", "fd00::/8", "junk"}, []string{"Corp.Example", "bad\"; evil()"})
	if len(cmd.Args) != 4 || strings.Contains(cmd.Args[2], "--proxy-server") {
		t.Fatalf("args %q", cmd.Args)
	}
	enc, ok := strings.CutPrefix(cmd.Args[2], "--proxy-pac-url=data:application/x-ns-proxy-autoconfig;base64,")
	if !ok {
		t.Fatalf("args %q", cmd.Args)
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatal(err)
	}
	pac := string(raw)
	for _, want := range []string{`"SOCKS5 127.0.0.1:41081"`, `isInNet(ip, "10.212.0.0", "255.255.0.0")`, `isInNet(ip, "172.31.58.0", "255.255.255.0")`,
		`dnsDomainIs(h, ".corp.example")`, `return "DIRECT";`} {
		if !strings.Contains(pac, want) {
			t.Errorf("auto-config lacks %s:\n%s", want, pac)
		}
	}
	for _, bad := range []string{"fd00", "junk", "evil", "DIRECT;", "; DIRECT"} {
		if strings.Contains(pac, bad) {
			t.Errorf("auto-config holds %q:\n%s", bad, pac)
		}
	}
}

// With no networks known, or a default route among them, everything goes
// through the listener.
func TestCommandProxiesEverythingForAFullTunnelOrUnknownRoutes(t *testing.T) {
	for _, nets := range [][]string{nil, {"fd00::/8"}, {"10.0.0.0/8", "0.0.0.0/0"}, {"0.0.0.0/1", "128.0.0.0/1"}} {
		if args := Command(Browser{Exe: "/b"}, "D", 41081, nets, nil).Args; args[2] != "--proxy-server=socks5://127.0.0.1:41081" {
			t.Errorf("%v: args %q", nets, args)
		}
	}
}

// A separate profile lives in its own user data folder, so it opens beside a
// running browser, and takes the same proxy flags.
func TestCommandSeparateUsesItsOwnUserDataFolder(t *testing.T) {
	args := CommandSeparate(Browser{Exe: "/b"}, "/data/browsers/chrome-Default", 41081, nil, nil).Args
	want := []string{"--user-data-dir=/data/browsers/chrome-Default", "--no-first-run", "--no-default-browser-check",
		"--proxy-server=socks5://127.0.0.1:41081", "--force-webrtc-ip-handling-policy=disable_non_proxied_udp"}
	if !slices.Equal(args[1:], want) {
		t.Fatalf("args %q", args)
	}
}
