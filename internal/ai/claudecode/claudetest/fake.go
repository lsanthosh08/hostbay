// Package claudetest is a stand-in for the `claude` program in tests. It
// re-executes the test binary under the name claude, so no test runs the
// person's real program or touches their sign-in. The fake answers
// `--version` and `auth status`, and replays a recorded stream-json file
// for a model call, recording how it was started.
//
// A package that uses it calls Main from its own TestMain.
package claudetest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Step is one model call. It replays Fixture (or Stdout) byte for byte,
// writes Stderr, and exits with Exit.
type Step struct {
	Fixture string `json:"fixture,omitempty"` // path of a stream-json file
	Stdout  string `json:"stdout,omitempty"`
	Stderr  string `json:"stderr,omitempty"`
	Exit    int    `json:"exit,omitempty"`
	// PauseAfter blocks the call after that many lines were written, until
	// it is killed, so a test can press Stop mid-stream. 0 never pauses.
	PauseAfter int `json:"pauseAfter,omitempty"`
}

// Script says how the fake behaves. The last step answers every call after
// it; with no steps a model call prints nothing and exits 0.
type Script struct {
	Version     string `json:"version,omitempty"`     // default "2.1.310 (Claude Code)"
	VersionExit int    `json:"versionExit,omitempty"` //
	Auth        string `json:"auth,omitempty"`        // printed by `auth status`; default signed in
	AuthExit    int    `json:"authExit,omitempty"`
	Steps       []Step `json:"steps,omitempty"`
}

// Call is what one start of the fake was given.
type Call struct {
	Kind  string // "version", "auth" or "model"
	Args  []string
	Env   []string
	Cwd   string
	Stdin string
}

const (
	scriptFile = "script.json"
	callsDir   = "calls"
	defVersion = "2.1.310 (Claude Code)"
	defAuth    = `{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty"}`
)

// Fake is an installed fake program.
type Fake struct {
	Path string // the executable named claude
	Dir  string // its folder, which also holds the script and the call log
}

// Main runs the tests, or the fake when this binary was started under the
// name claude.
func Main(m *testing.M) {
	if isFake() {
		os.Exit(runFake())
	}
	os.Exit(m.Run())
}

func isFake() bool {
	name := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	return name == "claude"
}

// Install puts a fake claude, scripted by s, in a new folder.
func Install(t testing.TB, s Script) *Fake {
	t.Helper()
	dir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	name := "claude"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	f := &Fake{Path: filepath.Join(dir, name), Dir: dir}
	// A copy named claude: a symlink would resolve to the test binary's own
	// name, and a hard link would share its mode with the test binary.
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.Path, b, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, callsDir), 0o755); err != nil {
		t.Fatal(err)
	}
	f.Set(t, s)
	return f
}

// Set replaces the script; calls already made stay logged.
func (f *Fake) Set(t testing.TB, s Script) {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.Dir, scriptFile), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Calls returns every start, in order.
func (f *Fake) Calls(t testing.TB) []Call {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(f.Dir, callsDir, "*.json"))
	sort.Strings(files)
	var out []Call
	for _, p := range files {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var c Call
		if err := json.Unmarshal(b, &c); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

// ModelCalls returns the starts that were model calls.
func (f *Fake) ModelCalls(t testing.TB) []Call {
	t.Helper()
	var out []Call
	for _, c := range f.Calls(t) {
		if c.Kind == "model" {
			out = append(out, c)
		}
	}
	return out
}

// WaitPaused waits for model call n (1-based) to stop at its PauseAfter line
// and returns its process id.
func (f *Fake) WaitPaused(t testing.TB, n int, within time.Duration) int {
	t.Helper()
	pidFile := filepath.Join(f.Dir, fmt.Sprintf("paused-%d", n))
	for end := time.Now().Add(within); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				return pid
			}
		}
	}
	t.Fatalf("model call %d did not pause within %s", n, within)
	return 0
}

// Fixture is the path of a recorded file in testdata.
func Fixture(name string) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "testdata", name)
}

