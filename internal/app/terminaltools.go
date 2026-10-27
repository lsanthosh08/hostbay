package app

import (
	"cmp"
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"hostbay/internal/secrets"
	"hostbay/internal/store"
	"hostbay/internal/terminal"
)

// terminalTools runs session logs and post-login rules on terminals and
// keeps what the page's terminal tools need per terminal (KTD39). Secrets
// it sends or scrubs are opened here and never cross the bridge.
type terminalTools struct {
	st    *store.Store // nil when the service has no store (some tests)
	vault *secrets.Vault
	terms *terminal.Registry
	emit  func(name string, data any)
	pick  func() (string, error) // the log folder dialog
	now   func() time.Time
	wait  time.Duration // how long a step waits for its prompt; 10 seconds when 0

	logMu sync.Mutex // one log change at a time, so a double toggle starts one log
	mu    sync.Mutex
	ptys  map[string]*ptyTools
}

// ptyTools is one terminal's state.
type ptyTools struct {
	hostID  string // "" for a terminal that is not a saved host's
	label   string // names the terminal's log and history
	echo    bool   // Go adds local echo: history is off
	typed   atomic.Bool
	log     *terminal.Logger
	logPath string
}

func newTerminalTools(creds *HostCredentials, terms *terminal.Registry, emit func(string, any)) *terminalTools {
	t := &terminalTools{terms: terms, emit: emit, pick: func() (string, error) { return PickDataFolder("Save session logs in") }, now: time.Now, ptys: map[string]*ptyTools{}}
	if creds != nil {
		t.st, t.vault = creds.st, creds.vault
	}
	return t
}

// start sets up a terminal that just opened, before any output is read:
// its log when its host logs every session, and its host's post-login
// rules, which run again on every new shell, a reconnect's included.
func (t *terminalTools) start(id, hostID, label string) {
	p := &ptyTools{hostID: hostID, label: label, echo: t.terms.LocalEcho(id)}
	t.mu.Lock()
	t.ptys[id] = p
	t.mu.Unlock()
	if t.st == nil {
		return
	}
	var rules []store.PostLoginRule
	if hostID != "" {
		if d, err := t.st.Data(); err == nil {
			if h := d.Host(hostID); h != nil {
				p.label = h.Alias // nothing else sees p before start returns
			}
			for _, r := range d.PostLoginRules(hostID) {
				if !r.Held && len(r.Steps) > 0 {
					rules = append(rules, r)
				}
			}
		}
	}
	if l, err := t.st.Local(); err == nil && hostID != "" && l.HostLogging[hostID] {
		if _, err := t.setLogging(id, true); err != nil {
			t.notice(id, "Session logging did not start: "+err.Error())
		}
	}
	if len(rules) > 0 {
		if sub, err := t.terms.Subscribe(id); err == nil {
			go t.runRules(id, p, rules, sub)
		}
	}
}

// ended forgets a terminal once it has closed.
func (t *terminalTools) ended(id string) {
	t.mu.Lock()
	var lg *terminal.Logger
	if p := t.ptys[id]; p != nil {
		lg = p.log
	}
	delete(t.ptys, id)
	t.mu.Unlock()
	if lg != nil {
		go lg.Stop() // the tap has closed; this only waits for the last line
	}
}

// typed records the user's first keystroke: no secret reply is sent after it.
func (t *terminalTools) typed(id string) {
	if p := t.get(id); p != nil {
		p.typed.Store(true)
	}
}

func (t *terminalTools) get(id string) *ptyTools {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ptys[id]
}

func (t *terminalTools) notice(id, msg string) { t.emit(TerminalNoticeEvent(id), msg) }

// runRules runs the rules in order on one subscription, stopping at the
// first that fails, with a notice in the tab.
func (t *terminalTools) runRules(id string, p *ptyTools, rules []store.PostLoginRule, sub *terminal.Subscription) {
	defer sub.Close()
	for _, r := range rules {
		steps := make([]terminal.ExpectStep, len(r.Steps))
		for i, s := range r.Steps {
			steps[i] = terminal.ExpectStep{Expect: s.Expect, Secret: s.Secret()}
		}
		x := terminal.Expect{Steps: steps, Timeout: t.wait, Typed: p.typed.Load, Send: func(i int) error {
			reply, err := t.reply(p.hostID, r, i)
			if err != nil {
				return err
			}
			return t.terms.Write(id, append(reply, '\r'))
		}}
		if err := x.Run(sub); err != nil {
			t.notice(id, "Post-login rule stopped: "+err.Error())
			return
		}
	}
}

