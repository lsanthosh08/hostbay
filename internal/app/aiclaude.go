package app

import (
	"context"
	"errors"
	"fmt"
	"os"

	"hostbay/internal/ai/claudecode"
	"hostbay/internal/apperr"
	"hostbay/internal/store"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// The Claude Code provider's setup (KTD6, KTD7). The program path is chosen
// and checked in Go: the page lists what Go found, names one by its index or
// asks for Go's own file dialog, and can never write a path. Nothing here
// reads a Claude credential; the only things asked of the program are its
// version and `auth status`, and only for the program the person selected.

// ClaudeCodeCandidate is a program that may be Claude Code, found by path
// only. Listing ran nothing.
type ClaudeCodeCandidate struct {
	Index  int    `json:"index"`
	Path   string `json:"path"`
	Source string `json:"source"` // "path", "native", "desktop" or "picked"
}

// ClaudeCodeStatus is where setup stands for one provider.
type ClaudeCodeStatus struct {
	// State is "notFound", "tooOld", "signedOut", "unsupported", "consent"
	// or "ready".
	State      string `json:"state"`
	Message    string `json:"message"`
	Path       string `json:"path"`
	Target     string `json:"target"` // what the path resolves to, which is what runs
	Version    string `json:"version"`
	AuthMethod string `json:"authMethod"`
	Consented  bool   `json:"consented"`
}

func discoverClaudeCode() []claudecode.Candidate {
	home, _ := os.UserHomeDir()
	return claudecode.DefaultRules().Discover(home, os.Getenv("PATH"), "", os.Getenv("APPDATA"))
}

func pickClaudeProgram() (string, error) {
	a := application.Get()
	if a == nil {
		return "", errors.New("no window to ask in")
	}
	return a.Dialog.OpenFile().CanChooseFiles(true).SetTitle("Choose the Claude Code program").PromptForSingleSelection()
}

// claudeProvider is the client for a saved Claude Code provider. It needs
// the program chosen and the person's consent for that path.
func (s *AIService) claudeProvider(p store.AIProvider) (*claudecode.Provider, error) {
	if p.ExecPath == "" || !p.Consented {
		return nil, apperr.New(apperr.ClaudeCodeMissing, "Set up Claude Code in Settings > AI first: choose the program and confirm.")
	}
	cp := claudecode.New(p.ExecPath)
	s.mu.Lock()
	cp.SetChecked(s.tested[p.ExecPath])
	s.mu.Unlock()
	return cp, nil
}

// ClaudeCodeCandidates lists where Claude Code may be, by path only, and
// holds the list so ClaudeCodeSelect can take one by index.
func (s *AIService) ClaudeCodeCandidates() []ClaudeCodeCandidate {
	found := s.discover()
	s.mu.Lock()
	s.cands = found
	s.mu.Unlock()
	out := make([]ClaudeCodeCandidate, len(found))
	for i, c := range found {
		out[i] = ClaudeCodeCandidate{Index: i, Path: c.Path, Source: c.Source}
	}
	return out
}

// ClaudeCodeSelect makes candidate index (from the last ClaudeCodeCandidates)
// the program of provider id. A different path clears the consent.
func (s *AIService) ClaudeCodeSelect(id string, index int) error {
	s.mu.Lock()
	var path string
	if index >= 0 && index < len(s.cands) {
		path = s.cands[index].Path
	}
	s.mu.Unlock()
	if path == "" {
		return errors.New("list the programs again and choose one")
	}
	return s.setClaudePath(id, path)
}

// ClaudeCodePickFile asks, in Go's file dialog, for the program. A cancelled
// dialog changes nothing.
func (s *AIService) ClaudeCodePickFile(id string) error {
	path, err := s.pickProgram()
	if err != nil || path == "" {
		return err
	}
	return s.setClaudePath(id, path)
}

func (s *AIService) setClaudePath(id, path string) error {
	res, err := s.claude.Check(path)
	if err != nil {
		if open := s.claude.OpenTo(path); open != "" {
			return apperr.New(apperr.ClaudeCodeMissing, "Hostbay only runs a program that only you can change, and "+open+" can be changed by other users (for example through group write access). Remove that access, for example with chmod go-w, then choose the program again.")
		}
		return apperr.New(apperr.ClaudeCodeMissing, "That is not a Claude Code program Hostbay can run: it must be a real executable that only you can change.")
	}
	return s.st.Edit(func(d *store.Data) error {
		cur := d.AIProvider(id)
		if cur == nil || cur.Protocol != store.AIClaudeCode {
			return errors.New("provider not found")
		}
		p := *cur
		if p.ExecPath != res.Path {
			p.ExecPath, p.Consented = res.Path, false
		}
		d.PutAIProvider(p)
		return nil
	})
}

func (s *AIService) savedClaude(id string) (store.AIProvider, error) {
	d, err := s.st.Data()
	if err != nil {
		return store.AIProvider{}, err
	}
	p := d.AIProvider(id)
	if p == nil || p.Protocol != store.AIClaudeCode {
		return store.AIProvider{}, errors.New("provider not found")
	}
	return *p, nil
}

// ClaudeCodeState inspects the selected program (its version and sign-in
// status, no model call) and says what the person can do next.
func (s *AIService) ClaudeCodeState(ctx context.Context, id string) (ClaudeCodeStatus, error) {
	p, err := s.savedClaude(id)
	if err != nil {
		return ClaudeCodeStatus{}, err
	}
	st := ClaudeCodeStatus{State: "notFound", Path: p.ExecPath, Consented: p.Consented}
	if p.ExecPath == "" {
		st.Message = "Choose where Claude Code is installed."
		return st, nil
	}
	info, err := claudecode.New(p.ExecPath).Inspect(ctx)
	st.Target, st.Version, st.AuthMethod = info.Target, info.Version, info.Status.AuthMethod
	switch code := apperr.CodeOf(err); {
	case err == nil:
		st.State = "consent"
		if p.Consented {
			st.State = "ready"
		}
	case code == apperr.ClaudeCodeMissing:
		st.Message = err.Error()
	case code == apperr.ClaudeCodeTooOld:
		st.State, st.Message = "tooOld", err.Error()
	case code == apperr.ClaudeCodeSignedOut:
		st.State, st.Message = "signedOut", err.Error()
	default:
		st.State, st.Message = "unsupported", err.Error()
	}
	return st, nil
}

const claudeTerms = "https://code.claude.com/docs/en/legal-and-compliance"

// ClaudeCodeConsent asks, in a dialog Go opens, whether this program may be
// used, and records the answer for its path. The page can ask but cannot
// answer. It refuses unless the program is ready: found, new enough and
// signed in.
func (s *AIService) ClaudeCodeConsent(ctx context.Context, id string) error {
	p, err := s.savedClaude(id)
	if err != nil {
		return err
	}
	if p.ExecPath == "" {
		return errors.New("choose the Claude Code program first")
	}
	info, err := claudecode.New(p.ExecPath).Inspect(ctx)
	if err != nil {
		return err
	}
	text := fmt.Sprintf("Program: %s\nResolves to: %s\nVersion: %s\nSigned in with: %s\n\n"+
		"Hostbay will run this program for the AI assistant. Your messages and the terminal text and commands the assistant works on go from the program straight to Anthropic, not through Hostbay's proxies, and count against that sign-in's usage limits.\n\n"+
		"Hostbay never reads or stores your Claude credentials; signing in stays your own step in your own terminal. Anthropic's terms: %s",
		info.Path, info.Target, info.Version, info.Status.AuthMethod, claudeTerms)
	if !s.confirm(ctx, "Let Hostbay use your Claude Code sign-in?", text) {
		return apperr.New(apperr.VerifyFailed, "Cancelled; nothing changed.")
	}
	return s.st.Edit(func(d *store.Data) error {
		cur := d.AIProvider(id)
		if cur == nil || cur.Protocol != store.AIClaudeCode || cur.ExecPath != info.Path {
			return errors.New("the selected program changed; confirm again")
		}
		c := *cur
		c.Consented = true
		d.PutAIProvider(c)
		return nil
	})
}
