package app

import (
	"context"
	"os"
	"strings"
	"testing"

	"hostbay/internal/ai/claudecode"
	"hostbay/internal/ai/claudecode/claudetest"
	"hostbay/internal/apperr"
	"hostbay/internal/store"
)

const (
	signedIn  = `{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty"}`
	signedOut = `{"loggedIn":false,"authMethod":"none","apiProvider":"firstParty"}`
)

// claudeEnv is an AI service whose discovery finds fake Claude Code programs.
type claudeEnv struct {
	*aiEnv
	fakes []*claudetest.Fake
	text  string // the last consent dialog's message
}

func newClaudeEnv(t *testing.T, scripts ...claudetest.Script) *claudeEnv {
	t.Helper()
	e := &claudeEnv{aiEnv: newAIService(t)}
	var found []claudecode.Candidate
	for _, s := range scripts {
		f := claudetest.Install(t, s)
		e.fakes = append(e.fakes, f)
		found = append(found, claudecode.Candidate{Path: f.Path, Source: "path"})
	}
	e.s.discover = func() []claudecode.Candidate { return found }
	e.s.confirm = func(_ context.Context, _, message string) bool { e.text = message; e.dialogs++; return e.confirm }
	return e
}

// add saves a Claude Code provider through the page's binding and returns its ID.
func (e *claudeEnv) add(t *testing.T) string {
	t.Helper()
	p, err := e.s.SaveProvider(AIProvider{Name: "Claude Code", Protocol: store.AIClaudeCode, Model: "sonnet"}, "")
	if err != nil {
		t.Fatal(err)
	}
	return p.ID
}

func (e *claudeEnv) stored(t *testing.T, id string) store.AIProvider {
	t.Helper()
	d, err := e.s.st.Data()
	if err != nil {
		t.Fatal(err)
	}
	p := d.AIProvider(id)
	if p == nil {
		t.Fatal("provider not saved")
	}
	return *p
}

func (e *claudeEnv) ready(t *testing.T, id string) {
	t.Helper()
	e.s.ClaudeCodeCandidates()
	if err := e.s.ClaudeCodeSelect(id, 0); err != nil {
		t.Fatal(err)
	}
	if err := e.s.ClaudeCodeConsent(context.Background(), id); err != nil {
		t.Fatal(err)
	}
}

