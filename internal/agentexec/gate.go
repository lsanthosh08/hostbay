// Package agentexec is the one gate and executor between an agent and the
// user's terminals (KTD32). internal/ai, and later internal/mcp, reach a
// terminal only through it: every command is checked here, asks in the
// app-wide approval tray unless an allowlist the user configured matches
// (KTD33), and runs inside a completion envelope (KTD34).
package agentexec

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"hostbay/internal/store"
	"hostbay/internal/terminal"
)

// TermInfo is what Go knows of a terminal from its owner, never from the
// page or from terminal text.
type TermInfo struct {
	HostID string // the saved host, or "local" for a shell on this computer
	Label  string // what an approval card names
	Kind   string // "ssh", "telnet", "local", ...
	Shell  Shell
}

// Terminals is the terminal layer as the executor sees it. Its Write is the
// agent's own path: those bytes are not the user's keystrokes.
type Terminals interface {
	Info(id string) (TermInfo, error)
	Write(id string, p []byte) error
	Recent(id string) ([]byte, error)
	Subscribe(id string) (*terminal.Subscription, error)
}

// Policy is what may run without asking on one host.
type Policy struct {
	Mode    string // store.AIConfirmAll, AIDangerousWrite or AIDangerousOnly
	Bypass  bool   // the in-app agent's wildcard for this run of Hostbay
	Entries []store.AllowEntry
}

// Request is one proposed command. Tool inputs carry no risk field, so a
// model cannot grade its own command.
type Request struct {
	Principal string // the conversation
	Target    string // the terminal ID, frozen when proposed
	Tool      string
	Command   string
}

// Executor runs agents' commands.
type Executor struct {
	Approvals *Approvals
	Wait      time.Duration // how long Run waits for a job before returning it running
	Now       func() time.Time
	After     func(time.Duration) <-chan time.Time
	// Secrets lists what to scrub from a terminal's output before any of it
	// is cut and returned. Called with no lock held.
	Secrets func(target string) []string

	terms  Terminals
	policy func(hostID string) Policy

	mu    sync.Mutex
	state map[string]*termState // by terminal ID
	jobs  map[string]*Job
}

type termState struct {
	idle bool // Go saw the agent's last job finish and no keystroke since
	job  *Job
	gone bool
}

func New(terms Terminals, policy func(hostID string) Policy) *Executor {
	return &Executor{
		Approvals: NewApprovals(), Wait: time.Minute, Now: time.Now, After: time.After,
		terms: terms, policy: policy, state: map[string]*termState{}, jobs: map[string]*Job{},
	}
}

// Errors the model gets back in place of a result.
var (
	ErrBusy       = errors.New("the agent's previous command in this terminal has not finished; wait, read its output or interrupt it")
	errNeedsAsk   = errors.New("needs approval")
	errTargetGone = errors.New("that terminal is closed; nothing ran")
	errNoJob      = errors.New("no command of yours is running")
)

// Refusal is a proposal refused before any card: the model is told why.
type Refusal struct{ Reason string }

func (r *Refusal) Error() string { return "refused: " + r.Reason }

func (e *Executor) st(id string) *termState {
	s := e.state[id]
	if s == nil {
		s = &termState{}
		e.state[id] = s
	}
	return s
}

// autoRun reports whether cmd may run on t without asking: only on an idle
// POSIX terminal, and only when bypass is on or an entry of the mode
// matches. Anything else asks; the agent's first command on a terminal
// always does.
func (e *Executor) autoRun(t TermInfo, cmd string, idle bool) bool {
	if !idle || t.Shell != ShellPOSIX {
		return false
	}
	p := e.policy(t.HostID)
	if p.Bypass {
		return true
	}
	switch p.Mode {
	case store.AIDangerousWrite:
		return Match(cmd, Preset)
	case store.AIDangerousOnly:
		return Match(cmd, slices.Concat(Preset, p.Entries))
	}
	return false
}

// Run gates r and, once allowed, runs it in its target terminal. It waits
// up to Wait for the job, then returns it as it stands. A command that
// asks waits for the tray's answer; a denial, an expiry, a closed target
// or ctx ending returns an error and nothing is written.
func (e *Executor) Run(ctx context.Context, r Request) (JobView, error) {
	if err := CheckBytes(r.Command); err != nil {
		return JobView{}, &Refusal{err.Error()}
	}
	if strings.TrimSpace(r.Command) == "" {
		return JobView{}, &Refusal{"the command is empty"}
	}
	info, err := e.terms.Info(r.Target)
	if err != nil {
		return JobView{}, errTargetGone
	}
	approved := false
	for {
		j, busy, err := e.start(r, info, approved)
		switch {
		case errors.Is(err, errNeedsAsk):
			if _, err := e.Approvals.ask(ctx, Pending{Principal: r.Principal, Target: r.Target, Host: info.Label, Tool: r.Tool, Bytes: r.Command}); err != nil {
				return JobView{}, err
			}
			approved = true
			continue
		case err != nil:
			return busy, err
		}
		select {
		case <-j.done:
		case <-e.After(e.Wait):
		case <-ctx.Done():
		}
		return j.view(-1, e.secrets(r.Target)), nil
	}
}

