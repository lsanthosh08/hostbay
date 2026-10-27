package mcp

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"hostbay/internal/store"
)

// tokensFile holds the tokens' SHA-256 hashes, never a token. It is in
// the data folder but not in Data, so sync and backup never carry it.
const tokensFile = "mcp-tokens.json"

// tokenPrefix marks a Hostbay MCP token, so a leaked one is recognisable.
const tokenPrefix = "hbmcp_"

// lastUsedEvery is how often a token's last use is written, so a busy
// client does not rewrite the file on every request.
const lastUsedEvery = time.Minute

// Token is one MCP client's token as stored: its hash, never itself.
type Token struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Hash     string `json:"hash"` // hex SHA-256 of the token
	Created  int64  `json:"created"`
	LastUsed int64  `json:"lastUsed,omitempty"` // Unix ms
}

// Tokens is the device-local token list (KTD35).
type Tokens struct {
	st  *store.Store
	Now func() time.Time

	mu sync.Mutex
}

func NewTokens(st *store.Store) *Tokens { return &Tokens{st: st, Now: time.Now} }

func hashOf(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func (t *Tokens) load() ([]Token, error) { return store.Load[[]Token](t.st, tokensFile) }

// List is every token, oldest first.
func (t *Tokens) List() ([]Token, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.load()
}

// Create makes a token named name and returns it with its 256-bit secret,
// which is not kept: only its hash is saved.
func (t *Tokens) Create(name string) (Token, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Token{}, "", errors.New("name the client this token is for")
	}
	var b [32]byte
	_, _ = rand.Read(b[:])
	secret := tokenPrefix + base64.RawURLEncoding.EncodeToString(b[:])
	tok := Token{ID: store.NewID(), Name: name, Hash: hashOf(secret), Created: t.Now().UnixMilli()}
	t.mu.Lock()
	defer t.mu.Unlock()
	all, err := t.load()
	if err != nil {
		return Token{}, "", err
	}
	if err := store.Save(t.st, tokensFile, append(all, tok)); err != nil {
		return Token{}, "", err
	}
	return tok, secret, nil
}

// Revoke deletes token id and returns it.
func (t *Tokens) Revoke(id string) (Token, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	all, err := t.load()
	if err != nil {
		return Token{}, err
	}
	i := slices.IndexFunc(all, func(x Token) bool { return x.ID == id })
	if i < 0 {
		return Token{}, errors.New("token not found")
	}
	tok := all[i]
	return tok, store.Save(t.st, tokensFile, slices.Delete(all, i, i+1))
}

// Verify finds the token whose hash is secret's. The lookup compares
// hashes, so its timing says nothing about the token.
func (t *Tokens) Verify(secret string) (Token, bool) {
	h := hashOf(secret)
	t.mu.Lock()
	defer t.mu.Unlock()
	all, err := t.load()
	if err != nil {
		return Token{}, false
	}
	i := slices.IndexFunc(all, func(x Token) bool { return x.Hash == h })
	if i < 0 {
		return Token{}, false
	}
	now := t.Now().UnixMilli()
	if now-all[i].LastUsed >= lastUsedEvery.Milliseconds() {
		all[i].LastUsed = now
		_ = store.Save(t.st, tokensFile, all) // a lost last-used stamp costs nothing
	}
	return all[i], true
}
