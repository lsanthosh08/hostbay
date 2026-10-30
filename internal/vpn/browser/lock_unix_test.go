//go:build !windows

package browser

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// A live SingletonLock (this host, a running pid) means the browser is
// running; a stale one or none does not.
func TestRunningFollowsSingletonLock(t *testing.T) {
	host, _ := os.Hostname()
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, target string
		want         bool
	}{
		{"no lock", "", false},
		{"live", host + "-" + strconv.Itoa(os.Getpid()), true},
		{"stale", host + "-" + strconv.Itoa(dead.Process.Pid), false},
		{"another host", "elsewhere.example-" + strconv.Itoa(os.Getpid()), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if c.target != "" {
				if err := os.Symlink(c.target, filepath.Join(dir, "SingletonLock")); err != nil {
					t.Fatal(err)
				}
			}
			if got := Running(Browser{UserData: dir}); got != c.want {
				t.Fatalf("Running %v, want %v", got, c.want)
			}
		})
	}
}
