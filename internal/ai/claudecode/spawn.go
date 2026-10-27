package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// modelArgs are the flags of every model call (KTD2, with the changes
// testdata/FINDINGS.md records): nothing from Claude Code's own
// configuration or tools reaches the call. The system prompt here is a fixed
// constant; Hostbay's own goes in on stdin with the conversation, so nothing
// about the host or the call shows in a process list.
func modelArgs(model string) []string {
	return []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--include-hook-events",
		"--no-session-persistence",
		"--safe-mode",
		"--setting-sources", "local",
		"--tools", "",
		"--strict-mcp-config",
		"--disable-slash-commands",
		"--permission-mode", "dontAsk",
		"--permission-prompts", "none",
		"--system-prompt", fixedSystemPrompt,
		"--model", model,
	}
}

const fixedSystemPrompt = "You are the model behind an application. Follow the instructions in the user message exactly."

const (
	probeTimeout = 15 * time.Second
	idleTimeout  = 3 * time.Minute // with no output, the call is stopped
	killWait     = 2 * time.Second
	maxStderr    = 16 << 10
)

// proc is a started program and a way to end it and everything it started.
type proc struct {
	cmd    *exec.Cmd
	kill   func()
	done   chan struct{} // closed when the program was waited for
	stderr *limitedBuffer
}

type limitedBuffer struct{ b bytes.Buffer }

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := maxStderr - l.b.Len(); room > 0 {
		l.b.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// start runs program with args in a new empty private folder, which end
// removes. The returned reader is its stdout.
func start(ctx context.Context, program string, args []string, stdin string, env []string) (*proc, io.Reader, func(), error) {
	dir, err := os.MkdirTemp("", "hostbay-claude-")
	if err != nil {
		return nil, nil, nil, err
	}
	cleanup := func() { os.RemoveAll(dir) }
	cmd := exec.Command(program, args...)
	cmd.Dir = dir
	cmd.Env = env
	if stdin != "" { // otherwise the null device, closed at once
		cmd.Stdin = strings.NewReader(stdin)
	}
	p := &proc{cmd: cmd, stderr: &limitedBuffer{}, done: make(chan struct{})}
	cmd.Stderr = p.stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		cleanup()
		return nil, nil, nil, err
	}
	prepare(cmd)
	if err := cmd.Start(); err != nil {
		cleanup()
		return nil, nil, nil, err
	}
	p.kill = track(cmd)
	go func() { // ctx ending ends the program and everything it started
		select {
		case <-ctx.Done():
			p.kill()
		case <-p.done:
		}
	}()
	return p, out, cleanup, nil
}

// wait returns the program's exit code, or -1 when it did not exit
// normally, once its output is read. It then sweeps anything it left behind.
func (p *proc) wait() int {
	err := p.cmd.Wait()
	close(p.done)
	p.kill()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		return exit.ExitCode()
	}
	return -1
}

// run runs a short command (--version, auth status) and returns its stdout.
func run(ctx context.Context, program string, args []string, env []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	p, out, cleanup, err := start(ctx, program, args, "", env)
	if err != nil {
		return "", err
	}
	defer cleanup()
	b, _ := io.ReadAll(io.LimitReader(out, 1<<20))
	if code := p.wait(); code != 0 {
		return string(b), fmt.Errorf("exit %d", code)
	}
	return string(b), nil
}

// Status is what `claude auth status` says; everything else in it, such as
// the account's e-mail address, is discarded unread.
type Status struct {
	LoggedIn    bool
	AuthMethod  string
	APIProvider string
}

// firstParty reports a sign-in to Claude itself, not to another provider.
func (s Status) firstParty() bool { return s.APIProvider == "" || s.APIProvider == "firstParty" }

func parseStatus(out string) (Status, error) {
	var s struct {
		LoggedIn    bool   `json:"loggedIn"`
		AuthMethod  string `json:"authMethod"`
		APIProvider string `json:"apiProvider"`
	}
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		return Status{}, errors.New("Claude Code's sign-in status cannot be read")
	}
	return Status(s), nil
}
