package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"sync"

	"hostbay/internal/agentexec"
	"hostbay/internal/ai"
	"hostbay/internal/ai/claudecode"
	"hostbay/internal/apperr"
	"hostbay/internal/container"
	"hostbay/internal/secrets"
	"hostbay/internal/sshx"
	"hostbay/internal/store"
	"hostbay/internal/terminal"
	"hostbay/internal/tlstrust"
)

// AIService is the AI sidebar's backend (R60, KTD31): providers, the
// execution mode and allowlist, conversations and the approval tray. The
// provider key never leaves Go: no binding takes it back out or returns
// it, and the page sees only whether one is saved.
type AIService struct {
	st      *store.Store
	vault   *secrets.Vault
	creds   *HostCredentials
	conns   *ConnService
	certs   *tlstrust.Store
	emit    func(name string, data any)
	confirm container.Confirm                              // a dialog Go opens, which the page cannot answer
	gate    func(ctx context.Context, reason string) error // OS user verification (KTD7)

	exec  *agentexec.Executor
	agent *ai.Agent

	// Claude Code (KTD6, KTD7): the path checks, discovery and the file
	// dialog run in Go, and the page names a program only by an index into
	// cands, the last discovery.
	claude      claudecode.Rules
	discover    func() []claudecode.Candidate
	pickProgram func() (string, error)

	mu     sync.Mutex
	bypass bool                   // this run of Hostbay only; never saved
	cands  []claudecode.Candidate // the last discovery
	tested map[string]string      // program path to the version its last successful Test saw

	db *DatabaseService // the open databases a question to SQL reads names from (LinkAIDatabase)
}

func NewAIService(st *store.Store, vault *secrets.Vault, creds *HostCredentials, conns *ConnService, certs *tlstrust.Store) *AIService {
	s := &AIService{st: st, vault: vault, creds: creds, conns: conns, certs: certs, emit: emitEvent, confirm: container.NativeConfirm, gate: vault.Gate,
		claude: claudecode.DefaultRules(), discover: discoverClaudeCode, pickProgram: pickClaudeProgram}
	s.exec = agentexec.New(agentTerms{conns}, s.policy)
	s.exec.Secrets = s.secrets
	s.exec.Approvals.OnChange = func(l []agentexec.Pending) { s.emit(AIApprovalsEvent, l) }
	s.agent = &ai.Agent{
		Exec:     s.exec,
		Emit:     func(conv string, ev ai.Event) { s.emit(AIEvent(conv), ev) },
		Provider: s.provider,
		Info:     func(id string) (agentexec.TermInfo, error) { return conns.agentInfo(context.Background(), id) },
		Secrets:  s.secrets,
	}
	s.agent.Skills, s.agent.SaveSkill = s.skillNamed, s.saveAgentSkill
	conns.setAgentHooks(s.exec.Keystroke, func(id string) {
		s.exec.TerminalClosed(id)
		s.agent.TerminalClosed(id)
	})
	return s
}

// agentTerms is the terminal layer as the gate sees it. Its Write is the
// agent's own path, which does not count as the user's keystrokes.
type agentTerms struct{ c *ConnService }

func (t agentTerms) Info(id string) (agentexec.TermInfo, error) {
	return t.c.agentInfo(context.Background(), id)
}

// Write counts as typing for post-login rules, so no secret reply lands in
// the agent's command, but not for the agent's own idle check.
func (t agentTerms) Write(id string, p []byte) error {
	t.c.tools.typed(id)
	return t.c.terms.Write(id, p)
}
func (t agentTerms) Recent(id string) ([]byte, error) { return t.c.terms.Recent(id) }
func (t agentTerms) Subscribe(id string) (*terminal.Subscription, error) {
	return t.c.terms.Subscribe(id)
}

// policy is what may run without asking on host: the synced mode, this
// run's bypass and this computer's entries for host.
func (s *AIService) policy(hostID string) agentexec.Policy {
	p := agentexec.Policy{Mode: store.AIConfirmAll}
	if d, err := s.st.Data(); err == nil {
		p.Mode = d.AIMode()
	}
	if l, err := s.st.Local(); err == nil {
		p.Entries = l.Allowlist[hostID]
	}
	s.mu.Lock()
	p.Bypass = s.bypass
	s.mu.Unlock()
	return p
}