// start writes the job, re-checking the gate under the lock right before
// anything not explicitly approved is written.
func (e *Executor) start(r Request, info TermInfo, approved bool) (*Job, JobView, error) {
	e.mu.Lock()
	s := e.st(r.Target)
	switch {
	case s.gone:
		e.mu.Unlock()
		return nil, JobView{}, errTargetGone
	case s.job != nil && s.job.running():
		j := s.job
		e.mu.Unlock()
		return nil, j.view(-1, e.secrets(r.Target)), ErrBusy
	case !approved && !e.autoRun(info, r.Command, s.idle):
		e.mu.Unlock()
		return nil, JobView{}, errNeedsAsk
	}
	sub, err := e.terms.Subscribe(r.Target)
	if err != nil {
		e.mu.Unlock()
		return nil, JobView{}, errTargetGone
	}
	j := &Job{ID: newID(), Principal: r.Principal, Target: r.Target, Command: r.Command, Shell: info.Shell,
		nonce: newID(), started: e.Now(), state: JobRunning, done: make(chan struct{})}
	e.prune()
	s.job, s.idle = j, false
	e.jobs[j.ID] = j
	e.mu.Unlock()

	if err := e.terms.Write(r.Target, Envelope(info.Shell, r.Command, j.nonce)); err != nil {
		sub.Close()
		j.finish(JobUnknown, 0, e.Now())
		return nil, JobView{}, fmt.Errorf("writing to the terminal failed, so whether it ran is unknown: %w", err)
	}
	go e.watch(j, sub)
	return j, JobView{}, nil
}

// watch follows a job's output until its marker, the terminal's end or,
// with no envelope, the confirm-only limit.
func (e *Executor) watch(j *Job, sub *terminal.Subscription) {
	defer sub.Close()
	sc := newScanner(j.nonce)
	var limit <-chan time.Time
	if j.Shell == ShellNone {
		limit = e.After(confirmLimit)
	}
	var gaps uint64
	for {
		select {
		case p, ok := <-sub.C:
			if !ok {
				e.end(j, JobUnknown, 0)
				return
			}
			j.append(p)
			if j.Shell == ShellNone {
				continue
			}
			code, found := sc.feed(p)
			if g := sub.Gaps(); !found && g != gaps { // a chunk was dropped: the marker may be in it
				gaps = g
				if recent, err := e.terms.Recent(j.Target); err == nil {
					code, found = sc.find(recent)
				}
			}
			if found {
				e.end(j, JobDone, code)
				return
			}
		case <-limit:
			e.end(j, JobEnded, 0)
			return
		case <-j.done: // interrupted or Done
			return
		}
	}
}

// end finishes j; a job seen to finish leaves its terminal idle.
func (e *Executor) end(j *Job, state JobState, code int) {
	if !j.finish(state, code, e.Now()) {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if s := e.state[j.Target]; s != nil && s.job == j && state == JobDone {
		s.idle = true
	}
}

// prune forgets jobs finished more than jobRetention ago; e.mu is held.
func (e *Executor) prune() {
	for id, j := range e.jobs {
		j.mu.Lock()
		old := j.state != JobRunning && e.Now().Sub(j.ended) > jobRetention
		j.mu.Unlock()
		if old {
			delete(e.jobs, id)
		}
	}
}

// running is principal's running job, if any.
func (e *Executor) running(principal string) *Job {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, s := range e.state {
		if s.job != nil && s.job.Principal == principal && s.job.running() {
			return s.job
		}
	}
	return nil
}

// Interrupt sends one Ctrl-C to principal's running job. Whether it
// stopped is not known, so its terminal is not idle afterwards.
func (e *Executor) Interrupt(principal string) error {
	j := e.running(principal)
	if j == nil {
		return errNoJob
	}
	err := e.terms.Write(j.Target, []byte{0x03})
	j.finish(JobInterrupted, 0, e.Now())
	return err
}

// Done ends principal's running job when the user says it finished, for a
// terminal with no envelope.
func (e *Executor) Done(principal string) error {
	j := e.running(principal)
	if j == nil {
		return errNoJob
	}
	j.finish(JobEnded, 0, e.Now())
	return nil
}

// Stop withdraws principal's pending requests and interrupts its running
// job, which gets one Ctrl-C.
func (e *Executor) Stop(principal string) {
	e.Approvals.drop(func(p Pending) bool { return p.Principal == principal }, errWithdrawn)
	_ = e.Interrupt(principal)
}

// Output reads principal's own job from offset.
func (e *Executor) Output(principal, jobID string, offset int) (JobView, error) {
	e.mu.Lock()
	j := e.jobs[jobID]
	e.mu.Unlock()
	if j == nil || j.Principal != principal {
		return JobView{}, errors.New("no such job")
	}
	return j.view(max(0, offset), e.secrets(j.Target)), nil
}

// ScreenTail is the last output of a terminal (KTD38), scrubbed before it
// is cut.
func (e *Executor) ScreenTail(target string) (string, error) {
	b, err := e.terms.Recent(target)
	if err != nil {
		return "", errTargetGone
	}
	s := Scrub(string(b), e.secrets(target))
	return strings.ToValidUTF8(s[max(0, len(s)-16<<10):], ""), nil
}

func (e *Executor) secrets(target string) []string {
	if e.Secrets == nil {
		return nil
	}
	return e.Secrets(target)
}

// Keystroke records that the user typed into a terminal: an allowlisted
// command would land in whatever holds it now, so the next one asks.
func (e *Executor) Keystroke(target string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if s := e.state[target]; s != nil {
		s.idle = false
	}
}

// TerminalClosed cancels the requests waiting on a terminal; its running
// job ends as unknown when its output does.
func (e *Executor) TerminalClosed(target string) {
	e.Approvals.drop(func(p Pending) bool { return p.Target == target }, errTargetClosed)
	e.mu.Lock()
	e.st(target).gone = true
	e.mu.Unlock()
}
