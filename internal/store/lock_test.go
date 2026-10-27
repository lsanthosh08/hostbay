package store

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

const (
	lockHelperEnv = "HOSTBAY_LOCK_HELPER_DIR"
	// moveKillEnv, as "<step>:<n>", makes the helper exit at that move step,
	// as a crash would.
	moveKillEnv = "HOSTBAY_MOVE_KILL"
	killedCode  = 3
)

// TestMain doubles as a second process: with lockHelperEnv set, it tries to
// open the store, prints the outcome and exits still holding any lock it got.
func TestMain(m *testing.M) {
	if dir := os.Getenv(lockHelperEnv); dir != "" {
		if kill := os.Getenv(moveKillEnv); kill != "" {
			step, n, _ := strings.Cut(kill, ":")
			at, _ := strconv.Atoi(n)
			moveHook = func(s string, i int) error {
				if s == step && i == at {
					os.Exit(killedCode)
				}
				return nil
			}
		}
		_, err := Open(dir)
		switch {
		case err == nil:
			fmt.Print("locked")
		case errors.Is(err, ErrLocked):
			fmt.Print("refused")
		default:
			fmt.Print(err)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runHelper runs TestMain's helper on dir with extra environment. With
// moveKillEnv set, it fails unless the helper was killed at that step.
func runHelper(t *testing.T, dir string, env ...string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(append(os.Environ(), lockHelperEnv+"="+dir), env...)
	out, err := cmd.Output()
	var exit *exec.ExitError
	switch {
	case len(env) > 0 && errors.As(err, &exit) && exit.ExitCode() == killedCode:
		return ""
	case len(env) > 0:
		t.Fatalf("helper was not killed at %v: %v, %q", env, err, out)
	case err != nil:
		t.Fatalf("helper process: %v", err)
	}
	return string(out)
}

func openInOtherProcess(t *testing.T, dir string) string {
	t.Helper()
	return runHelper(t, dir)
}

func TestSecondProcessIsRefusedWhileLocked(t *testing.T) {
	dir := t.TempDir()
	s := openT(t, dir)
	if got := openInOtherProcess(t, dir); got != "refused" {
		t.Fatalf("second process while locked: %q, want refused", got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// The helper takes the lock and exits without closing the store.
	if got := openInOtherProcess(t, dir); got != "locked" {
		t.Fatalf("second process after release: %q, want locked", got)
	}
	s = openT(t, dir) // the exited helper's lock was released by the OS
	s.Close()
}
