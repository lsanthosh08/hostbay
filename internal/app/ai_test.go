package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"hostbay/internal/agentexec"
	"hostbay/internal/ai/aitest"
	"hostbay/internal/apperr"
	"hostbay/internal/secrets"
	"hostbay/internal/store"
	"hostbay/internal/tlstrust"
)

// aiEvents records everything the AI service emits, as JSON.
type aiEvents struct {
	mu  sync.Mutex
	raw [][]byte
}

func (e *aiEvents) emit(name string, data any) {
	b, _ := json.Marshal(data)
	e.mu.Lock()
	e.raw = append(e.raw, append([]byte(name+" "), b...))
	e.mu.Unlock()
}

func (e *aiEvents) all() []byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	return bytes.Join(e.raw, []byte("\n"))
}

type aiEnv struct {
	s       *AIService
	conns   *ConnService
	dir     string
	ev      *aiEvents
	confirm bool
	gateErr error
	dialogs int
}

func newAIService(t *testing.T) *aiEnv {
	t.Helper()
	_, creds, dir := newHostService(t, fakeVerifier(secrets.Passed))
	conns, _ := newConnService(t)
	env := &aiEnv{conns: conns, dir: dir, ev: &aiEvents{}, confirm: true}
	env.s = NewAIService(creds.st, creds.vault, creds, conns, tlstrust.New(t.TempDir()))
	env.s.emit = env.ev.emit
	env.s.confirm = func(context.Context, string, string) bool { env.dialogs++; return env.confirm }
	env.s.gate = func(context.Context, string) error { return env.gateErr }
	return env
}