func runFake() int {
	dir := filepath.Dir(os.Args[0])
	var s Script
	b, err := os.ReadFile(filepath.Join(dir, scriptFile))
	if err == nil {
		err = json.Unmarshal(b, &s)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake claude: no script:", err)
		return 99
	}
	args := os.Args[1:]
	kind := "model"
	switch {
	case len(args) == 1 && (args[0] == "--version" || args[0] == "-v"):
		kind = "version"
	case len(args) >= 2 && args[0] == "auth" && args[1] == "status":
		kind = "auth"
	}
	var stdin []byte
	if kind == "model" {
		stdin, _ = io.ReadAll(os.Stdin)
	}
	n := logCall(dir, Call{Kind: kind, Args: args, Env: os.Environ(), Cwd: mustGetwd(), Stdin: string(stdin)})
	switch kind {
	case "version":
		v := s.Version
		if v == "" {
			v = defVersion
		}
		fmt.Println(v)
		return s.VersionExit
	case "auth":
		a := s.Auth
		if a == "" {
			a = defAuth
		}
		fmt.Println(a)
		return s.AuthExit
	}
	return replay(dir, s, n, stdin)
}

// logCall writes the call under the next free number and returns the
// number of model calls so far when it is one (the call's step index + 1).
func logCall(dir string, c Call) int {
	b, _ := json.Marshal(c)
	for i := 1; ; i++ {
		p := filepath.Join(dir, callsDir, fmt.Sprintf("%04d.json", i))
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake claude: cannot log the call:", err)
			os.Exit(99)
		}
		f.Write(b)
		f.Close()
		break
	}
	files, _ := filepath.Glob(filepath.Join(dir, callsDir, "*.json"))
	model := 0
	for _, p := range files {
		raw, _ := os.ReadFile(p)
		if bytes.Contains(raw, []byte(`"Kind":"model"`)) {
			model++
		}
	}
	return model
}

var tagIn = regexp.MustCompile(`hb-[0-9a-f]{12}`)

// replay answers model call n. "{{TAG}}" in the recording stands for the
// call's own random tag, which only the prompt on stdin tells the program.
func replay(dir string, s Script, n int, stdin []byte) int {
	if len(s.Steps) == 0 {
		return 0
	}
	step := s.Steps[min(n, len(s.Steps))-1]
	out := []byte(step.Stdout)
	if step.Fixture != "" {
		b, err := os.ReadFile(step.Fixture)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake claude:", err)
			return 99
		}
		out = b
	}
	if tag := tagIn.Find(stdin); tag != nil {
		out = bytes.ReplaceAll(out, []byte("{{TAG}}"), tag)
	}
	r := bufio.NewReader(bytes.NewReader(out))
	for lines := 0; ; {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			os.Stdout.Write(line)
			lines++
			if step.PauseAfter > 0 && lines == step.PauseAfter {
				pause(dir, n)
				return 1
			}
		}
		if err != nil {
			break
		}
	}
	if step.Stderr != "" {
		fmt.Fprint(os.Stderr, step.Stderr)
	}
	return step.Exit
}

// pause publishes the process id, then waits until the test kills the call.
func pause(dir string, n int) {
	p := filepath.Join(dir, fmt.Sprintf("paused-%d", n))
	os.WriteFile(p+".tmp", []byte(strconv.Itoa(os.Getpid())), 0o644)
	os.Rename(p+".tmp", p)
	time.Sleep(time.Minute)
}

// Reply is a stream-json answer that says text, for a Step's Stdout. Put
// "{{TAG}}" where the call's tag belongs to write a tool block.
func Reply(text string) string {
	line := func(v any) string {
		var b strings.Builder
		e := json.NewEncoder(&b) // keeps < and > as typed, so a test can read the tag
		e.SetEscapeHTML(false)
		e.Encode(v)
		return b.String()
	}
	return line(map[string]any{"type": "system", "subtype": "init", "claude_code_version": "2.1.310", "tools": []string{}, "mcp_servers": []string{}, "skills": []string{}, "slash_commands": []string{}}) +
		line(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}}}) +
		line(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}}}}) +
		line(map[string]any{"type": "result", "subtype": "success", "is_error": false, "stop_reason": "end_turn"})
}

func mustGetwd() string {
	d, _ := os.Getwd()
	return d
}
