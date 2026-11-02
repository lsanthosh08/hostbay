// Package sase drives the Perimeter 81 / Harmony SASE ("Check Point SASE")
// app on a Mac from its own command line, so a profile can connect and
// disconnect without Hostbay opening the app's window.
//
// The app binary is also its CLI: run with a subcommand (status, connect,
// disconnect) it talks to the app's own root daemon, which holds the login
// and the WireGuard tunnel, then exits. It refuses to run beside the menu
// bar app ("conflicting app"), so the app is quit first. Nothing here sees
// a credential: the daemon uses the sign-in the app already saved.
package sase

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Wait bounds one CLI call; a connect waits on the server.
const Wait = 90 * time.Second

// Bin is the app's executable (its CLI) for the saved client program path,
// or "" when the path is not an app bundle holding one, or off a Mac.
func Bin(app string) string {
	if runtime.GOOS != "darwin" || !strings.HasSuffix(strings.TrimRight(app, "/"), ".app") {
		return ""
	}
	exe := strings.TrimSuffix(filepath.Base(strings.TrimRight(app, "/")), ".app")
	bin := filepath.Join(app, "Contents", "MacOS", exe)
	if fi, err := os.Stat(bin); err != nil || fi.IsDir() {
		return ""
	}
	return bin
}

func realRun(ctx context.Context, bin string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	return string(out), err
}

// run and quit are replaced in tests.
var (
	run = realRun
	// quit ends the app's menu bar process if it is running: the CLI will not
	// start beside it. The match is the exact command line, so a CLI call of
	// our own is never taken for it.
	quit = func(bin string) {
		exec.Command("pkill", "-TERM", "-fx", bin).Run()
		for i := 0; i < 20; i++ {
			if out, _ := exec.Command("pgrep", "-fx", bin).Output(); len(strings.TrimSpace(string(out))) == 0 {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
)

// Connect connects the app's current network and returns when it says so.
func Connect(ctx context.Context, bin string) error { return call(ctx, bin, "connect", "connected") }

// Disconnect disconnects it.
func Disconnect(ctx context.Context, bin string) error {
	return call(ctx, bin, "disconnect", "disconnected")
}

func call(ctx context.Context, bin, cmd, want string) error {
	ctx, cancel := context.WithTimeout(ctx, Wait)
	defer cancel()
	quit(bin)
	out, err := run(ctx, bin, cmd)
	st := field(out, "Status")
	switch {
	case st == want, cmd == "disconnect" && strings.Contains(out, "disconnected"):
		return nil
	case st != "":
		return fmt.Errorf("the VPN app still says %q after %s; try again in a moment", st, cmd)
	}
	if msg := message(out); msg != "" {
		return errors.New(msg)
	}
	if err != nil {
		return fmt.Errorf("the VPN app's %s did not run: %w", cmd, err)
	}
	return fmt.Errorf("the VPN app gave no answer to %s", cmd)
}

// field is the value of a "Name:   value" line of the CLI's output.
func field(out, name string) string {
	for _, l := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), ":"); ok && k == name {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// message is the CLI's own "Error: ..." line, which says why it stopped,
// ignoring the Objective-C runtime's noise before it.
func message(out string) string {
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "Error") {
			return l
		}
	}
	return ""
}
