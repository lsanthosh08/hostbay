package ai

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"hostbay/internal/agentexec"
	"hostbay/internal/ai/aitest"
	"hostbay/internal/store"
)

// skillBook is a store of skills for the agent's hooks.
type skillBook struct {
	mu    sync.Mutex
	items map[string]store.Skill
	saved []store.Skill
}

func newSkillBook(s ...store.Skill) *skillBook {
	b := &skillBook{items: map[string]store.Skill{}}
	for _, x := range s {
		b.items[x.Name] = x
	}
	return b
}

func (b *skillBook) named(name string) *store.Skill {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s, ok := b.items[name]; ok {
		return &s
	}
	return nil
}

func (b *skillBook) save(s store.Skill) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.items[s.Name] = s
	b.saved = append(b.saved, s)
	return nil
}

func (b *skillBook) savedList() []store.Skill {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]store.Skill(nil), b.saved...)
}

var (
	deployCheck = store.Skill{Name: "deploy-check", Kind: store.SkillKind, Body: "DEPLOY-BODY: check nginx -t, then systemctl status nginx."}
	secretSkill = store.Skill{Name: "other", Kind: store.SkillKind, Body: "OTHER-BODY: never attached"}
	explain     = store.Skill{Name: "explain", Kind: store.TemplateKind, Body: "Explain simply: {{input}}. Keep it short."}
	review      = store.Skill{Name: "review", Kind: store.TemplateKind, Body: "Review this config."}
)