// secrets is what must not reach the provider from a terminal's output:
// its host's saved password (the login, sudo and key passphrase one) and
// the password the connection signed in with, and the secret replies of
// its post-login rules, the list its session log scrubs (U43, KTD39).
func (s *AIService) secrets(ptyID string) []string {
	m := s.conns.pty(ptyID)
	var out []string
	if m.hostID != "" {
		if c, err := s.creds.Lookup(m.hostID); err == nil && c.Password != "" {
			out = append(out, c.Password)
		}
		out = append(out, s.conns.tools.knownSecrets(m.hostID)...)
	}
	if s.conns.mgr == nil {
		return out
	}
	if l := s.conns.mgr.Lease(m.session); l != nil {
		if pw := l.LoginPassword(); pw != "" {
			out = append(out, pw)
		}
	}
	return out
}

// ---- Providers ----

// AIProvider is a provider as the page sees it: never its key.
type AIProvider struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Protocol    string `json:"protocol"` // "anthropic" or "openai-chat"
	BaseURL     string `json:"baseUrl"`
	Model       string `json:"model"`
	ProxyID     string `json:"proxyId"`
	Unencrypted bool   `json:"unencrypted"`
	// KeyState is "none", "saved", or "retype" when the saved key no
	// longer opens because the provider was pointed elsewhere.
	KeyState string `json:"keyState"`
	// ExecPath and Consented describe a Claude Code provider. The page only
	// reads them: SaveProvider ignores what it sends (KTD7).
	ExecPath  string `json:"execPath"`
	Consented bool   `json:"consented"`
}

func (s *AIService) toUI(p store.AIProvider) AIProvider {
	out := AIProvider{ID: p.ID, Name: p.Name, Protocol: p.Protocol, BaseURL: p.BaseURL, Model: p.Model, ProxyID: p.ProxyID, Unencrypted: p.Unencrypted, KeyState: "none",
		ExecPath: p.ExecPath, Consented: p.Consented}
	if p.APIKey != "" {
		out.KeyState = "saved"
		if _, err := s.vault.Open(p.APIKey, p.SecretAAD()); err != nil && undecryptable(err) {
			out.KeyState = "retype"
		}
	}
	return out
}

// undecryptable is a sealed value bound elsewhere or damaged, rather than
// a vault that is locked.
func undecryptable(err error) bool {
	var e *apperr.Error
	return errors.As(err, &e) && e.Code == apperr.SecretNeeded && e.Details["reason"] == "decrypt"
}

func (s *AIService) ListProviders() ([]AIProvider, error) {
	d, err := s.st.Data()
	if err != nil {
		return nil, err
	}
	out := make([]AIProvider, 0, len(d.AIProviders.Items))
	for _, p := range d.AIProviders.Items {
		out = append(out, s.toUI(p))
	}
	return out, nil
}

// DefaultModels lists the models offered before a provider lists its own.
func (*AIService) DefaultModels(protocol string) []string { return ai.DefaultModels[protocol] }

// record is in as a stored provider, checked, with saved's Meta.
func record(d *store.Data, in AIProvider) (store.AIProvider, error) {
	if in.Protocol == store.AIClaudeCode {
		return claudeRecord(d, in)
	}
	p := store.AIProvider{Meta: store.Meta{ID: in.ID}, Name: strings.TrimSpace(in.Name), Protocol: in.Protocol,
		BaseURL: strings.TrimSpace(in.BaseURL), Model: strings.TrimSpace(in.Model), ProxyID: in.ProxyID, Unencrypted: in.Unencrypted}
	u, err := url.Parse(p.BaseURL)
	switch {
	case p.Name == "":
		return p, errors.New("give the provider a name")
	case !slices.Contains(store.AIProtocols, p.Protocol):
		return p, fmt.Errorf("unknown protocol %q", p.Protocol)
	case err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "":
		return p, errors.New("enter the provider's base URL, such as https://api.anthropic.com")
	case u.Scheme == "http" && !p.Unencrypted && !ai.IsLoopback(u.Hostname()):
		return p, errors.New("an http:// provider must be on this computer; for another host use https, or mark it unencrypted")
	case p.Model == "":
		return p, errors.New("choose a model")
	case p.ProxyID != "" && d.Proxy(p.ProxyID) == nil:
		return p, errors.New("that proxy no longer exists")
	}
	if u.Scheme == "https" {
		p.Unencrypted = false
	}
	if cur := d.AIProvider(in.ID); cur != nil {
		p.Meta = cur.Meta
	}
	return p, nil
}