func (e *aiEnv) mode(t *testing.T) AIMode {
	t.Helper()
	m, err := e.s.GetMode()
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// A weaker mode needs the native dialog and then OS verification; either
// answering no leaves confirm all. The page's own setter refuses the group.
func TestAIModeChangesNeedTheDialogAndVerification(t *testing.T) {
	e := newAIService(t)
	ctx := context.Background()
	e.confirm = false
	if err := e.s.SetMode(ctx, store.AIDangerousOnly); err == nil || e.mode(t).Mode != store.AIConfirmAll {
		t.Fatalf("dialog said no: %v, mode %q", err, e.mode(t).Mode)
	}
	e.confirm, e.gateErr = true, apperr.New(apperr.VerifyFailed, "cancelled")
	if err := e.s.SetMode(ctx, store.AIDangerousOnly); err == nil || e.mode(t).Mode != store.AIConfirmAll {
		t.Fatalf("verification failed: %v, mode %q", err, e.mode(t).Mode)
	}
	e.gateErr = nil
	if err := e.s.SetMode(ctx, store.AIDangerousOnly); err != nil || e.mode(t).Mode != store.AIDangerousOnly {
		t.Fatalf("both passed: %v, mode %q", err, e.mode(t).Mode)
	}
	if NewSettingsService(e.s.st).Set(store.SettingsGroup{Meta: store.Meta{ID: store.GroupAI}, ExecutionMode: store.AIConfirmAll}) == nil {
		t.Fatal("the page's setter changed the ai group")
	}
	n := e.dialogs
	if err := e.s.SetMode(ctx, store.AIConfirmAll); err != nil || e.dialogs != n || e.mode(t).Mode != store.AIConfirmAll {
		t.Fatal("going back to confirm all asked or failed")
	}
}

// Bypass is on only for this run: after a restart it is off and the saved
// mode is back.
func TestAIBypassIsOffAfterRestart(t *testing.T) {
	e := newAIService(t)
	ctx := context.Background()
	if err := e.s.SetMode(ctx, store.AIDangerousWrite); err != nil {
		t.Fatal(err)
	}
	e.confirm = false
	if e.s.SetBypass(ctx, true) == nil || e.mode(t).Bypass {
		t.Fatal("bypass turned on without the dialog")
	}
	e.confirm = true
	if err := e.s.SetBypass(ctx, true); err != nil || !e.mode(t).Bypass {
		t.Fatal(err)
	}
	restarted := NewAIService(e.s.st, e.s.vault, e.s.creds, e.conns, e.s.certs)
	if m, _ := restarted.GetMode(); m.Bypass || m.Mode != store.AIDangerousWrite {
		t.Fatalf("after restart: %+v", m)
	}
}

const sampleAIKey = "sk-ant-api03-SAMPLE-key-for-hostbay-tests"

// The key is sealed on disk, used in Go, and in no binding result or
// event; a conversation against the fake provider proves it is sent there.
func TestAIProviderKeyNeverLeavesGo(t *testing.T) {
	e := newAIService(t)
	srv := aitest.New(t)
	p, err := e.s.SaveProvider(AIProvider{Name: "Claude", Protocol: "anthropic", BaseURL: srv.URL, Model: "claude-opus-5-5"}, sampleAIKey)
	if err != nil {
		t.Fatal(err)
	}
	list, _ := e.s.ListProviders()
	results, _ := json.Marshal([]any{p, list})
	if bytes.Contains(results, []byte(sampleAIKey)) || p.KeyState != "saved" {
		t.Fatalf("results = %s", results)
	}
	srv.Push(aitest.Reply{Body: aitest.AnthropicText("hello")})
	conv, err := e.s.StartConversation(p.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.Send(conv, "hi"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the reply", func() bool { return bytes.Contains(e.ev.all(), []byte(`"idle"`)) })
	if got := srv.Requests()[0].Header.Get("x-api-key"); got != sampleAIKey {
		t.Fatalf("the provider got key %q", got)
	}
	if !bytes.Contains(e.ev.all(), []byte("hello")) || bytes.Contains(e.ev.all(), []byte(sampleAIKey)) {
		t.Fatalf("events = %s", e.ev.all())
	}
	// Secret scan: no Hostbay file holds the key, readable or obfuscated.
	filepath.WalkDir(e.dir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(path)
			if bytes.Contains(b, []byte(sampleAIKey)) || bytes.Contains(b, []byte("SAMPLE-key")) {
				t.Errorf("%s holds the key", path)
			}
		}
		return nil
	})
}

// Editing the base URL makes the saved key unopenable; testing the edited
// provider without retyping the key sends nothing to the new URL.
func TestAIEditedProviderDoesNotSendTheSavedKey(t *testing.T) {
	e := newAIService(t)
	old, moved := aitest.New(t), aitest.New(t)
	p, err := e.s.SaveProvider(AIProvider{Name: "Claude", Protocol: "anthropic", BaseURL: old.URL, Model: "claude-opus-5-5"}, sampleAIKey)
	if err != nil {
		t.Fatal(err)
	}
	if ids, err := e.s.TestProvider(context.Background(), p, ""); err != nil || len(ids) != 2 || old.Requests()[0].Header.Get("x-api-key") != sampleAIKey {
		t.Fatalf("unchanged provider test = %v, %v", ids, err)
	}
	edited := p
	edited.BaseURL = moved.URL
	if _, err := e.s.TestProvider(context.Background(), edited, ""); apperr.CodeOf(err) != apperr.SecretNeeded {
		t.Fatalf("edited provider test = %v", err)
	}
	saved, err := e.s.SaveProvider(edited, "")
	if err != nil || saved.KeyState != "retype" {
		t.Fatalf("saved edit = %+v, %v", saved, err)
	}
	if _, err := e.s.TestProvider(context.Background(), saved, ""); err == nil {
		t.Fatal("the unopenable key was sent")
	}
	if len(moved.Requests()) != 0 {
		t.Fatal("the new URL saw a request without a retyped key")
	}
	if _, _, err := e.s.provider(p.ID); apperr.CodeOf(err) != apperr.SecretNeeded {
		t.Fatalf("a conversation on it = %v", err)
	}
}

// An allowlist entry needs the dialog and verification; the editor
// refuses a prefix entry whose program runs code, before any dialog.
func TestAIAllowlistEntriesNeedVerification(t *testing.T) {
	e := newAIService(t)
	ctx := context.Background()
	if err := e.s.AddAllowEntry(ctx, "local", "docker", true); err == nil || e.dialogs != 0 {
		t.Fatalf("docker prefix = %v after %d dialogs", err, e.dialogs)
	}
	e.confirm = false
	if err := e.s.AddAllowEntry(ctx, "local", "systemctl restart nginx", false); err == nil {
		t.Fatal("added without the dialog")
	}
	if l, _ := e.s.st.Local(); len(l.Allowlist) != 0 {
		t.Fatalf("local = %+v", l)
	}
	e.confirm = true
	if err := e.s.AddAllowEntry(ctx, "local", "systemctl restart nginx", false); err != nil {
		t.Fatal(err)
	}
	g, _ := e.s.ListAllowlist()
	if len(g) != 1 || g[0].Label != "This computer" || g[0].Entries[0].Command != "systemctl restart nginx" {
		t.Fatalf("allowlist = %+v", g)
	}
	if p := e.s.policy("local"); len(p.Entries) != 1 {
		t.Fatalf("policy = %+v", p)
	}
	if err := e.s.RemoveAllowEntry("local", "systemctl restart nginx", false); err != nil {
		t.Fatal(err)
	}
	if g, _ := e.s.ListAllowlist(); len(g) != 0 {
		t.Fatalf("after remove: %+v", g)
	}
}

// Against an SSH terminal: the shell probe finds bash, a proposal waits
// under confirm all, and after approval the shell gets the envelope.
func TestAIProposalWaitsOnAnSSHTerminal(t *testing.T) {
	c, _, ev, sid := newTermConn(t)
	e := newAIService(t)
	s := NewAIService(e.s.st, e.s.vault, e.s.creds, c, e.s.certs)
	s.emit = e.ev.emit
	id, err := c.PtyOpen(context.Background(), sid, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	c.PtyAttach(id)
	targets := s.Terminals(context.Background())
	if len(targets) != 1 || targets[0].ID != id || targets[0].ConfirmOnly {
		t.Fatalf("targets = %+v", targets)
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.exec.Run(context.Background(), agentexec.Request{Principal: "c1", Target: id, Tool: "run_command", Command: "ls"})
		done <- err
	}()
	waitUntil(t, "the card", func() bool { return len(s.Approvals()) == 1 })
	time.Sleep(50 * time.Millisecond)
	if strings.Contains(ev.joined(PtyDataEvent(id)), "eval") {
		t.Fatal("the shell got the command before approval")
	}
	if !bytes.Contains(e.ev.all(), []byte(AIApprovalsEvent)) {
		t.Fatal("the tray was not told")
	}
	if err := s.AnswerApproval(s.Approvals()[0].ID, true); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the envelope", func() bool { return strings.Contains(ev.joined(PtyDataEvent(id)), " eval 'ls'; printf") })

	// The user's keys go through PtyWrite and count as keystrokes; closing
	// the terminal cancels what waits on it.
	c.PtyWrite(id, "x")
	c.PtyClose(id)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not end when the terminal closed")
	}
	waitUntil(t, "the target gone", func() bool { return len(s.Terminals(context.Background())) == 0 })
}

// Output sent to the provider is scrubbed of the host's saved password and
// of its post-login rules' secret replies (U43), as a session log is.
func TestAISecretsIncludePostLoginReplies(t *testing.T) {
	e := newToolsEnv(t, fakeVerifier(secrets.Passed))
	answer := ruleAnswer
	e.saveRule(t, []store.PostLoginStep{{Expect: "Token:", Kind: store.ReplySecret}}, &answer)
	s := NewAIService(e.c.creds.st, e.c.creds.vault, e.c.creds, e.c, tlstrust.New(t.TempDir()))
	id, _ := e.open(t)
	e.c.track(id, ptyMeta{hostID: e.hostID, kind: "ssh"})
	got := s.secrets(id)
	if !slices.Contains(got, ruleAnswer) || !slices.Contains(got, savedPassword) {
		t.Fatalf("got %d secrets, want the rule's answer and the saved password", len(got))
	}
}

// Removing an entry from an allowlist that holds none must not panic: the
// page can send any host or token.
func TestRemovingAnAllowEntryFromAnEmptyListIsANoOp(t *testing.T) {
	e := newAIService(t)
	if err := e.s.RemoveAllowEntry("local", "uptime", false); err != nil {
		t.Fatal(err)
	}
	m := &MCPService{st: e.s.st}
	if err := m.RemoveAllowEntry("no-such-token", "local", "uptime"); err != nil {
		t.Fatal(err)
	}
}