func TestAPathFromThePageIsIgnoredAndOneFromDetectIsStored(t *testing.T) {
	e := newClaudeEnv(t, claudetest.Script{Auth: signedIn})
	p, err := e.s.SaveProvider(AIProvider{Name: "Claude Code", Protocol: store.AIClaudeCode, Model: "sonnet",
		ExecPath: "/tmp/evil/claude", Consented: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := e.stored(t, p.ID); got.ExecPath != "" || got.Consented {
		t.Fatalf("the page set %q consented=%v", got.ExecPath, got.Consented)
	}
	cands := e.s.ClaudeCodeCandidates()
	if len(cands) != 1 || cands[0].Path != e.fakes[0].Path {
		t.Fatalf("candidates = %+v", cands)
	}
	if err := e.s.ClaudeCodeSelect(p.ID, 0); err != nil {
		t.Fatal(err)
	}
	if got := e.stored(t, p.ID).ExecPath; got != e.fakes[0].Path {
		t.Fatalf("stored %q", got)
	}
	// Saving the form again, with another path, keeps the chosen one.
	if _, err := e.s.SaveProvider(AIProvider{ID: p.ID, Name: "Renamed", Protocol: store.AIClaudeCode, Model: "opus", ExecPath: "/tmp/evil/claude"}, ""); err != nil {
		t.Fatal(err)
	}
	if got := e.stored(t, p.ID); got.ExecPath != e.fakes[0].Path || got.Name != "Renamed" {
		t.Fatalf("stored %+v", got)
	}
	if err := e.s.ClaudeCodeSelect(p.ID, 7); err == nil {
		t.Fatal("an index that was never listed was taken")
	}
	if len(e.fakes[0].Calls(t)) != 0 {
		t.Fatal("choosing a program ran it")
	}
}

func TestAPathTheChecksRefuseIsNotStored(t *testing.T) {
	e := newClaudeEnv(t)
	id := e.add(t)
	e.s.pickProgram = func() (string, error) { return "/definitely/not/here/claude", nil }
	if err := e.s.ClaudeCodePickFile(id); apperr.CodeOf(err) != apperr.ClaudeCodeMissing || e.stored(t, id).ExecPath != "" {
		t.Fatalf("err %v, stored %q", err, e.stored(t, id).ExecPath)
	}
	f := claudetest.Install(t, claudetest.Script{Auth: signedIn})
	e.s.pickProgram = func() (string, error) { return f.Path, nil }
	if err := e.s.ClaudeCodePickFile(id); err != nil || e.stored(t, id).ExecPath != f.Path {
		t.Fatalf("a picked program: %v, stored %q", err, e.stored(t, id).ExecPath)
	}
	e.s.pickProgram = func() (string, error) { return "", nil } // cancelled
	if err := e.s.ClaudeCodePickFile(id); err != nil || e.stored(t, id).ExecPath != f.Path {
		t.Fatal("a cancelled dialog changed the provider")
	}
}

func TestEachSetupStateShowsAndOnlyReadyProvidersRun(t *testing.T) {
	cases := []struct {
		name   string
		script claudetest.Script
		state  string
	}{
		{"consent", claudetest.Script{Auth: signedIn}, "consent"},
		{"signed out", claudetest.Script{Auth: signedOut, AuthExit: 1}, "signedOut"},
		{"too old", claudetest.Script{Version: "2.1.1 (Claude Code)", Auth: signedIn}, "tooOld"},
		{"another provider", claudetest.Script{Auth: `{"loggedIn":true,"authMethod":"api_key","apiProvider":"bedrock"}`}, "unsupported"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newClaudeEnv(t, c.script)
			id := e.add(t)
			if st, err := e.s.ClaudeCodeState(t.Context(), id); err != nil || st.State != "notFound" || st.Message == "" {
				t.Fatalf("before choosing: %+v %v", st, err)
			}
			e.s.ClaudeCodeCandidates()
			if err := e.s.ClaudeCodeSelect(id, 0); err != nil {
				t.Fatal(err)
			}
			st, err := e.s.ClaudeCodeState(t.Context(), id)
			if err != nil || st.State != c.state || st.Path != e.fakes[0].Path || st.Target == "" {
				t.Fatalf("state %+v, %v; want %s", st, err, c.state)
			}
			// Consent is refused unless the program is usable, and the provider does not run.
			err = e.s.ClaudeCodeConsent(t.Context(), id)
			if c.state == "consent" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || e.stored(t, id).Consented || e.dialogs != 0 {
				t.Fatalf("consent for a %s program: err %v dialogs %d", c.name, err, e.dialogs)
			}
			if _, _, err := e.s.provider(id); apperr.CodeOf(err) != apperr.ClaudeCodeMissing {
				t.Fatalf("provider() = %v, want the set-up error", err)
			}
		})
	}
}

func TestConsentIsAGoDialogThatNamesWhatIsSentAndWhoseSignInIsUsed(t *testing.T) {
	e := newClaudeEnv(t, claudetest.Script{Auth: signedIn})
	id := e.add(t)
	e.s.ClaudeCodeCandidates()
	if err := e.s.ClaudeCodeSelect(id, 0); err != nil {
		t.Fatal(err)
	}
	e.confirm = false
	if err := e.s.ClaudeCodeConsent(t.Context(), id); err == nil || e.stored(t, id).Consented {
		t.Fatalf("a cancelled dialog consented: %v", err)
	}
	e.confirm = true
	if err := e.s.ClaudeCodeConsent(t.Context(), id); err != nil || !e.stored(t, id).Consented {
		t.Fatal(err)
	}
	for _, want := range []string{e.fakes[0].Path, "2.1.310", "claude.ai", "not through Hostbay's proxies", "usage limits", "never reads or stores your Claude credentials", "legal-and-compliance"} {
		if !strings.Contains(e.text, want) {
			t.Errorf("the dialog does not say %q:\n%s", want, e.text)
		}
	}
	if st, _ := e.s.ClaudeCodeState(t.Context(), id); st.State != "ready" || !st.Consented || st.AuthMethod != "claude.ai" {
		t.Fatalf("state = %+v", st)
	}
}

func TestChangingTheProgramClearsTheConsent(t *testing.T) {
	e := newClaudeEnv(t, claudetest.Script{Auth: signedIn}, claudetest.Script{Auth: signedIn})
	id := e.add(t)
	e.ready(t, id)
	e.s.ClaudeCodeCandidates()
	if err := e.s.ClaudeCodeSelect(id, 0); err != nil || !e.stored(t, id).Consented {
		t.Fatalf("choosing the same program again cleared the consent: %v", err)
	}
	if err := e.s.ClaudeCodeSelect(id, 1); err != nil {
		t.Fatal(err)
	}
	if got := e.stored(t, id); got.ExecPath != e.fakes[1].Path || got.Consented {
		t.Fatalf("after choosing another program: %+v", got)
	}
}

func TestAClaudeCodeProviderOpensWithNoVaultAccess(t *testing.T) {
	e := newClaudeEnv(t, claudetest.Script{Auth: signedIn})
	id := e.add(t)
	e.ready(t, id)
	e.s.vault = nil // a nil vault panics on first use
	prov, model, err := e.s.provider(id)
	if err != nil || model != "sonnet" {
		t.Fatalf("provider: %v %q", err, model)
	}
	if _, ok := prov.(*claudecode.Provider); !ok {
		t.Fatalf("provider is %T", prov)
	}
}

func TestSavingRulesForAClaudeCodeProvider(t *testing.T) {
	e := newClaudeEnv(t)
	for name, in := range map[string]AIProvider{
		"an address":   {Name: "x", Protocol: store.AIClaudeCode, Model: "sonnet", BaseURL: "https://api.anthropic.com"},
		"a proxy":      {Name: "x", Protocol: store.AIClaudeCode, Model: "sonnet", ProxyID: "p"},
		"no name":      {Protocol: store.AIClaudeCode, Model: "sonnet"},
		"no model":     {Name: "x", Protocol: store.AIClaudeCode},
		"a flag model": {Name: "x", Protocol: store.AIClaudeCode, Model: "--dangerously-skip-permissions"},
	} {
		if _, err := e.s.SaveProvider(in, ""); err == nil {
			t.Errorf("%s was saved", name)
		}
	}
	if _, err := e.s.SaveProvider(AIProvider{Name: "x", Protocol: store.AIClaudeCode, Model: "sonnet"}, "sk-ant-typed"); err == nil {
		t.Error("a key was accepted")
	}
	// Turning an API provider into a Claude Code one drops its key; going back drops the path.
	api, err := e.s.SaveProvider(AIProvider{Name: "API", Protocol: "anthropic", BaseURL: "https://api.anthropic.com", Model: "claude-opus-5-5"}, "sk-ant-secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.SaveProvider(AIProvider{ID: api.ID, Name: "API", Protocol: store.AIClaudeCode, Model: "sonnet"}, ""); err != nil {
		t.Fatal(err)
	}
	if got := e.stored(t, api.ID); got.APIKey != "" || got.BaseURL != "" {
		t.Fatalf("a Claude Code provider kept %+v", got)
	}
}

func TestSwitchingAClaudeCodeProviderBackToAnAPIOneDropsItsPath(t *testing.T) {
	e := newClaudeEnv(t, claudetest.Script{Auth: signedIn})
	id := e.add(t)
	e.ready(t, id)
	if _, err := e.s.SaveProvider(AIProvider{ID: id, Name: "API", Protocol: "anthropic", BaseURL: "https://api.anthropic.com", Model: "claude-opus-5-5"}, ""); err != nil {
		t.Fatal(err)
	}
	if got := e.stored(t, id); got.ExecPath != "" || got.Consented {
		t.Fatalf("the API provider kept %q consented=%v", got.ExecPath, got.Consented)
	}
	if _, err := e.s.SaveProvider(AIProvider{ID: id, Name: "Back", Protocol: store.AIClaudeCode, Model: "sonnet"}, ""); err != nil {
		t.Fatal(err)
	}
	if got := e.stored(t, id); got.ExecPath != "" || got.Consented {
		t.Fatal("switching back brought the old path with it")
	}
}

func TestSuggestionsSkipAClaudeCodeProviderButQuestionsToSQLMayUseIt(t *testing.T) {
	e := newClaudeEnv(t, claudetest.Script{Auth: signedIn})
	cc := e.add(t)
	e.ready(t, cc)
	if _, _, err := e.s.firstProvider(false); err == nil || !strings.Contains(err.Error(), "API-key provider") {
		t.Fatalf("with only a Claude Code provider, completion got %v", err)
	}
	if _, model, err := e.s.firstProvider(true); err != nil || model != "sonnet" {
		t.Fatalf("a question to SQL: %v %q", err, model)
	}
	if _, err := e.s.SaveProvider(AIProvider{Name: "API", Protocol: "anthropic", BaseURL: "https://api.anthropic.com", Model: "claude-opus-5-5"}, "sk-ant-k"); err != nil {
		t.Fatal(err)
	}
	if _, model, err := e.s.firstProvider(false); err != nil || model != "claude-opus-5-5" {
		t.Fatalf("completion should use the API provider: %v %q", err, model)
	}
}

func TestTestProviderMakesOneRealCallAfterConsent(t *testing.T) {
	e := newClaudeEnv(t, claudetest.Script{Auth: signedIn, Steps: []claudetest.Step{{Stdout: `{"type":"system","subtype":"init","tools":[]}` + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"text","text":"ok"}]}}` + "\n" + `{"type":"result","subtype":"success","is_error":false}` + "\n"}}})
	id := e.add(t)
	in := AIProvider{ID: id, Name: "Claude Code", Protocol: store.AIClaudeCode, Model: "sonnet"}
	if _, err := e.s.TestProvider(t.Context(), in, ""); apperr.CodeOf(err) != apperr.ClaudeCodeMissing {
		t.Fatalf("before setup: %v", err)
	}
	e.ready(t, id)
	models, err := e.s.TestProvider(t.Context(), in, "")
	if err != nil || len(models) == 0 || len(e.fakes[0].ModelCalls(t)) != 1 {
		t.Fatalf("models %v err %v calls %d", models, err, len(e.fakes[0].ModelCalls(t)))
	}
}

// A program refused because others could change it says so, and names the
// folder, so the person can fix it.
func TestARefusalForSharedAccessSaysWhatToChange(t *testing.T) {
	e := newClaudeEnv(t)
	id := e.add(t)
	f := claudetest.Install(t, claudetest.Script{Auth: signedIn})
	if err := os.Chmod(f.Dir, 0o775); err != nil {
		t.Fatal(err)
	}
	e.s.pickProgram = func() (string, error) { return f.Path, nil }
	err := e.s.ClaudeCodePickFile(id)
	if apperr.CodeOf(err) != apperr.ClaudeCodeMissing || !strings.Contains(err.Error(), f.Dir) || !strings.Contains(err.Error(), "chmod go-w") {
		t.Fatalf("err = %v", err)
	}
	if e.stored(t, id).ExecPath != "" {
		t.Fatal("a refused program was stored")
	}
}
