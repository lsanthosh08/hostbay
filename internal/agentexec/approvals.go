package agentexec

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"slices"
	"sync"
	"time"

	"hostbay/internal/apperr"
)

// ApprovalTTL is how long a request waits for an answer before it is
// denied (KTD32).
const ApprovalTTL = 110 * time.Second

// Pending is one request waiting in the app-wide approval tray. Go keeps
// the bytes and the target; the page shows them and answers yes or no to
// the opaque, single-use ID, and on yes Go runs its own copy.
type Pending struct {
	ID        string    `json:"id"`
	Principal string    `json:"principal"`        // the conversation, or "mcp:" and an MCP token's ID
	Client    string    `json:"client,omitempty"` // an MCP request's token name
	Target    string    `json:"target"`           // the terminal it runs in, or an MCP request's host ID, frozen when proposed
	Host      string    `json:"host"`             // the label Go derived from the terminal's owner or the saved host
	Tool      string    `json:"tool"`
	Bytes     string    `json:"bytes"` // exactly what runs
	Size      int       `json:"size"`  // len(Bytes) in bytes
	Expires   time.Time `json:"expires"`
	// A save_skill request (KTD33) writes to Hostbay, not to a terminal:
	// Name is the skill's, Bytes its whole body, and Replaces says a
	// skill with that name is saved already.
	Name     string `json:"name,omitempty"`
	Replaces bool   `json:"replaces,omitempty"`
}

// Approvals holds the pending requests.
type Approvals struct {
	TTL      time.Duration
	Now      func() time.Time
	After    func(time.Duration) <-chan time.Time
	OnChange func([]Pending) // the tray's list changed

	mu  sync.Mutex
	m   map[string]*waiting
	seq int
}

type waiting struct {
	p      Pending
	seq    int
	answer chan bool
	gone   chan error // withdrawn or its target closed
}

func NewApprovals() *Approvals {
	return &Approvals{TTL: ApprovalTTL, Now: time.Now, After: time.After, m: map[string]*waiting{}}
}

var (
	errWithdrawn    = errors.New("the request was withdrawn")
	errTargetClosed = errors.New("the terminal closed before the request was answered; nothing ran")
)

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails on supported platforms
	return hex.EncodeToString(b[:])
}

// ask waits for the user's answer to p. A request not answered within TTL
// is denied; one whose ctx ends, or that is withdrawn or loses its target,
// is cancelled. Either way it leaves the tray.
func (a *Approvals) ask(ctx context.Context, p Pending) (bool, error) {
	p.ID, p.Size, p.Expires = newID(), len(p.Bytes), a.Now().Add(a.TTL)
	w := &waiting{p: p, answer: make(chan bool, 1), gone: make(chan error, 1)}
	a.mu.Lock()
	a.seq++
	w.seq = a.seq
	a.m[p.ID] = w
	a.mu.Unlock()
	a.changed()
	defer func() {
		a.mu.Lock()
		delete(a.m, p.ID)
		a.mu.Unlock()
		a.changed()
	}()
	select {
	case ok := <-w.answer:
		if !ok {
			return false, apperr.New(apperr.ApprovalDenied, "The user denied this command; it did not run.")
		}
		return true, nil
	case err := <-w.gone:
		return false, err
	case <-ctx.Done():
		return false, ctx.Err()
	case <-a.After(a.TTL):
		return false, apperr.New(apperr.ApprovalExpired, "No one answered within 110 seconds, so the command was denied; it did not run.")
	}
}

// Ask puts p in the tray and waits for the answer, as a command's card
// does, whatever the mode: nothing an allowlist or bypass says applies.
func (a *Approvals) Ask(ctx context.Context, p Pending) (bool, error) { return a.ask(ctx, p) }

// Answer resolves a request once; unknown, expired and used IDs are refused.
func (a *Approvals) Answer(id string, allow bool) error {
	a.mu.Lock()
	w, ok := a.m[id]
	delete(a.m, id)
	a.mu.Unlock()
	if !ok {
		return errors.New("this request was already answered, withdrawn or expired")
	}
	w.answer <- allow
	return nil
}

// List is the pending requests, oldest first.
func (a *Approvals) List() []Pending {
	a.mu.Lock()
	defer a.mu.Unlock()
	ws := make([]*waiting, 0, len(a.m))
	for _, w := range a.m {
		ws = append(ws, w)
	}
	slices.SortFunc(ws, func(x, y *waiting) int { return x.seq - y.seq })
	out := make([]Pending, len(ws))
	for i, w := range ws {
		out[i] = w.p
	}
	return out
}

// drop cancels every request match picks with err.
func (a *Approvals) drop(match func(Pending) bool, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, w := range a.m {
		if match(w.p) {
			delete(a.m, id)
			w.gone <- err
		}
	}
}

func (a *Approvals) changed() {
	if a.OnChange != nil {
		a.OnChange(a.List())
	}
}
