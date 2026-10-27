package app

import (
	"context"
	"errors"
	"sync"
	"time"

	"hostbay/internal/secrets"
)

// MasterPasswordEvent asks the UI for the Hostbay master password. It
// carries a MasterPasswordRequest; the UI answers through
// MasterPasswordService.AnswerMasterPassword.
const MasterPasswordEvent = "master-password-prompt"

// passwordPromptTTL is how long the user has to type the master password.
const passwordPromptTTL = 2 * time.Minute

// MasterPasswordRequest is one pending master password prompt.
type MasterPasswordRequest struct {
	RequestID string `json:"requestId"`
	Reason    string `json:"reason"`
}

// MasterPasswordPrompt asks the UI for the Hostbay master password when
// Linux verification needs it (KTD7). Ask blocks the backend call that
// needs verification until the UI answers the opaque request ID, the call
// is cancelled or the prompt times out; only a typed password counts as an
// answer. It only collects a password to check: it never sets one up.
//
// It is not a Wails service: binding Ask would let the page raise prompts.
type MasterPasswordPrompt struct {
	emit func(name string, data any)
	ttl  time.Duration

	mu      sync.Mutex
	pending map[string]chan string
}

func NewMasterPasswordPrompt() *MasterPasswordPrompt {
	return &MasterPasswordPrompt{emit: emitEvent, ttl: passwordPromptTTL, pending: map[string]chan string{}}
}

// Ask is a secrets.PasswordPrompt. ok is false when the user cancels, the
// call's context ends or nobody answers in time.
func (p *MasterPasswordPrompt) Ask(ctx context.Context, reason string) (string, bool) {
	id := newRequestID()
	answer := make(chan string, 1)
	p.mu.Lock()
	p.pending[id] = answer
	p.mu.Unlock()
	// The waiter removes its own entry, so unanswered prompts never pile up.
	defer func() {
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
	}()

	p.emit(MasterPasswordEvent, MasterPasswordRequest{RequestID: id, Reason: reason})
	timeout := time.NewTimer(p.ttl)
	defer timeout.Stop()
	select {
	case typed := <-answer:
		return typed, typed != ""
	case <-ctx.Done():
		return "", false
	case <-timeout.C:
		return "", false
	}
}

// answer hands the typed password to the waiting Ask. Each ID works once;
// unknown, expired and cancelled IDs are refused.
func (p *MasterPasswordPrompt) answer(requestID, password string) error {
	p.mu.Lock()
	ch, ok := p.pending[requestID]
	delete(p.pending, requestID)
	p.mu.Unlock()
	if !ok {
		return errors.New("this password prompt has expired; try again")
	}
	ch <- password // buffered, and this is its only send
	return nil
}

// MasterPasswordService sets up the Hostbay master password and answers the
// backend's prompts for it.
type MasterPasswordService struct {
	vault  *secrets.Vault
	prompt *MasterPasswordPrompt
}

func NewMasterPasswordService(vault *secrets.Vault, prompt *MasterPasswordPrompt) *MasterPasswordService {
	return &MasterPasswordService{vault: vault, prompt: prompt}
}

// NeedsMasterPassword reports whether this computer verifies with a Hostbay
// master password and none is set yet. Only Linux needs one.
func (s *MasterPasswordService) NeedsMasterPassword() (bool, error) {
	return s.vault.NeedsMasterPassword()
}

// SetMasterPassword creates the Hostbay master password. It is its own
// deliberate action, never offered from a reveal, export or sync prompt, and
// it refuses to replace an existing password (KTD7).
func (s *MasterPasswordService) SetMasterPassword(password string) error {
	return s.vault.SetMasterPassword(password)
}

// UnlockVault asks for the master password when the vault is locked after
// a restart (master-password mode without a Secret Service). The UI calls
// it when connecting or saving a password fails as locked, then retries.
func (s *MasterPasswordService) UnlockVault(ctx context.Context) error {
	return s.vault.UnlockWith(ctx, s.prompt.Ask)
}

// AnswerMasterPassword answers a MasterPasswordEvent. An empty password
// cancels the prompt.
func (s *MasterPasswordService) AnswerMasterPassword(requestID, password string) error {
	return s.prompt.answer(requestID, password)
}