// reply is step i's text, a secret opened only now, at send time.
func (t *terminalTools) reply(hostID string, r store.PostLoginRule, i int) ([]byte, error) {
	s := r.Steps[i]
	if !s.Secret() {
		return []byte(s.Reply), nil
	}
	d, err := t.st.Data()
	if err != nil {
		return nil, err
	}
	h := d.Host(hostID)
	if h == nil {
		return nil, errNoHost
	}
	if s.Kind == store.ReplyPassword {
		if h.Password == "" {
			return nil, errors.New("this host has no saved password to send")
		}
		return t.vault.Open(h.Password, h.SecretAAD())
	}
	cur := d.PostLoginRule(r.ID)
	if cur == nil || cur.Answer == "" {
		return nil, errors.New("the rule's secret reply needs entering again")
	}
	return t.vault.Open(cur.Answer, store.PostLoginAAD(cur.ID, h))
}

var errNoHost = errors.New("host not found")

// knownSecrets are the secrets a log of hostID's terminal must not hold:
// its saved password and its rules' secret replies. One that does not
// open is skipped.
func (t *terminalTools) knownSecrets(hostID string) []string {
	d, err := t.st.Data()
	if err != nil || hostID == "" {
		return nil
	}
	h := d.Host(hostID)
	if h == nil {
		return nil
	}
	var out []string
	if h.Password != "" {
		if pw, err := t.vault.Open(h.Password, h.SecretAAD()); err == nil {
			out = append(out, string(pw))
		}
	}
	for _, r := range d.PostLoginRules(hostID) {
		if r.Answer == "" {
			continue
		}
		if a, err := t.vault.Open(r.Answer, store.PostLoginAAD(r.ID, h)); err == nil {
			out = append(out, string(a))
		}
	}
	return out
}

// logFolder is where logs go: the folder the user picked, or logs/ in the
// data folder.
func (t *terminalTools) logFolder() (string, error) {
	l, err := t.st.Local()
	if err != nil || l.LogFolder != "" {
		return l.LogFolder, err
	}
	return filepath.Join(t.st.Dir(), "logs"), nil
}

// setLogging starts or stops a terminal's log. One started mid-session
// begins with the next output.
func (t *terminalTools) setLogging(id string, on bool) (TerminalState, error) {
	if t.st == nil {
		return TerminalState{}, errors.New("session logging is not available")
	}
	t.logMu.Lock()
	defer t.logMu.Unlock()
	t.mu.Lock()
	p := t.ptys[id]
	if p == nil {
		p = &ptyTools{label: "terminal", echo: t.terms.LocalEcho(id)} // a container or pod shell
		t.ptys[id] = p
	}
	l := p.log
	if !on {
		p.log, p.logPath = nil, ""
	}
	t.mu.Unlock()
	if !on {
		if l != nil {
			l.Stop()
		}
		return t.state(id)
	}
	if l != nil {
		return t.state(id)
	}
	dir, err := t.logFolder()
	if err != nil {
		return TerminalState{}, err
	}
	sub, err := t.terms.Subscribe(id)
	if err != nil {
		return TerminalState{}, err
	}
	f, err := terminal.OpenLogFile(dir, p.label, t.now())
	if err != nil {
		sub.Close()
		return TerminalState{}, err
	}
	var lg *terminal.Logger
	lg = terminal.StartLog(sub, f, t.knownSecrets(p.hostID), func(err error) {
		t.mu.Lock()
		if p.log == lg {
			p.log, p.logPath = nil, ""
		}
		t.mu.Unlock()
		t.notice(id, "Session logging stopped: "+err.Error())
	})
	t.mu.Lock()
	p.log, p.logPath = lg, f.Name()
	t.mu.Unlock()
	return t.state(id)
}