// claudeRecord is a Claude Code provider as stored: a name and a model, no
// URL, proxy or key. The program path and the consent come from the saved
// record only, never from in (KTD7).
func claudeRecord(d *store.Data, in AIProvider) (store.AIProvider, error) {
	p := store.AIProvider{Meta: store.Meta{ID: in.ID}, Name: strings.TrimSpace(in.Name), Protocol: store.AIClaudeCode, Model: strings.TrimSpace(in.Model)}
	switch {
	case p.Name == "":
		return p, errors.New("give the provider a name")
	case p.Model == "":
		return p, errors.New("choose a model")
	case !claudecode.ValidModel(p.Model):
		return p, errors.New("that model name is not one Claude Code can take")
	case in.BaseURL != "" || in.ProxyID != "" || in.Unencrypted:
		return p, errors.New("a Claude Code provider has no address, proxy or key")
	}
	if cur := d.AIProvider(in.ID); cur != nil {
		p.Meta = cur.Meta
		if cur.Protocol == store.AIClaudeCode {
			p.ExecPath, p.Consented = cur.ExecPath, cur.Consented
		}
	}
	return p, nil
}

// SaveProvider saves a provider. apiKey is the key typed in this form: it
// is sealed and never returned; an empty one keeps the saved key, which no
// longer opens once the protocol, base URL or proxy changed (KTD31).
func (s *AIService) SaveProvider(in AIProvider, apiKey string) (AIProvider, error) {
	var out store.AIProvider
	err := s.st.Edit(func(d *store.Data) error {
		p, err := record(d, in)
		if err != nil {
			return err
		}
		if p.ID == "" {
			p.ID = store.NewID()
		}
		switch cur := d.AIProvider(p.ID); {
		case p.Protocol == store.AIClaudeCode:
			if apiKey != "" {
				return errors.New("a Claude Code provider takes no API key")
			}
		case apiKey != "":
			sealed, err := s.vault.Seal([]byte(strings.TrimSpace(apiKey)), p.SecretAAD())
			if err != nil {
				return err
			}
			p.APIKey = sealed
		case cur != nil:
			p.APIKey = cur.APIKey
		}
		out = d.PutAIProvider(p)
		return nil
	})
	if err != nil {
		return AIProvider{}, err
	}
	return s.toUI(out), nil
}

func (s *AIService) DeleteProvider(id string) error {
	return s.st.Edit(func(d *store.Data) error { return d.PurgeAIProvider(id) })
}

// TestProvider lists the models of the provider as edited in the form, so
// it tests the address, the key and any certificate at once. With no typed
// key it reuses the saved one only when the edited record's binding equals
// the saved one byte for byte; otherwise it sends nothing.
func (s *AIService) TestProvider(ctx context.Context, in AIProvider, apiKey string) ([]string, error) {
	d, err := s.st.Data()
	if err != nil {
		return nil, err
	}
	p, err := record(d, in)
	if err != nil {
		return nil, err
	}
	if p.Protocol == store.AIClaudeCode {
		cp, err := s.claudeProvider(p)
		if err != nil {
			return nil, err
		}
		info, err := cp.Test(ctx)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		if s.tested == nil {
			s.tested = map[string]string{}
		}
		s.tested[p.ExecPath] = info.Version
		s.mu.Unlock()
		return cp.Models(ctx)
	}
	key := strings.TrimSpace(apiKey)
	if key == "" {
		if key, err = s.savedKey(d, p); err != nil {
			return nil, err
		}
	}
	prov, err := s.client(ctx, d, p, key)
	if err != nil {
		return nil, err
	}
	return prov.Models(ctx)
}

