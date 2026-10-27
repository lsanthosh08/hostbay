//go:build !windows

package terminal

// Local shells on macOS and Linux (R21, KTD10) learn from uniterm's
// backend/session/local_session_unix.go (Apache-2.0,
// github.com/ys-ll/uniterm). Unlike that one, every platform starts a login
// shell, and closing kills the shell's whole process group and reaps it.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

const loginShellID = "login"

// ListLocalShells offers the user's login shell.
func ListLocalShells() []LocalShell {
	return []LocalShell{{ID: loginShellID, Name: filepath.Base(loginShell())}}
}

// loginShell is $SHELL when it names an executable by absolute path, else
// /bin/sh.
func loginShell() string {
	sh := os.Getenv("SHELL")
	if fi, err := os.Stat(sh); err == nil && filepath.IsAbs(sh) && !fi.IsDir() && fi.Mode()&0o111 != 0 {
		return sh
	}
	return "/bin/sh"
}

// OpenLocal starts the shell named by id (from ListLocalShells) in the home
// directory, on a PTY of cols x rows.
func OpenLocal(id string, cols, rows int) (Transport, error) {
	if id != loginShellID {
		return nil, errors.New("unknown local shell")
	}
	sh := loginShell()
	cmd := exec.Command(sh)
	cmd.Args[0] = "-" + filepath.Base(sh) // a leading dash makes it a login shell
	cmd.Env = terminalEnv(os.Environ())
	if home, err := os.UserHomeDir(); err == nil {
		cmd.Dir = home
	}
	return start(cmd, cols, rows)
}

// OpenCommand starts the program at path with args and env on a PTY, as a
// local container engine's exec -it. No shell reads the arguments.
func OpenCommand(path string, args, env []string, cols, rows int) (Transport, error) {
	cmd := exec.Command(path, args...)
	cmd.Env = terminalEnv(env)
	return start(cmd, cols, rows)
}

func start(cmd *exec.Cmd, cols, rows int) (Transport, error) {
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, err
	}
	l := &localShell{cmd: cmd, pty: f, done: make(chan struct{})}
	go func() {
		cmd.Wait()
		close(l.done)
	}()
	return l, nil
}

type localShell struct {
	cmd  *exec.Cmd
	pty  *os.File
	done chan struct{} // closed once the shell is reaped

	mu     sync.Mutex
	closed bool
}

// Read returns the shell's output; it ends when the shell and everything
// holding its terminal exit, or when Close runs.
func (l *localShell) Read(b []byte) (int, error) { return l.pty.Read(b) }

func (l *localShell) Write(b []byte) (int, error) { return l.pty.Write(b) }

func (l *localShell) Resize(cols, rows int) error {
	return pty.Setsize(l.pty, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

// Close hangs up the terminal, which signals its foreground job, then ends
// the shell's process group: SIGHUP first so the shell can hang up its own
// jobs, SIGKILL if it is still there after two seconds.
func (l *localShell) Close() error {
	l.mu.Lock()
	first := !l.closed
	l.closed = true
	l.mu.Unlock()
	if !first {
		return nil
	}
	err := l.pty.Close()
	pgid := -l.cmd.Process.Pid // the shell leads its own session and group
	syscall.Kill(pgid, syscall.SIGHUP)
	select {
	case <-l.done:
	case <-time.After(2 * time.Second):
		syscall.Kill(pgid, syscall.SIGKILL)
		select {
		case <-l.done:
		case <-time.After(2 * time.Second):
		}
	}
	return err
}

func (l *localShell) EndReason() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return "session closed"
	}
	return "the shell exited"
}
