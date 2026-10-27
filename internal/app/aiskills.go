package app

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"strings"

	"hostbay/internal/agentexec"
	"hostbay/internal/ai"
	"hostbay/internal/store"
)

// LinkAIDatabase lets questions to SQL read the schema names of the
// databases d has open. It is a function, not a method, so it is no
// binding.
func LinkAIDatabase(a *AIService, d *DatabaseService) { a.db = d }

// ---- Skills and templates (R61) ----

// AISkill is a skill or prompt template as the page sees it. The page
// cannot set AgentAuthored: only an approved save_skill does.
type AISkill struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	Body          string `json:"body"`
	Kind          string `json:"kind"` // "skill" or "template"
	AgentAuthored bool   `json:"agentAuthored"`
}

func skillUI(s store.Skill) AISkill {
	return AISkill{ID: s.ID, Name: s.Name, Description: s.Description, Body: s.Body, Kind: s.Kind, AgentAuthored: s.AgentAuthored}
}

// ListSkills lists the saved skills and templates by name.
func (s *AIService) ListSkills() ([]AISkill, error) {
	d, err := s.st.Data()
	if err != nil {
		return nil, err
	}
	out := make([]AISkill, 0, len(d.Skills.Items))
	for _, sk := range d.Skills.Items {
		out = append(out, skillUI(sk))
	}
	slices.SortFunc(out, func(a, b AISkill) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// SaveSkill saves a skill or template from the settings screen. A name
// another one has is refused.
func (s *AIService) SaveSkill(in AISkill) (AISkill, error) {
	sk := store.Skill{Meta: store.Meta{ID: in.ID}, Name: strings.TrimSpace(in.Name), Description: strings.TrimSpace(in.Description), Body: in.Body, Kind: in.Kind}
	if err := store.CheckSkill(sk); err != nil {
		return AISkill{}, err
	}
	var out store.Skill
	err := s.st.Edit(func(d *store.Data) error {
		if o := d.SkillNamed(sk.Name); o != nil && o.ID != sk.ID {
			return errors.New("a skill or template with that name exists")
		}
		if cur := d.Skill(sk.ID); cur != nil {
			sk.Meta, sk.AgentAuthored = cur.Meta, cur.AgentAuthored
		} else {
			sk.ID = ""
		}
		out = d.PutSkill(sk)
		return nil
	})
	return skillUI(out), err
}

func (s *AIService) DeleteSkill(id string) error {
	return s.st.Edit(func(d *store.Data) error { return d.PurgeSkill(id) })
}

// skillNamed is the skill /name attaches, read when the message is sent.
func (s *AIService) skillNamed(name string) *store.Skill {
	d, err := s.st.Data()
	if err != nil {
		return nil
	}
	return d.SkillNamed(name)
}

// saveAgentSkill stores a save_skill proposal the user approved, in place
// of the skill with its name.
func (s *AIService) saveAgentSkill(sk store.Skill) error {
	return s.st.Edit(func(d *store.Data) error {
		if cur := d.SkillNamed(sk.Name); cur != nil {
			sk.Meta = cur.Meta
		}
		d.PutSkill(sk)
		return nil
	})
}

// firstProvider opens the first saved provider, which completions and
// questions to SQL use. A Claude Code provider is skipped unless allowLocalOnly
// is set: completion fires on typed lines, which would spend a plan's usage
// limits on every pause in typing (R3).
// shortcut: no per-feature provider choice; add one to the ai settings group when users ask.
func (s *AIService) firstProvider(allowLocalOnly bool) (ai.Provider, string, error) {
	d, err := s.st.Data()
	if err != nil {
		return nil, "", err
	}
	if len(d.AIProviders.Items) == 0 {
		return nil, "", errors.New("add a provider in Settings > AI")
	}
	for _, p := range d.AIProviders.Items {
		if allowLocalOnly || !p.LocalOnly() {
			return s.provider(p.ID)
		}
	}
	return nil, "", errors.New("AI suggestions need an API-key provider; add one in Settings > AI")
}

// ---- Completion (R62) ----

// historySent is how many of the host's last commands a completion sends.
const historySent = 20

// Suggest completes line, the command being typed in terminal ptyID, with
// the AI. It is off until the user turns AI suggestions on, because it
// sends what is typed, and off where Go adds local echo (KTD39); the page
// sends nothing from a line typed with echo off. The request holds the
// line, the host's kind and OS and its last 20 history entries, scrubbed
// of the host's secrets; a line holding one is not sent. The completed
// line is "" or starts with line, and accepting it only types it.
func (s *AIService) Suggest(ctx context.Context, ptyID, line string) (string, error) {
	l, err := s.st.Local()
	if err != nil {
		return "", err
	}
	if l.AISuggestions == nil || !*l.AISuggestions {
		return "", errors.New("AI suggestions are off; turn them on in Settings > AI")
	}
	p := s.conns.tools.get(ptyID)
	if p == nil || p.echo {
		return "", errors.New("this terminal takes no AI suggestions")
	}
	// What Go already knows: no shell probe runs for a suggestion.
	m := s.conns.pty(ptyID)
	info := agentexec.TermInfo{Kind: m.kind}
	if m.probed {
		info.Shell = m.shell
	}
	secrets := s.secrets(ptyID)
	if strings.TrimSpace(line) == "" || ai.Clean(line, secrets) != line {
		return "", nil
	}
	hist, err := s.st.History(p.historyKey())
	if err != nil {
		return "", err
	}
	hist = hist[max(0, len(hist)-historySent):]
	for i := range hist {
		hist[i] = ai.Clean(hist[i], secrets)
	}
	prov, model, err := s.firstProvider(false)
	if err != nil {
		return "", err
	}
	return ai.Suggest(ctx, prov, model, ai.SuggestInput{Line: line, Kind: info.Kind, OS: osFamily(info), History: hist})
}

// osFamily is what Go knows of a terminal's OS: this computer's for a
// local shell, else the family its probed shell implies.
func osFamily(t agentexec.TermInfo) string {
	switch {
	case t.Kind == "local":
		return runtime.GOOS
	case t.Shell == agentexec.ShellPOSIX:
		return "unix-like"
	case t.Shell == agentexec.ShellPowerShell:
		return "windows"
	}
	return "unknown"
}

// ---- Questions to SQL (R64) ----

// AskSQL turns question into SQL for the open database dbID. Only the
// database's table and column names go with it, never a row. It runs
// nothing: the page puts the SQL in the editor, and Run, the database
// tab's one run path, is the user's confirmation, whatever the AI mode.
func (s *AIService) AskSQL(ctx context.Context, dbID, question string) (string, error) {
	if strings.TrimSpace(question) == "" {
		return "", errors.New("type a question")
	}
	if s.db == nil {
		return "", errors.New("databases are not available")
	}
	kind, tables, err := s.db.schemaNames(ctx, dbID)
	if err != nil {
		return "", err
	}
	prov, model, err := s.firstProvider(true)
	if err != nil {
		return "", err
	}
	q, err := ai.SQL(ctx, prov, model, ai.SQLInput{Dialect: kind, Tables: tables, Question: question})
	if err != nil {
		return "", err
	}
	q = strings.ReplaceAll(q, "\r\n", "\n")
	// The editor must show exactly what Run would send.
	if err := store.CheckVisible(q); err != nil {
		return "", errors.New("the assistant's SQL " + err.Error() + "; ask again")
	}
	return q, nil
}
