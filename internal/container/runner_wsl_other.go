//go:build !windows

package container

import (
	"context"
	"errors"
	"os/exec"

	"hostbay/internal/admin"
	"hostbay/internal/terminal"
)

func hideWindow(*exec.Cmd) {}

// WSLRunner exists only on Windows; elsewhere it refuses every command.
type WSLRunner struct{ Distro string }

var errNoWSL = errors.New("WSL runs only on Windows")

func (WSLRunner) Run(context.Context, []string, []byte) (admin.Result, error) {
	return admin.Result{}, errNoWSL
}

func (WSLRunner) PTY(context.Context, []string, int, int) (terminal.Transport, error) {
	return nil, errNoWSL
}
