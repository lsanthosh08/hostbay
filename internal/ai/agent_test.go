package ai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"hostbay/internal/agentexec"
	"hostbay/internal/ai/aitest"
	"hostbay/internal/store"
	"hostbay/internal/terminal"
)

// fakeShell is a remote shell the test drives.
type fakeShell struct {
	r   *io.PipeReader
	out *io.PipeWriter
	mu  sync.Mutex
	in  bytes.Buffer
}

func (s *fakeShell) Read(b []byte) (int, error) { return s.r.Read(b) }
func (s *fakeShell) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.in.Write(b)
}
func (s *fakeShell) Resize(int, int) error { return nil }
func (s *fakeShell) Close() error          { return s.r.Close() }
func (s *fakeShell) got() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.in.String()
}

var nonceIn = regexp.MustCompile(`'hostbay-done' '([0-9a-f]+)'`)

// finish prints output and the last envelope's marker.
func (s *fakeShell) finish(t *testing.T, output string) {
	t.Helper()
	waitUntil(t, func() bool { return nonceIn.MatchString(s.got()) })
	m := nonceIn.FindAllStringSubmatch(s.got(), -1)
	s.out.Write([]byte(output + "\x1b]777;hostbay-done;" + m[len(m)-1][1] + ";0\a$ "))
}

func waitUntil(t *testing.T, ok func() bool) {
	t.Helper()
	for range 300 {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out")
}

type terms struct {
	reg  *terminal.Registry
	mu   sync.Mutex
	info map[string]agentexec.TermInfo
}

func (t *terms) Info(id string) (agentexec.TermInfo, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	i, ok := t.info[id]
	if !ok {
		return i, errors.New("closed")
	}
	return i, nil
}
func (t *terms) Write(id string, p []byte) error                     { return t.reg.Write(id, p) }
func (t *terms) Recent(id string) ([]byte, error)                    { return t.reg.Recent(id) }
func (t *terms) Subscribe(id string) (*terminal.Subscription, error) { return t.reg.Subscribe(id) }

type harness struct {
	t      *testing.T
	srv    *aitest.Server
	agent  *Agent
	terms  *terms
	mu     sync.Mutex
	events []Event
}

const sampleKey = "sk-ant-api03-agent-test-key"

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, srv: aitest.New(t), terms: &terms{info: map[string]agentexec.TermInfo{}}}
	var a *Agent
	h.terms.reg = terminal.NewRegistry(func(string, string) {}, func(id, _ string) {
		h.terms.mu.Lock()
		delete(h.terms.info, id)
		h.terms.mu.Unlock()
		a.Exec.TerminalClosed(id)
		a.TerminalClosed(id)
	})
	exec := agentexec.New(h.terms, func(string) agentexec.Policy { return agentexec.Policy{Mode: store.AIConfirmAll} })
	exec.Wait = 2 * time.Second
	a = &Agent{
		Exec: exec,
		Emit: func(_ string, ev Event) { h.mu.Lock(); h.events = append(h.events, ev); h.mu.Unlock() },
		Provider: func(string) (Provider, string, error) {
			p, err := New(Anthropic, h.srv.URL, sampleKey, "Hostbay/test", http.DefaultClient)
			return p, "claude-opus-5-5", err
		},
		Info:    h.terms.Info,
		Secrets: func(string) []string { return []string{"S3cret-Login-Pass"} },
	}
	h.agent = a
	return h
}

func (h *harness) open() (string, *fakeShell) {
	r, w := io.Pipe()
	s := &fakeShell{r: r, out: w}
	id := h.terms.reg.Open("owner", s)
	h.terms.reg.Attach(id)
	h.terms.mu.Lock()
	h.terms.info[id] = agentexec.TermInfo{HostID: "web", Label: "web (root@10.0.0.5)", Kind: "ssh", Shell: agentexec.ShellPOSIX}
	h.terms.mu.Unlock()
	h.t.Cleanup(func() { h.terms.reg.Close(id) })
	return id, s
}

func (h *harness) card() agentexec.Pending {
	h.t.Helper()
	var l []agentexec.Pending
	waitUntil(h.t, func() bool { l = h.agent.Exec.Approvals.List(); return len(l) == 1 })
	return l[0]
}

func (h *harness) idle() {
	h.t.Helper()
	time.Sleep(20 * time.Millisecond)
	waitUntil(h.t, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return len(h.events) > 0 && h.events[len(h.events)-1].Type == "state" && h.events[len(h.events)-1].State == "idle"
	})
}

func (h *harness) saw(typ string) *Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.events {
		if h.events[i].Type == typ {
			return &h.events[i]
		}
	}
	return nil
}

