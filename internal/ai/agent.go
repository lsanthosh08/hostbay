package ai

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"

	"hostbay/internal/agentexec"
	"hostbay/internal/apperr"
	"hostbay/internal/store"
)

// maxSteps bounds one turn's model calls, so a model that keeps calling
// tools cannot spin forever.
const maxSteps = 40

// Event is what a conversation's page receives. Model text and tool output
// are plain text; the page renders them as text, never as HTML.
type Event struct {
	Type    string            `json:"type"` // "text", "command", "skill", "result", "error", "state", "stopped", "targetClosed"
	Text    string            `json:"text,omitempty"`
	ToolID  string            `json:"toolId,omitempty"`
	Command string            `json:"command,omitempty"`
	Host    string            `json:"host,omitempty"`
	State   string            `json:"state,omitempty"` // state: "thinking", "working", "idle"; result: the job's state or "refused"/"denied"
	Retry   bool              `json:"retry,omitempty"`
	Code    string            `json:"code,omitempty"` // an error's apperr code
	Details map[string]string `json:"details,omitempty"`
	// A "skill" event is a save_skill proposal: Name, and its body in
	// Text. Its "result" has State "saved", "denied" or "refused".
	Name     string `json:"name,omitempty"`
	Replaces bool   `json:"replaces,omitempty"`
}

// Agent holds the conversations, one per sidebar, in memory only: they are
// never written to disk (KTD31).
type Agent struct {
	Exec *agentexec.Executor
	Emit func(conv string, ev Event)
	// Provider opens provider id: its client and model. The key stays in
	// the client.
	Provider func(id string) (Provider, string, error)
	Info     func(terminal string) (agentexec.TermInfo, error)
	// Secrets lists what must never reach the provider from terminal's
	// output: its host's login and sudo password, key passphrase and
	// post-login answers.
	Secrets func(terminal string) []string
	// Skills finds the saved skill or template /name names, or nil.
	Skills func(name string) *store.Skill
	// SaveSkill stores an approved save_skill proposal, replacing a skill
	// with the same name.
	SaveSkill func(store.Skill) error

	mu     sync.Mutex
	active string // the focused terminal, which follow-active conversations target
	convs  map[string]*conv
}

type conv struct {
	id, provider, pinned string
	history              []Message
	cancel               context.CancelFunc
	busy, closed         bool
}

var errBusy = errors.New("the assistant is still answering; stop it first")

func newConvID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Start opens a conversation with provider, pinned to terminal, or
// following the active terminal when pinned is "".
func (a *Agent) Start(provider, pinned string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.convs == nil {
		a.convs = map[string]*conv{}
	}
	c := &conv{id: newConvID(), provider: provider, pinned: pinned}
	a.convs[c.id] = c
	return c.id
}

// SetActive records the terminal the page focused.
func (a *Agent) SetActive(terminal string) {
	a.mu.Lock()
	a.active = terminal
	a.mu.Unlock()
}

func (a *Agent) get(id string) (*conv, error) {
	c := a.convs[id]
	if c == nil {
		return nil, errors.New("this conversation has ended; start a new one")
	}
	if c.closed {
		return nil, errors.New("this conversation's terminal closed; start a new conversation")
	}
	return c, nil
}

// Send adds the user's message and runs a turn.
func (a *Agent) Send(id, text string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	c, err := a.get(id)
	if err != nil {
		return err
	}
	if c.busy {
		return errBusy
	}
	c.history = trim(append(c.history, Message{Role: "user", Content: Expand(text, a.Skills)}))
	a.begin(c)
	return nil
}

// Retry runs the turn again after a provider error; nothing from the
// failed reply was kept.
func (a *Agent) Retry(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	c, err := a.get(id)
	if err != nil {
		return err
	}
	if c.busy || len(c.history) == 0 || c.history[len(c.history)-1].Role != "user" {
		return errors.New("there is nothing to retry")
	}
	a.begin(c)
	return nil
}

// begin starts a turn; a.mu is held.
func (a *Agent) begin(c *conv) {
	ctx, cancel := context.WithCancel(context.Background())
	c.busy, c.cancel = true, cancel
	go a.turn(ctx, c)
}

