//go:build windows

package terminal

// Local shells on Windows (R21, KTD10) learn from uniterm's
// backend/session/local_session_windows.go and wsl_session.go (Apache-2.0,
// github.com/ys-ll/uniterm). Unlike those, there is no pipe fallback: a
// shell needs ConPTY (Windows 10 1809 or later), and only a listed shell or
// WSL distribution can be started.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"

	"github.com/UserExistsError/conpty"
	"golang.org/x/sys/windows"
)

var windowsShells = []struct{ id, exe, name string }{
	{"pwsh", "pwsh.exe", "PowerShell 7"},
	{"powershell", "powershell.exe", "Windows PowerShell"},
	{"cmd", "cmd.exe", "Command Prompt"},
}

const wslPrefix = "wsl:"

// ListLocalShells offers PowerShell, CMD and each installed WSL distribution.
func ListLocalShells() []LocalShell {
	var l []LocalShell
	for _, s := range windowsShells {
		if _, err := exec.LookPath(s.exe); err == nil {
			l = append(l, LocalShell{ID: s.id, Name: s.name})
		}
	}
	for _, d := range wslDistros() {
		l = append(l, LocalShell{ID: wslPrefix + d, Name: d + " (WSL)"})
	}
	return l
}

// commandLine turns a listed shell ID into the command ConPTY runs.
func commandLine(id string) (string, error) {
	if d, ok := strings.CutPrefix(id, wslPrefix); ok {
		if !slices.Contains(wslDistros(), d) {
			return "", errors.New("unknown local shell")
		}
		exe, err := exec.LookPath("wsl.exe")
		if err != nil {
			return "", err
		}
		return syscall.EscapeArg(exe) + " -d " + syscall.EscapeArg(d) + " --cd ~", nil
	}
	for _, s := range windowsShells {
		if s.id == id {
			exe, err := exec.LookPath(s.exe)
			if err != nil {
				return "", err
			}
			return syscall.EscapeArg(exe), nil
		}
	}
	return "", errors.New("unknown local shell")
}

// OpenLocal starts the shell named by id (from ListLocalShells) in the home
// directory, on a pseudo console of cols x rows.
func OpenLocal(id string, cols, rows int) (Transport, error) {
	line, err := commandLine(id)
	if err != nil {
		return nil, err
	}
	home, _ := os.UserHomeDir()
	return start(line, os.Environ(), home, cols, rows)
}

// OpenCommand starts the program at path with args and env on a pseudo
// console, as a local container engine's exec -it. Only an .exe runs.
func OpenCommand(path string, args, env []string, cols, rows int) (Transport, error) {
	if !strings.EqualFold(filepath.Ext(path), ".exe") {
		return nil, errors.New("only an .exe can run here")
	}
	line := syscall.EscapeArg(path)
	for _, a := range args {
		line += " " + syscall.EscapeArg(a)
	}
	return start(line, env, "", cols, rows)
}

func start(line string, env []string, dir string, cols, rows int) (Transport, error) {
	if !conpty.IsConPtyAvailable() {
		return nil, errors.New("local terminals need Windows 10 version 1809 or later")
	}
	opts := []conpty.ConPtyOption{conpty.ConPtyDimensions(cols, rows), conpty.ConPtyEnv(terminalEnv(env))}
	if dir != "" {
		opts = append(opts, conpty.ConPtyWorkDir(dir))
	}
	c, err := conpty.Start(line, opts...)
	if err != nil {
		return nil, err
	}
	proc, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(c.Pid()))
	if err != nil {
		c.Close()
		return nil, err
	}
	l := &localShell{c: c, proc: proc}
	go func() {
		c.Wait(context.Background())
		// The pseudo console outlives its shell; close it so Read ends.
		l.finish(false)
	}()
	return l, nil
}

type localShell struct {
	c    *conpty.ConPty
	proc windows.Handle

	mu           sync.Mutex
	closed, done bool
}

func (l *localShell) Read(b []byte) (int, error)  { return l.c.Read(b) }
func (l *localShell) Write(b []byte) (int, error) { return l.c.Write(b) }
func (l *localShell) Resize(cols, rows int) error { return l.c.Resize(cols, rows) }

func (l *localShell) Close() error {
	l.finish(true)
	return nil
}

// finish ends the shell and closes the pseudo console, which ends every
// other process attached to it, once.
func (l *localShell) finish(byUser bool) {
	l.mu.Lock()
	if l.done {
		l.mu.Unlock()
		return
	}
	l.done, l.closed = true, byUser
	l.mu.Unlock()
	windows.TerminateProcess(l.proc, 1)
	l.c.Close()
	windows.CloseHandle(l.proc)
}

func (l *localShell) EndReason() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return "session closed"
	}
	return "the shell exited"
}