// TerminalState is what a terminal's tab shows about its tools.
type TerminalState struct {
	// LocalEcho is a terminal where Go shows typed text itself, so
	// history and AI suggestions are off there (KTD39).
	LocalEcho bool   `json:"localEcho"`
	Logging   bool   `json:"logging"`
	LogPath   string `json:"logPath,omitempty"`
}

func (t *terminalTools) state(id string) (TerminalState, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.ptys[id]
	if p == nil {
		return TerminalState{LocalEcho: t.terms.LocalEcho(id)}, nil
	}
	return TerminalState{LocalEcho: p.echo, Logging: p.log != nil, LogPath: p.logPath}, nil
}

// historyKey names a terminal's history: its host, or its label.
func (p *ptyTools) historyKey() string { return cmp.Or(p.hostID, p.label) }

// TerminalToolsService serves the page's terminal tools: logging, post-login
// rules and command history (R69–R71). Quick commands need nothing more
// than PtyWrite.
type TerminalToolsService struct {
	t *terminalTools
}

func NewTerminalToolsService(c *ConnService) *TerminalToolsService {
	return &TerminalToolsService{t: c.tools}
}

// TerminalState reports a terminal's local echo and log.
func (s *TerminalToolsService) TerminalState(ptyID string) (TerminalState, error) {
	return s.t.state(ptyID)
}

// SetLogging turns a terminal's log on or off.
func (s *TerminalToolsService) SetLogging(ptyID string, on bool) (TerminalState, error) {
	return s.t.setLogging(ptyID, on)
}

// LogFolder is where new session logs are written.
func (s *TerminalToolsService) LogFolder() (string, error) {
	if s.t.st == nil {
		return "", errors.New("session logging is not available")
	}
	return s.t.logFolder()
}

// ChooseLogFolder asks for the folder new logs go in, in a dialog Go opens,
// and returns it; "" when the user cancelled.
func (s *TerminalToolsService) ChooseLogFolder() (string, error) {
	dir, err := s.t.pick()
	if err != nil || dir == "" {
		return "", err
	}
	return dir, s.t.st.EditLocal(func(l *store.Local) error { l.LogFolder = dir; return nil })
}

// ResetLogFolder puts new logs back in the data folder.
func (s *TerminalToolsService) ResetLogFolder() error {
	return s.t.st.EditLocal(func(l *store.Local) error { l.LogFolder = ""; return nil })
}

// PostLoginRule is a rule as the page sees it: its custom secret is only
// HasAnswer.
type PostLoginRule struct {
	ID        string                `json:"id"`
	HostID    string                `json:"hostId"`
	Steps     []store.PostLoginStep `json:"steps"`
	HasAnswer bool                  `json:"hasAnswer"`
	// Held is a rule another computer made or changed that waits for
	// confirmation in Sync before it runs here.
	Held bool `json:"held"`
}

func ruleView(r store.PostLoginRule) PostLoginRule {
	steps := r.Steps
	if steps == nil {
		steps = []store.PostLoginStep{}
	}
	return PostLoginRule{ID: r.ID, HostID: r.HostID, Steps: steps, HasAnswer: r.Answer != "", Held: r.Held}
}

// PostLoginRules lists hostID's rules.
func (s *TerminalToolsService) PostLoginRules(hostID string) ([]PostLoginRule, error) {
	d, err := s.t.st.Data()
	if err != nil {
		return nil, err
	}
	out := []PostLoginRule{}
	for _, r := range d.PostLoginRules(hostID) {
		out = append(out, ruleView(r))
	}
	return out, nil
}

const maxSteps = 20

