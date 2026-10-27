//go:build windows

package terminal

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// wslDistros names the installed WSL distributions. It is empty when WSL is
// not installed: wsl.exe is then missing, or exits with an error.
func wslDistros() []string {
	exe, err := exec.LookPath("wsl.exe")
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-l", "-q")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	// WSL_UTF8=1 would switch the output to UTF-8; keep the UTF-16 parseWSLList reads.
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(kv), "WSL_UTF8=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return parseWSLList(out)
}