func TestExpand(t *testing.T) {
	book := newSkillBook(deployCheck, secretSkill, explain, review)
	text := func(bs []Block) []string {
		var out []string
		for _, b := range bs {
			out = append(out, b.Text)
		}
		return out
	}
	cases := []struct {
		in   string
		want []string
	}{
		{"is the disk full?", []string{"is the disk full?"}},
		{"/deploy-check now", []string{skillText(deployCheck), "/deploy-check now"}},
		{"/deploy-check", []string{skillText(deployCheck), "/deploy-check"}},
		{"/explain this error", []string{"Explain simply: this error. Keep it short."}},
		{"/review", []string{"Review this config."}},
		{"/review nginx.conf", []string{"Review this config.\n\nnginx.conf"}},
		{"/deploy-check /explain the output", []string{skillText(deployCheck), "Explain simply: the output. Keep it short."}},
		{"/etc/hosts is empty", []string{"/etc/hosts is empty"}}, // not a saved name: literal text
		{"/missing please", []string{"/missing please"}},
		{"look at /deploy-check", []string{"look at /deploy-check"}}, // only leading names attach
	}
	for _, c := range cases {
		got := text(Expand(c.in, book.named))
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("Expand(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := text(Expand("/deploy-check", nil)); len(got) != 1 || got[0] != "/deploy-check" {
		t.Errorf("with no skills: %q", got)
	}
}

// /deploy-check attaches that skill: the provider receives its body and
// never the body of a skill the user did not attach.
func TestAgentSendsOnlyAttachedSkills(t *testing.T) {
	h := newHarness(t)
	book := newSkillBook(deployCheck, secretSkill)
	h.agent.Skills = book.named
	h.srv.Push(aitest.Reply{Body: aitest.AnthropicText("ok")}, aitest.Reply{Body: aitest.AnthropicText("ok")})
	conv := h.agent.Start("p1", "")
	if err := h.agent.Send(conv, "/deploy-check on web"); err != nil {
		t.Fatal(err)
	}
	h.idle()
	if err := h.agent.Send(conv, "and again"); err != nil {
		t.Fatal(err)
	}
	h.idle()
	for i, r := range h.srv.Requests() {
		if !bytes.Contains(r.Body, []byte("DEPLOY-BODY")) || bytes.Contains(r.Body, []byte("OTHER-BODY")) {
			t.Fatalf("request %d: %s", i, r.Body)
		}
	}
}

// /explain expands its template around the user's text.
func TestAgentExpandsTemplates(t *testing.T) {
	h := newHarness(t)
	h.agent.Skills = newSkillBook(explain).named
	h.srv.Push(aitest.Reply{Body: aitest.AnthropicText("ok")})
	conv := h.agent.Start("p1", "")
	h.agent.Send(conv, "/explain this error")
	h.idle()
	if r := h.srv.Requests(); len(r) != 1 || !bytes.Contains(r[0].Body, []byte("Explain simply: this error. Keep it short.")) || bytes.Contains(r[0].Body, []byte("/explain")) {
		t.Fatalf("requests = %+v", r)
	}
}

func saveSkillInput(name, body string) string {
	b, _ := json.Marshal(map[string]string{"name": name, "description": "Checks a deploy", "body": body})
	return string(b)
}

// A save_skill proposal waits on a card with the full name and body.
// Denying stores nothing; approving stores Go's copy, marked
// agent-authored, and a known name is shown as a replacement.
func TestAgentSaveSkillAsksFirst(t *testing.T) {
	h := newHarness(t)
	book := newSkillBook(deployCheck)
	h.agent.Skills, h.agent.SaveSkill = book.named, book.save
	body := "Step 1: run uptime.\nStep 2: <script>alert(1)</script>"
	h.srv.Push(aitest.Reply{Body: aitest.AnthropicTool("toolu_1", "save_skill", saveSkillInput("uptime-check", body))}, aitest.Reply{Body: aitest.AnthropicText("Not saved.")})
	conv := h.agent.Start("p1", "")
	h.agent.Send(conv, "remember this")
	card := h.card()
	if card.Tool != "save_skill" || card.Name != "uptime-check" || card.Bytes != body || card.Replaces || card.Target != "" {
		t.Fatalf("card = %+v", card)
	}
	h.agent.Exec.Approvals.Answer(card.ID, false)
	h.idle()
	if len(book.savedList()) != 0 || !strings.Contains(h.toolResult(1), "not saved") {
		t.Fatalf("denied: saved %+v, result %q", book.savedList(), h.toolResult(1))
	}

	h.srv.Push(aitest.Reply{Body: aitest.AnthropicTool("toolu_2", "save_skill", saveSkillInput("deploy-check", body))}, aitest.Reply{Body: aitest.AnthropicText("Saved.")})
	h.agent.Send(conv, "save it over deploy-check")
	card = h.card()
	if !card.Replaces || card.Name != "deploy-check" {
		t.Fatalf("replacing card = %+v", card)
	}
	h.agent.Exec.Approvals.Answer(card.ID, true)
	h.idle()
	got := book.savedList()
	if len(got) != 1 || got[0].Name != "deploy-check" || got[0].Body != body || got[0].Kind != store.SkillKind || !got[0].AgentAuthored {
		t.Fatalf("saved = %+v", got)
	}
	if ev := h.saw("skill"); ev == nil || ev.Name != "uptime-check" || ev.Text != body {
		t.Fatalf("skill event = %+v", ev)
	}
}

// save_skill asks in bypass too (KTD33), and a body with an escape
// sequence is refused before any card.
func TestAgentSaveSkillAsksInBypass(t *testing.T) {
	h := newHarness(t)
	h.agent.Exec = agentexec.New(h.terms, func(string) agentexec.Policy { return agentexec.Policy{Mode: store.AIDangerousOnly, Bypass: true} })
	h.agent.Exec.Wait = 2 * time.Second
	book := newSkillBook()
	h.agent.Skills, h.agent.SaveSkill = book.named, book.save
	h.srv.Push(aitest.Reply{Body: aitest.AnthropicTool("toolu_1", "save_skill", saveSkillInput("bad", "red \x1b[31m"))},
		aitest.Reply{Body: aitest.AnthropicTool("toolu_2", "save_skill", saveSkillInput("ok-skill", "fine"))},
		aitest.Reply{Body: aitest.AnthropicText("done")})
	conv := h.agent.Start("p1", "")
	h.agent.Send(conv, "save")
	card := h.card()
	if card.Name != "ok-skill" || len(book.savedList()) != 0 {
		t.Fatalf("card = %+v, saved %+v", card, book.savedList())
	}
	if !strings.Contains(h.toolResult(1), "control character") {
		t.Fatalf("refusal = %q", h.toolResult(1))
	}
	h.agent.Exec.Approvals.Answer(card.ID, true)
	h.idle()
	if len(book.savedList()) != 1 {
		t.Fatalf("saved = %+v", book.savedList())
	}
}