// savedKey is the key saved for edited's ID, when edited still binds it.
func (s *AIService) savedKey(d *store.Data, edited store.AIProvider) (string, error) {
	cur := d.AIProvider(edited.ID)
	if cur == nil || cur.APIKey == "" {
		return "", nil
	}
	if !bytes.Equal(cur.SecretAAD(), edited.SecretAAD()) {
		return "", apperr.New(apperr.SecretNeeded, "Type the API key again: the provider's protocol, address or proxy changed, so the saved key is not sent there").
			With("reason", "retype").With("provider", edited.Name)
	}
	k, err := s.vault.Open(cur.APIKey, cur.SecretAAD())
	return string(k), err
}

// TrustCert answers a provider certificate prompt (HostKeyUnknown, details
// kind "aiCert").
func (s *AIService) TrustCert(requestID string, accept bool) error {
	return s.certs.Answer(requestID, accept)
}

// client is p's provider client, reached through p's proxy and only there.
func (s *AIService) client(ctx context.Context, d *store.Data, p store.AIProvider, key string) (ai.Provider, error) {
	var px *sshx.Proxy
	if p.ProxyID != "" {
		var err error
		if px, err = s.creds.proxyByID(d, p.ProxyID); err != nil {
			return nil, err
		}
	}
	hc, err := ai.HTTPClient(ctx, ai.Endpoint{ID: p.ID, BaseURL: p.BaseURL, Unencrypted: p.Unencrypted},
		func(ctx context.Context, addr string) (net.Conn, error) { return sshx.DialTCP(ctx, px, addr) }, s.certs)
	if err != nil {
		return nil, err
	}
	prov, err := ai.New(p.Protocol, p.BaseURL, key, "Hostbay/"+Version, hc)
	if err != nil {
		return nil, err
	}
	return scrubbed{prov, key}, nil
}

// provider opens a saved provider for a conversation.
func (s *AIService) provider(id string) (ai.Provider, string, error) {
	d, err := s.st.Data()
	if err != nil {
		return nil, "", err
	}
	p := d.AIProvider(id)
	if p == nil {
		return nil, "", errors.New("this conversation's provider was deleted; choose another")
	}
	if p.Protocol == store.AIClaudeCode { // no key, no vault, no network client of ours
		cp, err := s.claudeProvider(*p)
		if err != nil {
			return nil, "", err
		}
		return cp, p.Model, nil
	}
	var key string
	if p.APIKey != "" {
		k, err := s.vault.Open(p.APIKey, p.SecretAAD())
		if err != nil {
			if undecryptable(err) {
				return nil, "", apperr.New(apperr.SecretNeeded, "The API key of "+p.Name+" must be typed again in Settings > AI").
					With("reason", "retype").With("provider", p.Name)
			}
			return nil, "", err
		}
		key = string(k)
	}
	prov, err := s.client(context.Background(), d, *p, key)
	return prov, p.Model, err
}

// scrubbed keeps the key out of any error a provider sends back.
type scrubbed struct {
	p   ai.Provider
	key string
}

func (c scrubbed) clean(err error) error {
	if err == nil || c.key == "" || !strings.Contains(err.Error(), c.key) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), c.key, "[key]"))
}

func (c scrubbed) Stream(ctx context.Context, r ai.Request, text func(string)) (ai.Reply, error) {
	rep, err := c.p.Stream(ctx, r, text)
	return rep, c.clean(err)
}

func (c scrubbed) Models(ctx context.Context) ([]string, error) {
	m, err := c.p.Models(ctx)
	return m, c.clean(err)
}

// ---- Mode, bypass and allowlist ----

// AIMode is the execution mode in force and whether bypass is on.
type AIMode struct {
	Mode   string `json:"mode"`
	Bypass bool   `json:"bypass"`
}

