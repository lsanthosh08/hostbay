//go:build windows

package container

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"

	"hostbay/internal/admin"
	"hostbay/internal/terminal"
)

func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
}

// WSLRunner runs an engine inside a WSL distribution: wsl.exe from the
// system folder with -d <distro> --exec, so the distribution's default
// shell never reads the arguments (KTD27). The caller checks that Distro
// is an installed distribution.
type WSLRunner struct{ Distro string }

func (r WSLRunner) wsl(argv []string) (string, []string) {
	return filepath.Join(os.Getenv("SystemRoot"), "System32", "wsl.exe"), append([]string{"-d", r.Distro, "--exec"}, argv...)
}

func (r WSLRunner) Run(ctx context.Context, argv []string, stdin []byte) (admin.Result, error) {
	exe, args := r.wsl(argv)
	return LocalRunner{Path: exe, Env: os.Environ()}.Run(ctx, append([]string{exe}, args...), stdin)
}

func (r WSLRunner) PTY(_ context.Context, argv []string, cols, rows int) (terminal.Transport, error) {
	exe, args := r.wsl(argv)
	return terminal.OpenCommand(exe, args, os.Environ(), cols, rows)
}