// toolResult is the text of the tool_result the n-th request carried.
func (h *harness) toolResult(n int) string {
	h.t.Helper()
	var body struct {
		Messages []struct {
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(h.srv.Requests()[n].Body, &body); err != nil {
		h.t.Fatal(err)
	}
	last := body.Messages[len(body.Messages)-1].Content[0]
	if last["type"] != "tool_result" {
		h.t.Fatalf("request %d ends with %v", n, last)
	}
	return last["content"].(string)
}

// AE15 end to end: the model proposes ls; nothing reaches the shell until
// the user approves; then its output goes back to the model, scrubbed of
// the host's password.
func TestAgentRunsAProposalOnlyAfterApproval(t *testing.T) {
	h := newHarness(t)
	id, sh := h.open()
	h.srv.Push(aitest.Reply{Body: aitest.AnthropicTool("toolu_1", "run_command", aitest.Command("ls"))}, aitest.Reply{Body: aitest.AnthropicText("Two files.")})
	conv := h.agent.Start("p1", id)
	if err := h.agent.Send(conv, "what is here?"); err != nil {
		t.Fatal(err)
	}
	card := h.card()
	if sh.got() != "" || card.Bytes != "ls" || card.Host != "web (root@10.0.0.5)" {
		t.Fatalf("before approval the shell got %q; card %+v", sh.got(), card)
	}
	h.agent.Exec.Approvals.Answer(card.ID, true)
	sh.finish(t, "a.txt  S3cret-Login-Pass\r\n")
	h.idle()
	if res := h.toolResult(1); !strings.Contains(res, "exit status 0") || !strings.Contains(res, "a.txt") || strings.Contains(res, "S3cret") {
		t.Fatalf("tool result = %q", res)
	}
	for _, r := range h.srv.Requests() {
		if bytes.Contains(r.Body, []byte("S3cret-Login-Pass")) {
			t.Fatal("the host's password reached the provider")
		}
	}
	if ev := h.saw("result"); ev == nil || ev.State != "done" {
		t.Fatalf("result event = %+v", ev)
	}
}

// The provider fails after half a tool call: nothing runs, the error comes
// with Retry, and the key is in no event; Retry then completes the turn.
func TestAgentProviderErrorRunsNothingAndRetries(t *testing.T) {
	h := newHarness(t)
	id, sh := h.open()
	h.srv.Push(aitest.Reply{Body: aitest.AnthropicError("overloaded_error", "Overloaded")}, aitest.Reply{Status: 529, Body: aitest.Overloaded})
	conv := h.agent.Start("p1", id)
	h.agent.Send(conv, "hi")
	h.idle()
	ev := h.saw("error")
	if ev == nil || !ev.Retry || !strings.Contains(ev.Text, "overloaded") || sh.got() != "" || len(h.agent.Exec.Approvals.List()) != 0 {
		t.Fatalf("error = %+v, shell %q", ev, sh.got())
	}
	h.agent.Retry(conv)
	h.idle()
	h.srv.Push(aitest.Reply{Body: aitest.AnthropicText("hello")})
	h.agent.Retry(conv)
	h.idle()
	raw, _ := json.Marshal(h.events)
	if bytes.Contains(raw, []byte(sampleKey)) || !bytes.Contains(raw, []byte("hello")) || !bytes.Contains(raw, []byte("529")) {
		t.Fatalf("events = %s", raw)
	}
}

// Stop while streaming ends the turn and no tool call from the partial
// reply runs; Stop while a card waits withdraws it.
func TestAgentStopEndsTheTurn(t *testing.T) {
	h := newHarness(t)
	id, sh := h.open()
	hold := make(chan struct{})
	defer close(hold)
	h.srv.Push(aitest.Reply{Body: aitest.AnthropicTool("toolu_1", "run_command", aitest.Command("ls")), Before: hold})
	conv := h.agent.Start("p1", id)
	h.agent.Send(conv, "go")
	waitUntil(t, func() bool { return len(h.srv.Requests()) == 1 })
	time.Sleep(50 * time.Millisecond)
	h.agent.Stop(conv)
	h.idle()
	if h.saw("stopped") == nil || h.saw("command") != nil || sh.got() != "" {
		t.Fatalf("events %+v", h.events)
	}

	h.srv.Push(aitest.Reply{Body: aitest.AnthropicTool("toolu_2", "run_command", aitest.Command("ls"))})
	h.agent.Send(conv, "again")
	h.card()
	h.agent.Stop(conv)
	h.idle()
	if len(h.agent.Exec.Approvals.List()) != 0 || sh.got() != "" {
		t.Fatal("the card stayed or the command ran")
	}
}

// When the pinned terminal closes the conversation stops and never
// retargets, even though another terminal is active.
func TestAgentPinnedTerminalClosingStopsIt(t *testing.T) {
	h := newHarness(t)
	id, sh := h.open()
	other, osh := h.open()
	h.agent.SetActive(other)
	h.srv.Push(aitest.Reply{Body: aitest.AnthropicTool("toolu_1", "run_command", aitest.Command("make install"))})
	conv := h.agent.Start("p1", id)
	h.agent.Send(conv, "build")
	h.agent.Exec.Approvals.Answer(h.card().ID, true)
	waitUntil(t, func() bool { return nonceIn.MatchString(sh.got()) })
	h.terms.reg.Close(id)
	h.idle()
	if h.saw("targetClosed") == nil || osh.got() != "" {
		t.Fatalf("closed event missing or the other shell got %q", osh.got())
	}
	if err := h.agent.Send(conv, "again"); err == nil {
		t.Fatal("a conversation whose terminal closed took a message")
	}
}

// Following the active terminal, the connection drops mid-command: after
// reconnect the model is told the outcome is unknown, and the command is
// not sent to the new shell.
func TestAgentDropLeavesTheOutcomeUnknown(t *testing.T) {
	h := newHarness(t)
	id, sh := h.open()
	h.agent.SetActive(id)
	h.srv.Push(aitest.Reply{Body: aitest.AnthropicTool("toolu_1", "run_command", aitest.Command("make install"))}, aitest.Reply{Body: aitest.AnthropicText("It may not have finished.")})
	conv := h.agent.Start("p1", "")
	h.agent.Send(conv, "build")
	h.agent.Exec.Approvals.Answer(h.card().ID, true)
	waitUntil(t, func() bool { return nonceIn.MatchString(sh.got()) })
	sh.out.CloseWithError(io.ErrUnexpectedEOF) // the connection drops
	fresh, fsh := h.open()                     // the reconnected shell
	h.agent.SetActive(fresh)
	h.idle()
	if res := h.toolResult(1); !strings.Contains(res, "outcome is unknown") {
		t.Fatalf("tool result = %q", res)
	}
	if fsh.got() != "" || h.saw("targetClosed") != nil {
		t.Fatalf("the new shell got %q", fsh.got())
	}
}

func historySize(h []Message) int {
	n := 0
	for _, m := range h {
		for _, b := range m.Content {
			n += len(b.Text) + len(b.Input) + len(b.Opaque)
		}
	}
	return n
}

// Every call resends the conversation, so it must stay within what a
// provider takes: past the budget the oldest command output goes first,
// each tool call keeping its result, then the oldest turns.
func TestAgentHistoryStaysWithinItsBudget(t *testing.T) {
	const budget = 400 << 10
	a := &Agent{}
	c := &conv{}
	a.add(c, Message{Role: "user", Content: []Block{{Type: "text", Text: "read the logs"}}})
	for i := range 60 {
		id := fmt.Sprint("call", i)
		a.add(c, Message{Role: "assistant", Content: []Block{{Type: "tool_use", ToolID: id, Name: "run_command", Input: json.RawMessage(`{"command":"cat log"}`)}}})
		a.add(c, Message{Role: "user", Content: []Block{{Type: "tool_result", ToolID: id, Text: id + strings.Repeat("x", 16<<10)}}})
	}
	h := a.history(c)
	if n := historySize(h); n > budget {
		t.Fatalf("history is %d bytes, over %d", n, budget)
	}
	if h[0].Role != "user" || h[0].Content[0].Text != "read the logs" {
		t.Fatalf("first message = %+v", h[0])
	}
	last := h[len(h)-1].Content[0]
	if !strings.HasPrefix(last.Text, "call59x") || strings.HasPrefix(h[2].Content[0].Text, "call0x") {
		t.Fatal("the newest output was dropped or the oldest kept")
	}
	for i, m := range h {
		for _, b := range m.Content {
			if b.Type == "tool_result" && (i == 0 || h[i-1].Content[0].ToolID != b.ToolID) {
				t.Fatalf("result %s lost its call", b.ToolID)
			}
		}
	}

	c = &conv{}
	for i := range 10 {
		a.add(c, Message{Role: "user", Content: []Block{{Type: "text", Text: fmt.Sprint("q", i, strings.Repeat("y", 100<<10))}}})
		a.add(c, Message{Role: "assistant", Content: []Block{{Type: "text", Text: "ok"}}})
	}
	h = a.history(c)
	if n := historySize(h); n > budget || h[0].Role != "user" || !strings.HasPrefix(h[len(h)-2].Content[0].Text, "q9") {
		t.Fatalf("long turns: %d bytes, first %s, want within %d and the last turn kept", n, h[0].Role, budget)
	}
}

// A conversation whose terminal closed can never be used again: its
// history is freed then, not when the page gets round to closing it.
func TestAgentClosedConversationFreesItsHistory(t *testing.T) {
	h := newHarness(t)
	id, _ := h.open()
	conv := h.agent.Start("p", id)
	h.agent.add(h.agent.convs[conv], Message{Role: "user", Content: []Block{{Type: "text", Text: "hi"}}})
	h.agent.TerminalClosed(id)
	if got := h.agent.history(h.agent.convs[conv]); len(got) != 0 {
		t.Fatalf("closed conversation still holds %d messages", len(got))
	}
}