func (s *AIService) GetMode() (AIMode, error) {
	d, err := s.st.Data()
	if err != nil {
		return AIMode{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return AIMode{Mode: d.AIMode(), Bypass: s.bypass}, nil
}

// widen asks, in a dialog Go opens and then through OS verification, before
// a change that lets more run without asking (KTD32).
func widen(ctx context.Context, confirm container.Confirm, gate func(context.Context, string) error, title, message string) error {
	if !confirm(ctx, title, message) {
		return apperr.New(apperr.VerifyFailed, "Cancelled; nothing changed.")
	}
	return gate(ctx, title)
}

func (s *AIService) widen(ctx context.Context, title, message string) error {
	return widen(ctx, s.confirm, s.gate, title, message)
}

const modeTitle = "Let the AI agent run some commands without asking?"

var modeText = map[string]string{
	store.AIDangerousWrite: "The AI agent will run Hostbay's read-only commands, such as uptime and df -h, without asking, on a terminal where its last command finished and nothing was typed since.",
	store.AIDangerousOnly:  "The AI agent will run Hostbay's read-only commands and the commands you allowed for each host without asking, on a terminal where its last command finished and nothing was typed since.",
}

// SetMode changes the execution mode. Any mode but confirm all needs the
// dialog and OS verification; confirm all applies at once.
func (s *AIService) SetMode(ctx context.Context, mode string) error {
	if mode != store.AIConfirmAll {
		text, ok := modeText[mode]
		if !ok {
			return fmt.Errorf("unknown AI execution mode %q", mode)
		}
		if err := s.widen(ctx, modeTitle, text); err != nil {
			return err
		}
	}
	return s.st.Edit(func(d *store.Data) error {
		g := store.SettingsGroup{Meta: store.Meta{ID: store.GroupAI}}
		if cur := d.SettingsGroup(store.GroupAI); cur != nil {
			g = *cur
		}
		g.ExecutionMode = mode
		d.PutSettings(g)
		return nil
	})
}

// SetBypass turns bypass on, after the dialog and OS verification, or off.
// It lasts until Hostbay quits, then the saved mode is back (KTD33).
func (s *AIService) SetBypass(ctx context.Context, on bool) error {
	if on {
		if err := s.widen(ctx, "Let the AI agent run every command without asking?",
			"Until Hostbay quits, the in-app AI agent will run any command it proposes without asking, on a terminal where its last command finished and nothing was typed since."); err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.bypass = on
	s.mu.Unlock()
	return nil
}

// AIAllowGroup is one host's allowlist entries.
type AIAllowGroup struct {
	HostID  string         `json:"hostId"`
	Label   string         `json:"label"`
	Entries []AIAllowEntry `json:"entries"`
}

type AIAllowEntry struct {
	Command string `json:"command"`
	Prefix  bool   `json:"prefix"`
}

func (s *AIService) ListAllowlist() ([]AIAllowGroup, error) {
	l, err := s.st.Local()
	if err != nil {
		return nil, err
	}
	d, err := s.st.Data()
	if err != nil {
		return nil, err
	}
	out := []AIAllowGroup{}
	for host, es := range l.Allowlist {
		g := AIAllowGroup{HostID: host, Label: hostLabel(d, host)}
		for _, e := range es {
			g.Entries = append(g.Entries, AIAllowEntry{Command: agentexec.Command(e), Prefix: e.Prefix})
		}
		out = append(out, g)
	}
	slices.SortFunc(out, func(a, b AIAllowGroup) int { return strings.Compare(a.Label, b.Label) })
	return out, nil
}

func hostLabel(d *store.Data, id string) string {
	if id == "local" {
		return "This computer"
	}
	if h := d.Host(id); h != nil {
		return h.Alias
	}
	return "Deleted host"
}

// AddAllowEntry lets command run without asking on host in dangerous-only
// mode, after the dialog and OS verification.
func (s *AIService) AddAllowEntry(ctx context.Context, hostID, command string, prefix bool) error {
	e, err := agentexec.ParseEntry(command, prefix)
	if err != nil {
		return err
	}
	d, err := s.st.Data()
	if err != nil {
		return err
	}
	if hostID != "local" && d.Host(hostID) == nil {
		return errors.New("host not found")
	}
	what := agentexec.Command(e)
	if prefix {
		what += " followed by plain arguments"
	}
	if err := s.widen(ctx, "Let the AI agent run this without asking?",
		fmt.Sprintf("On %s, in dangerous-only mode, the AI agent will run %q without asking. The command runs as the remote shell sees it, aliases included.", hostLabel(d, hostID), what)); err != nil {
		return err
	}
	return s.st.EditLocal(func(l *store.Local) error {
		if l.Allowlist == nil {
			l.Allowlist = map[string][]store.AllowEntry{}
		}
		for _, x := range l.Allowlist[hostID] {
			if x.Prefix == e.Prefix && slices.Equal(x.Argv, e.Argv) {
				return nil
			}
		}
		l.Allowlist[hostID] = append(l.Allowlist[hostID], e)
		return nil
	})
}

// RemoveAllowEntry takes an entry off; that only makes more ask.
func (s *AIService) RemoveAllowEntry(hostID, command string, prefix bool) error {
	return s.st.EditLocal(func(l *store.Local) error {
		if len(l.Allowlist[hostID]) == 0 {
			return nil // l.Allowlist may be nil
		}
		l.Allowlist[hostID] = slices.DeleteFunc(l.Allowlist[hostID], func(x store.AllowEntry) bool {
			return x.Prefix == prefix && agentexec.Command(x) == command
		})
		if len(l.Allowlist[hostID]) == 0 {
			delete(l.Allowlist, hostID)
		}
		return nil
	})
}

// ---- Terminals, conversations and approvals ----

// AITarget is a terminal the agent can act on.
type AITarget struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	ConfirmOnly bool   `json:"confirmOnly"` // no envelope: every command asks and ends on Done
}

func (s *AIService) Terminals(ctx context.Context) []AITarget {
	out := []AITarget{}
	for _, id := range s.conns.agentTerminals() {
		if i, err := s.conns.agentInfo(ctx, id); err == nil {
			out = append(out, AITarget{ID: id, Label: i.Label, ConfirmOnly: i.Shell == agentexec.ShellNone})
		}
	}
	slices.SortFunc(out, func(a, b AITarget) int { return strings.Compare(a.Label, b.Label) })
	return out
}

// SetActiveTerminal records the terminal the page focused, which
// follow-active conversations target.
func (s *AIService) SetActiveTerminal(ptyID string) { s.agent.SetActive(ptyID) }

// StartConversation opens a conversation with providerID, pinned to the
// terminal ptyID or, with "", following the active one. Its events are
// AIEvent(id).
func (s *AIService) StartConversation(providerID, ptyID string) (string, error) {
	d, err := s.st.Data()
	if err != nil {
		return "", err
	}
	if d.AIProvider(providerID) == nil {
		return "", errors.New("choose a provider in Settings > AI")
	}
	if ptyID != "" {
		if _, err := s.conns.agentInfo(context.Background(), ptyID); err != nil {
			return "", err
		}
	}
	return s.agent.Start(providerID, ptyID), nil
}

func (s *AIService) Send(convID, text string) error {
	if strings.TrimSpace(text) == "" {
		return errors.New("type a message")
	}
	return s.agent.Send(convID, text)
}

func (s *AIService) Retry(convID string) error { return s.agent.Retry(convID) }

// Stop ends the turn, withdraws its card and sends one Ctrl-C to its
// running command.
func (s *AIService) Stop(convID string)              { s.agent.Stop(convID) }
func (s *AIService) CloseConversation(convID string) { s.agent.Close(convID) }

// JobDone ends the conversation's running command on a confirm-only
// terminal.
func (s *AIService) JobDone(convID string) error { return s.agent.Done(convID) }

// Approvals is the app-wide tray, oldest first.
func (s *AIService) Approvals() []agentexec.Pending { return s.exec.Approvals.List() }

// AnswerApproval allows or denies one request by its ID; Go runs its own
// copy of the bytes on its own target.
func (s *AIService) AnswerApproval(requestID string, allow bool) error {
	return s.exec.Approvals.Answer(requestID, allow)
}
