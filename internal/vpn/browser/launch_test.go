package browser

import (
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
			cmd := Command(b, "Profile 2", 41081)
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
