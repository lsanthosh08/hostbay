package store

import (
	"errors"
	"net/url"
	"strings"
)

// AIProvider is an Anthropic- or OpenAI-compatible endpoint the AI agent
// talks to (R60, KTD31). Providers are their own collection, as proxies
// are, so sync carries their one sealed field, APIKey, bound to SecretAAD.
type AIProvider struct {
	Meta
	Name     string `json:"name"`
	Protocol string `json:"protocol"` // "anthropic" or "openai-chat"
	BaseURL  string `json:"baseUrl"`
	Model    string `json:"model"`
	ProxyID  string `json:"proxyId,omitempty"` // the Proxy every request goes through; a missing one is an error
	// Unencrypted is the explicit choice to send the key over http:// to a
	// host that is not this computer.
	Unencrypted bool   `json:"unencrypted,omitempty"`
	APIKey      string `json:"apiKey,omitempty"` // sealed

	// ExecPath and Consented belong to a Claude Code provider only: the
	// program it runs and the person's go-ahead for that path. They mean
	// something on this computer alone, so sync and backup never carry them
	// (KTD7); only Go sets them, from a discovery result or its own dialog.
	ExecPath  string `json:"execPath,omitempty"`
	Consented bool   `json:"consented,omitempty"`
}

// AIClaudeCode is the protocol of a provider that runs the person's own
// signed-in Claude Code (internal/ai/claudecode). It has no URL and no key.
const AIClaudeCode = "claude-code"

// AIProtocols are the provider protocols Hostbay speaks.
var AIProtocols = []string{"anthropic", "openai-chat", AIClaudeCode}

// LocalOnly reports a provider that never leaves this computer.
func (p AIProvider) LocalOnly() bool { return p.Protocol == AIClaudeCode }

// NormalBaseURL is raw with its scheme and host lower-cased and trailing
// slashes removed, so two spellings of one endpoint bind the key alike.
func NormalBaseURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return raw
	}
	u.Scheme, u.Host = strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String()
}

// SecretAAD binds the key to where it may be sent: after the provider's
// protocol, address or proxy changes it no longer opens (KTD31).
func (p *AIProvider) SecretAAD() []byte {
	return []byte("hostbay ai provider\x00" + p.ID + "\x00" + p.Protocol + "\x00" + NormalBaseURL(p.BaseURL) + "\x00" + p.ProxyID)
}

// AIProvider returns the provider with id, or nil.
func (d *Data) AIProvider(id string) *AIProvider {
	if i := find(d.AIProviders.Items, id); i >= 0 {
		return &d.AIProviders.Items[i]
	}
	return nil
}

// PutAIProvider saves p as a local change and returns it with its new Meta.
func (d *Data) PutAIProvider(p AIProvider) AIProvider { return put(d, &d.AIProviders, p) }

// AddAIProvider adds p, keeping any Meta it carries.
func (d *Data) AddAIProvider(p AIProvider) AIProvider { return add(d, &d.AIProviders, p) }

// PurgeAIProvider removes a provider, leaving a tombstone for sync.
func (d *Data) PurgeAIProvider(id string) error {
	if !purge(d, &d.AIProviders, id) {
		return errors.New("AI provider not found")
	}
	return nil
}
