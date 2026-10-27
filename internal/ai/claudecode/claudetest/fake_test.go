package claudetest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) { Main(m) }

func run(t *testing.T, f *Fake, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(f.Path, args...)
	cmd.Stdin = strings.NewReader(stdin)
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	if x, ok := err.(*exec.ExitError); ok {
		code = x.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return o.String(), e.String(), code
}

func TestFakeReplaysEachFixtureByteForByteWithItsExitCode(t *testing.T) {
	cases := []struct {
		fixture string
		exit    int
		stderr  string
	}{
		{"plain.jsonl", 0, ""},
		{"tool_block.jsonl", 0, ""},
		{"signed_out.jsonl", 1, ""},
		{"rate_limit.jsonl", 1, ""},
		{"api_retry.jsonl", 1, ""},
		{"killed_midstream.jsonl", 1, ""},
		{"unknown_option.jsonl", 1, "error: unknown option '--x'\n"},
	}
	for _, c := range cases {
		t.Run(c.fixture, func(t *testing.T) {
			f := Install(t, Script{Steps: []Step{{Fixture: Fixture(c.fixture), Exit: c.exit, Stderr: c.stderr}}})
			want, err := os.ReadFile(Fixture(c.fixture))
			if err != nil {
				t.Fatal(err)
			}
			out, errOut, code := run(t, f, "prompt text", "-p")
			if out != string(want) {
				t.Fatalf("stdout differs from %s", c.fixture)
			}
			if code != c.exit || errOut != c.stderr {
				t.Fatalf("exit %d stderr %q, want %d %q", code, errOut, c.exit, c.stderr)
			}
		})
	}
}

func TestFakeRecordsHowItWasStarted(t *testing.T) {
	f := Install(t, Script{Steps: []Step{{Stdout: "{}\n"}}})
	run(t, f, "the prompt", "-p", "--model", "haiku")
	run(t, f, "", "--version")
	run(t, f, "", "auth", "status")
	calls := f.Calls(t)
	if len(calls) != 3 || calls[0].Kind != "model" || calls[1].Kind != "version" || calls[2].Kind != "auth" {
		t.Fatalf("calls = %+v", calls)
	}
	m := f.ModelCalls(t)
	if len(m) != 1 || m[0].Stdin != "the prompt" || strings.Join(m[0].Args, " ") != "-p --model haiku" || len(m[0].Env) == 0 {
		t.Fatalf("model call = %+v", m)
	}
}

func TestFakeAnswersVersionAndAuthStatus(t *testing.T) {
	auth, _ := os.ReadFile(Fixture("auth_status_signed_out.json"))
	f := Install(t, Script{Version: "9.9.9 (Claude Code)", Auth: string(auth), AuthExit: 0})
	if out, _, _ := run(t, f, "", "--version"); strings.TrimSpace(out) != "9.9.9 (Claude Code)" {
		t.Fatalf("version = %q", out)
	}
	out, _, _ := run(t, f, "", "auth", "status")
	var got struct {
		LoggedIn   bool   `json:"loggedIn"`
		AuthMethod string `json:"authMethod"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.LoggedIn || got.AuthMethod != "none" {
		t.Fatalf("auth = %q, %v", out, err)
	}
}

func TestFakeCanPauseMidStreamSoATestCanStopIt(t *testing.T) {
	f := Install(t, Script{Steps: []Step{{Fixture: Fixture("tool_block.jsonl"), PauseAfter: 3}}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, f.Path, "-p")
	cmd.Stdin = strings.NewReader("x")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := f.WaitPaused(t, 1, 10*time.Second)
	if pid != cmd.Process.Pid {
		t.Fatalf("paused pid %d, started %d", pid, cmd.Process.Pid)
	}
	lines := 0
	sc := bufio.NewScanner(stdout)
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	for sc.Scan() {
		lines++
	}
	cmd.Wait()
	if lines != 3 {
		t.Fatalf("wrote %d lines before pausing, want 3", lines)
	}
}

func TestRecordedStreamsListNoToolsMCPServersOrSkills(t *testing.T) {
	files, _ := filepath.Glob(Fixture("*.jsonl"))
	if len(files) == 0 {
		t.Fatal("no fixtures")
	}
	for _, p := range files {
		b, _ := os.ReadFile(p)
		for _, line := range bytes.Split(b, []byte("\n")) {
			var e struct {
				Type, Subtype string
				Tools         []any `json:"tools"`
				MCP           []any `json:"mcp_servers"`
				Skills        []any `json:"skills"`
				Commands      []any `json:"slash_commands"`
			}
			if json.Unmarshal(line, &e) != nil || e.Subtype != "init" {
				continue
			}
			if len(e.Tools)+len(e.MCP)+len(e.Skills)+len(e.Commands) != 0 {
				t.Errorf("%s: init lists tools, MCP servers, skills or commands", filepath.Base(p))
			}
		}
	}
}

var (
	anyEmail = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	anyUUID  = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
)

func TestRecordingsCarryNoIdentifiersOfTheMachineTheyCameFrom(t *testing.T) {
	var names []string
	if h, err := os.UserHomeDir(); err == nil && len(h) > 3 {
		names = append(names, h)
	}
	if u, err := user.Current(); err == nil && len(u.Username) > 3 {
		names = append(names, u.Username)
	}
	paths, _ := filepath.Glob(Fixture("*"))
	paths = append(paths, filepath.Join(filepath.Dir(Fixture("x")), "FINDINGS.md"))
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		for _, m := range anyEmail.FindAllString(s, -1) {
			if !strings.HasSuffix(m, "@example.invalid") && !strings.HasPrefix(m, "example-plugin@") && !strings.HasSuffix(m, "@example.com") {
				t.Errorf("%s: email-shaped %q", filepath.Base(p), m)
			}
		}
		for _, m := range anyUUID.FindAllString(s, -1) {
			if !strings.HasPrefix(m, "00000000-0000-4000-8000-") {
				t.Errorf("%s: identifier %q", filepath.Base(p), m)
			}
		}
		for _, n := range names {
			if strings.Contains(s, n) {
				t.Errorf("%s: contains the local user or home folder", filepath.Base(p))
			}
		}
		if strings.Contains(s, "/Users/") || strings.Contains(s, `\Users\`) {
			t.Errorf("%s: contains a home folder path", filepath.Base(p))
		}
	}
}

func TestReplyTagStandsForTheTagThePromptCarries(t *testing.T) {
	f := Install(t, Script{Steps: []Step{{Stdout: Reply("<{{TAG}}>x</{{TAG}}>")}}})
	out, _, _ := run(t, f, "tag is hb-0123456789ab here", "-p")
	if !strings.Contains(out, `<hb-0123456789ab>x</hb-0123456789ab>`) || strings.Contains(out, "{{TAG}}") {
		t.Fatalf("out = %q", out)
	}
}
