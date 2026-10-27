//go:build !windows

package terminal

import (
	"bytes"
	"errors"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// screen collects a transport's output so a test can wait for a line.
type screen struct {
	mu  sync.Mutex
	buf bytes.Buffer
	end chan struct{}
}

func watch(tr Transport) *screen {
	s := &screen{end: make(chan struct{})}
	go func() {
		defer close(s.end)
		b := make([]byte, 4096)
		for {
			n, err := tr.Read(b)
			s.mu.Lock()
			s.buf.Write(b[:n])
			s.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return s
}

// waitFor returns the first match of re in the output, failing after 5s.
func (s *screen) waitFor(t *testing.T, re string) []string {
	t.Helper()
	rx := regexp.MustCompile(re)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		s.mu.Lock()
		m := rx.FindStringSubmatch(s.buf.String())
		s.mu.Unlock()
		if m != nil {
			return m
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t.Fatalf("no %q in output:\n%s", re, s.buf.String())
	return nil
}

// openSh starts the login shell with SHELL=/bin/sh and HOME=a temp dir.
func openSh(t *testing.T) (Transport, *screen, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("HOME", home)
	t.Setenv("TERM", "dumb")
	tr, err := OpenLocal(loginShellID, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tr.Close() })
	return tr, watch(tr), home
}

func gone(pid int) bool {
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		// ESRCH means the process is dead and reaped; a zombie still answers.
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestLocalShellRunsTypedCommandInHome(t *testing.T) {
	tr, out, home := openSh(t)
	tr.Write([]byte("echo out-$((40+2)) T=$TERM D=$(pwd -P)\n"))
	m := out.waitFor(t, `out-42 T=(\S+) D=(\S+)`)
	if m[1] != "xterm-256color" {
		t.Errorf("TERM=%s", m[1])
	}
	want, _ := filepath.EvalSymlinks(home)
	if m[2] != want {
		t.Errorf("started in %s, want %s", m[2], want)
	}
}

func TestLocalShellResizesPTY(t *testing.T) {
	tr, out, _ := openSh(t)
	out.waitFor(t, `\$ ?$`) // a prompt, so stty runs after the shell is up
	if err := tr.Resize(132, 43); err != nil {
		t.Fatal(err)
	}
	tr.Write([]byte("echo S=$(stty size)\n"))
	out.waitFor(t, `S=43 132`)
}

func TestLocalShellCloseKillsAndReapsShellAndForegroundJob(t *testing.T) {
	tr, out, _ := openSh(t)
	shell := tr.(*localShell).cmd.Process.Pid
	// The job prints its pid, then becomes a long sleep in the foreground.
	tr.Write([]byte("sh -c 'echo J=$$; exec sleep 1000'\n"))
	job, _ := strconv.Atoi(out.waitFor(t, `J=(\d+)`)[1])

	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	if !gone(shell) {
		t.Errorf("shell %d still exists after close", shell)
	}
	if !gone(job) {
		syscall.Kill(job, syscall.SIGKILL)
		t.Errorf("foreground job %d survived the tab closing", job)
	}
	select {
	case <-out.end:
	case <-time.After(3 * time.Second):
		t.Fatal("read did not end after close")
	}
	if r := tr.(Ender).EndReason(); r != "session closed" {
		t.Errorf("end reason %q", r)
	}
}

func TestLocalShellExitEndsTerminal(t *testing.T) {
	tr, out, _ := openSh(t)
	shell := tr.(*localShell).cmd.Process.Pid
	tr.Write([]byte("exit\n"))
	select {
	case <-out.end:
	case <-time.After(5 * time.Second):
		t.Fatal("read did not end after the shell exited")
	}
	if r := tr.(Ender).EndReason(); !strings.Contains(r, "exited") {
		t.Errorf("end reason %q", r)
	}
	if !gone(shell) {
		t.Errorf("shell %d not reaped", shell)
	}
}

func TestLoginShellFallsBackToBinSh(t *testing.T) {
	for _, v := range []string{"", "/no/such/shell", "relative-sh"} {
		t.Setenv("SHELL", v)
		if got := loginShell(); got != "/bin/sh" {
			t.Errorf("SHELL=%q: %s", v, got)
		}
	}
	t.Setenv("SHELL", "/bin/sh")
	if got := loginShell(); got != "/bin/sh" {
		t.Errorf("SHELL=/bin/sh: %s", got)
	}
}

func TestLocalShellsListsOnlyTheLoginShell(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	l := ListLocalShells()
	if len(l) != 1 || l[0].ID != loginShellID || l[0].Name != "sh" {
		t.Fatalf("%+v", l)
	}
	if _, err := OpenLocal("/bin/rm", 80, 24); err == nil {
		t.Fatal("opened a shell ID that was not listed")
	}
}