// SavePostLoginRule saves a rule. answer nil keeps the saved secret reply,
// "" removes it, and any other value is sealed for the rule's host. A rule
// whose steps change and that sends the host's saved password, or keeps
// its saved secret reply, needs OS verification first: a page that could
// make one at will could have the secret typed into a shell that shows it.
func (s *TerminalToolsService) SavePostLoginRule(ctx context.Context, in PostLoginRule, answer *string) (PostLoginRule, error) {
	if len(in.Steps) > maxSteps {
		return PostLoginRule{}, errors.New("a post-login rule has at most 20 steps")
	}
	for _, st := range in.Steps {
		if !slices.Contains([]string{store.ReplyText, store.ReplyPassword, store.ReplySecret}, st.Kind) {
			return PostLoginRule{}, errors.New("unknown reply kind " + st.Kind)
		}
		if strings.ContainsAny(st.Reply+st.Expect, "\r\n") {
			return PostLoginRule{}, errors.New("a step's prompt and reply are one line each")
		}
	}
	d, err := s.t.st.Data()
	if err != nil {
		return PostLoginRule{}, err
	}
	if d.Host(in.HostID) == nil {
		return PostLoginRule{}, errNoHost
	}
	var old []store.PostLoginStep
	keepsSecret := false
	if r := d.PostLoginRule(in.ID); r != nil && r.HostID == in.HostID {
		old = r.Steps
		// The page cannot read the saved secret, so it is one more thing a
		// changed step could have typed where it shows.
		keepsSecret = answer == nil && r.Answer != "" && slices.ContainsFunc(in.Steps, func(s store.PostLoginStep) bool { return s.Kind == store.ReplySecret })
	}
	if (sendsPassword(in.Steps) || keepsSecret) && !slices.Equal(old, in.Steps) {
		if err := s.t.vault.Gate(ctx, "let a post-login rule send a saved password or secret"); err != nil {
			return PostLoginRule{}, err
		}
	}
	var out store.PostLoginRule
	err = s.t.st.Edit(func(d *store.Data) error {
		h := d.Host(in.HostID)
		if h == nil {
			return errNoHost
		}
		r := store.PostLoginRule{Meta: store.Meta{ID: in.ID}}
		if cur := d.PostLoginRule(in.ID); cur != nil && cur.HostID == in.HostID {
			r = *cur
		} else {
			r.ID = store.NewID() // the secret is bound to the ID, so it is new before sealing
		}
		r.HostID, r.Steps = in.HostID, slices.Clone(in.Steps)
		if answer != nil {
			r.Answer = ""
			if *answer != "" {
				sealed, err := s.t.vault.Seal([]byte(*answer), store.PostLoginAAD(r.ID, h))
				if err != nil {
					return err
				}
				r.Answer = sealed
			}
		}
		out = d.PutPostLogin(r)
		return nil
	})
	return ruleView(out), err
}

func sendsPassword(steps []store.PostLoginStep) bool {
	return slices.ContainsFunc(steps, func(s store.PostLoginStep) bool { return s.Kind == store.ReplyPassword })
}

// DeletePostLoginRule removes a rule.
func (s *TerminalToolsService) DeletePostLoginRule(id string) error {
	return s.t.st.Edit(func(d *store.Data) error { return d.PurgePostLogin(id) })
}

// AddHistory records a command the page read from the terminal at Enter.
// It is refused on a terminal where Go adds local echo, where the page
// cannot tell a typed password from a command.
func (s *TerminalToolsService) AddHistory(ptyID, line string) error {
	p := s.t.get(ptyID)
	if p == nil || s.t.st == nil {
		return errors.New("this terminal keeps no history")
	}
	if p.echo {
		return errors.New("history is off on a terminal that shows typed text itself")
	}
	if line = strings.TrimSpace(line); line == "" {
		return nil
	}
	return s.t.st.AddHistory(p.historyKey(), line)
}

// History lists the terminal's host's commands, oldest first.
func (s *TerminalToolsService) History(ptyID string) ([]string, error) {
	p := s.t.get(ptyID)
	if p == nil || s.t.st == nil {
		return []string{}, nil
	}
	h, err := s.t.st.History(p.historyKey())
	if h == nil {
		h = []string{}
	}
	return h, err
}

// ClearHistory removes the terminal's host's history.
func (s *TerminalToolsService) ClearHistory(ptyID string) error {
	p := s.t.get(ptyID)
	if p == nil || s.t.st == nil {
		return nil
	}
	return s.t.st.ClearHistory(p.historyKey())
}