// Stop ends the turn: the stream stops, a waiting card is withdrawn and a
// running command gets one Ctrl-C.
func (a *Agent) Stop(id string) {
	a.mu.Lock()
	c := a.convs[id]
	var cancel context.CancelFunc
	if c != nil {
		cancel = c.cancel
	}
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	a.Exec.Stop(id)
}

// Close stops and forgets a conversation.
func (a *Agent) Close(id string) {
	a.Stop(id)
	a.mu.Lock()
	delete(a.convs, id)
	a.mu.Unlock()
}

// Done ends the conversation's running command that has no envelope.
func (a *Agent) Done(id string) error { return a.Exec.Done(id) }

// TerminalClosed stops every conversation pinned to terminal; they never
// retarget.
func (a *Agent) TerminalClosed(terminal string) {
	a.mu.Lock()
	var stopped []string
	for _, c := range a.convs {
		if c.pinned == terminal && !c.closed {
			c.closed, c.history = true, nil // it can never be used again
			stopped = append(stopped, c.id)
		}
	}
	if a.active == terminal {
		a.active = ""
	}
	a.mu.Unlock()
	for _, id := range stopped {
		a.Stop(id)
		a.Emit(id, Event{Type: "targetClosed"})
	}
}

// target is the terminal a proposal goes to now: the pinned one, or the
// active one.
func (a *Agent) target(c *conv) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if c.pinned != "" {
		if c.closed {
			return ""
		}
		return c.pinned
	}
	return a.active
}

func (a *Agent) history(c *conv) []Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Message(nil), c.history...)
}

func (a *Agent) add(c *conv, m Message) {
	a.mu.Lock()
	c.history = trim(append(c.history, m))
	a.mu.Unlock()
}

// historyBudget bounds what a conversation keeps and resends on every
// call, about 100k tokens, so a long one does not outgrow the provider's
// context and fail every request after.
const historyBudget = 400 << 10

const elided = "[output removed to keep the conversation short; run the command again if it is needed]"

func msgSize(m Message) int {
	n := 0
	for _, b := range m.Content {
		n += len(b.Text) + len(b.Input) + len(b.Opaque)
	}
	return n
}

// trim keeps h within historyBudget: it replaces the oldest command output
// first, keeping every tool call paired with its result, then drops the
// oldest turns whole, so h still starts with the user's message. The last
// turn always stays.
func trim(h []Message) []Message {
	total := 0
	for _, m := range h {
		total += msgSize(m)
	}
	for i := 0; total > historyBudget && i < len(h); i++ {
		for j, b := range h[i].Content {
			if b.Type == "tool_result" && b.Text != elided && total > historyBudget {
				h[i].Content = slices.Clone(h[i].Content) // history() handed out the old one
				total -= len(b.Text) - len(elided)
				h[i].Content[j].Text = elided
			}
		}
	}
	for total > historyBudget {
		next := slices.IndexFunc(h[min(1, len(h)):], func(m Message) bool {
			return m.Role == "user" && !slices.ContainsFunc(m.Content, func(b Block) bool { return b.Type == "tool_result" })
		})
		if next < 0 {
			break
		}
		for _, m := range h[:next+1] {
			total -= msgSize(m)
		}
		h = slices.Delete(h, 0, next+1)
	}
	return h
}

const toolSaveSkill = "save_skill"

var tools = []Tool{
	{Name: agentexec.ToolRun, Description: "Run one shell command in the target terminal and return its output and state. The user sees the exact command and usually approves it first.",
		Schema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","description":"The command, on one line"}},"required":["command"]}`)},
	{Name: "read_output", Description: "Read more output of one of your commands by its job_id from offset, or with no job_id the terminal's recent screen output.",
		Schema: json.RawMessage(`{"type":"object","properties":{"job_id":{"type":"string"},"offset":{"type":"integer"}}}`)},
	{Name: "interrupt", Description: "Send Ctrl-C to your own running command.",
		Schema: json.RawMessage(`{"type":"object","properties":{}}`)},
	{Name: toolSaveSkill, Description: "Propose saving a skill: instructions the user can attach to a later request by typing /name. The user sees the name and the whole text and decides; nothing is saved unless they approve. A name already saved is replaced.",
		Schema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"Lower-case letters, digits, - and _, such as deploy-check"},"description":{"type":"string","description":"One line on what it is for"},"body":{"type":"string","description":"The instructions"}},"required":["name","body"]}`)},
}

func (a *Agent) turn(ctx context.Context, c *conv) {
	defer func() {
		a.mu.Lock()
		c.busy, c.cancel = false, nil
		a.mu.Unlock()
		a.Emit(c.id, Event{Type: "state", State: "idle"})
	}()
	for range maxSteps {
		p, model, err := a.Provider(c.provider)
		if err != nil {
			a.fail(c, err, false)
			return
		}
		var info *agentexec.TermInfo
		if t := a.target(c); t != "" {
			if i, err := a.Info(t); err == nil {
				info = &i
			}
		}
		a.Emit(c.id, Event{Type: "state", State: "thinking"})
		reply, err := p.Stream(ctx, Request{Model: model, System: systemPrompt(info), Messages: a.history(c), Tools: tools},
			func(s string) { a.Emit(c.id, Event{Type: "text", Text: s}) })
		if ctx.Err() != nil {
			a.Emit(c.id, Event{Type: "stopped"})
			return
		}
		if err != nil {
			a.fail(c, err, true)
			return
		}
		a.add(c, reply.Message)
		var results []Block
		for _, b := range reply.Message.Content {
			if b.Type != "tool_use" {
				continue
			}
			res := Block{Type: "tool_result", ToolID: b.ToolID, Text: "stopped by the user before it ran", IsError: true}
			if ctx.Err() == nil {
				a.Emit(c.id, Event{Type: "state", State: "working"})
				res.Text, res.IsError = a.tool(ctx, c, b)
			}
			results = append(results, res)
		}
		if len(results) == 0 {
			return
		}
		a.add(c, Message{Role: "user", Content: results})
		if ctx.Err() != nil {
			a.Emit(c.id, Event{Type: "stopped"})
			return
		}
	}
	a.Emit(c.id, Event{Type: "error", Text: "the assistant stopped after 40 steps; send a message to go on"})
}

// fail shows a provider or setup error. Its text is whatever the provider
// said, which the caller has scrubbed of the key.
func (a *Agent) fail(c *conv, err error, retry bool) {
	ev := Event{Type: "error", Text: err.Error(), Retry: retry}
	var ae *apperr.Error
	if errors.As(err, &ae) {
		ev.Code, ev.Details = string(ae.Code), ae.Details
	}
	a.Emit(c.id, ev)
}

// tool runs one call and returns what the model is told.
func (a *Agent) tool(ctx context.Context, c *conv, call Block) (string, bool) {
	var in struct {
		Command string `json:"command"`
		JobID   string `json:"job_id"`
		Offset  int    `json:"offset"`
		Name    string `json:"name"`
		Desc    string `json:"description"`
		Body    string `json:"body"`
	}
	if err := json.Unmarshal(call.Input, &in); err != nil {
		return "the tool input is not valid: " + err.Error(), true
	}
	target := a.target(c) // frozen into the request now
	switch call.Name {
	case agentexec.ToolRun:
		return a.runCommand(ctx, c, call.ToolID, target, in.Command)
	case "read_output":
		if in.JobID != "" {
			v, err := a.Exec.Output(c.id, in.JobID, in.Offset)
			if err != nil {
				return err.Error(), true
			}
			return a.describe(v, target), false
		}
		if target == "" {
			return "no terminal is selected", true
		}
		s, err := a.Exec.ScreenTail(target)
		if err != nil {
			return err.Error(), true
		}
		return Clean(s, a.Secrets(target)), false
	case "interrupt":
		if err := a.Exec.Interrupt(c.id); err != nil {
			return err.Error(), true
		}
		return "Ctrl-C sent", false
	case toolSaveSkill:
		return a.saveSkill(ctx, c, call.ToolID, store.Skill{Name: in.Name, Description: in.Desc, Body: in.Body, Kind: store.SkillKind, AgentAuthored: true})
	}
	return "there is no tool named " + call.Name, true
}

func (a *Agent) runCommand(ctx context.Context, c *conv, toolID, target, cmd string) (string, bool) {
	if target == "" {
		a.Emit(c.id, Event{Type: "result", ToolID: toolID, Command: cmd, State: "refused", Text: "no terminal to run it in"})
		return "No terminal is selected, so nothing ran. Ask the user to open or pick one.", true
	}
	host := ""
	if i, err := a.Info(target); err == nil {
		host = i.Label
	}
	a.Emit(c.id, Event{Type: "command", ToolID: toolID, Command: cmd, Host: host})
	v, err := a.Exec.Run(ctx, agentexec.Request{Principal: c.id, Target: target, Tool: agentexec.ToolRun, Command: cmd})
	var ref *agentexec.Refusal
	switch {
	case errors.As(err, &ref):
		a.Emit(c.id, Event{Type: "result", ToolID: toolID, State: "refused", Text: ref.Reason})
		return "Refused, nothing ran: " + ref.Reason, true
	case errors.Is(err, agentexec.ErrBusy):
		a.Emit(c.id, Event{Type: "result", ToolID: toolID, State: "refused", Text: err.Error()})
		return err.Error() + "\n" + a.describe(v, target), true
	case err != nil:
		state := "denied"
		if ctx.Err() != nil {
			state = "stopped"
		}
		a.Emit(c.id, Event{Type: "result", ToolID: toolID, State: state, Text: err.Error()})
		return err.Error(), true
	}
	out := a.describe(v, target)
	a.Emit(c.id, Event{Type: "result", ToolID: toolID, State: string(v.State), Text: Clean(v.Output, a.Secrets(target))})
	return out, false
}

// describe is a job as the model reads it.
func (a *Agent) describe(v agentexec.JobView, target string) string {
	var head string
	switch v.State {
	case agentexec.JobDone:
		head = fmt.Sprintf("exit status %d", *v.Exit)
	case agentexec.JobRunning:
		head = "still running after 60 seconds; read_output with this job_id to read more, or interrupt it"
	case agentexec.JobUnknown:
		head = "the terminal ended or the connection dropped while it ran: its outcome is unknown, and it was not sent again"
	case agentexec.JobInterrupted:
		head = "interrupted with Ctrl-C"
	case agentexec.JobEnded:
		head = "the user marked it done"
	}
	out := fmt.Sprintf("job_id %s: %s\noutput (bytes %d to %d):\n%s", v.ID, head, v.Offset, v.Next, Clean(v.Output, a.Secrets(target)))
	if v.Truncated {
		out += "\n[output beyond 1 MiB was not kept]"
	}
	return out
}

// saveSkill asks before storing a skill the model wrote, in every mode
// (KTD33): a saved skill steers later requests. The card shows the whole
// name and body; on yes Go stores its own copy.
func (a *Agent) saveSkill(ctx context.Context, c *conv, toolID string, sk store.Skill) (string, bool) {
	if err := store.CheckSkill(sk); err != nil {
		a.Emit(c.id, Event{Type: "result", ToolID: toolID, State: "refused", Text: err.Error()})
		return "Refused, nothing saved: " + err.Error(), true
	}
	if a.SaveSkill == nil {
		return "skills cannot be saved here", true
	}
	replaces := a.Skills != nil && a.Skills(sk.Name) != nil
	a.Emit(c.id, Event{Type: "skill", ToolID: toolID, Name: sk.Name, Text: sk.Body, Replaces: replaces})
	if _, err := a.Exec.Approvals.Ask(ctx, agentexec.Pending{Principal: c.id, Host: "Hostbay", Tool: toolSaveSkill, Name: sk.Name, Bytes: sk.Body, Replaces: replaces}); err != nil {
		a.Emit(c.id, Event{Type: "result", ToolID: toolID, State: "denied", Text: err.Error()})
		return "The user did not approve it; the skill was not saved.", true
	}
	if err := a.SaveSkill(sk); err != nil {
		a.Emit(c.id, Event{Type: "result", ToolID: toolID, State: "refused", Text: err.Error()})
		return "Saving failed: " + err.Error(), true
	}
	a.Emit(c.id, Event{Type: "result", ToolID: toolID, State: "saved"})
	return "Saved. The user can attach it with /" + sk.Name + ".", false
}
