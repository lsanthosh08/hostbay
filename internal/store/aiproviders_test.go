package store

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestAIProvidersPersistAndLeaveTombstones(t *testing.T) {
	s := openTestStore(t)
	var id string
	d := edit(t, s, func(d *Data) error {
		id = d.PutAIProvider(AIProvider{Name: "Claude", Protocol: "anthropic", BaseURL: "https://api.anthropic.com", Model: "claude-opus-5-5", APIKey: "enc:v1:x"}).ID
		return nil
	})
	if got := d.AIProvider(id); got == nil || got.Revision != 1 || got.APIKey != "enc:v1:x" {
		t.Fatalf("saved provider = %+v", got)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "aiproviders.json")); err != nil {
		t.Fatalf("aiproviders.json not written: %v", err)
	}
	d = edit(t, s, func(d *Data) error { return d.PurgeAIProvider(id) })
	if d.AIProvider(id) != nil || len(d.AIProviders.Tombstones) != 1 {
		t.Fatalf("after purge: %+v", d.AIProviders)
	}
}

// The key is bound to the provider's ID, protocol, normalised base URL and
// proxy, so editing any of them makes it unopenable (KTD31); the model and
// name are not part of it.
func TestAIProviderSecretAADFollowsWhereTheKeyGoes(t *testing.T) {
	p := AIProvider{Meta: Meta{ID: "p1"}, Protocol: "anthropic", BaseURL: "https://api.anthropic.com", Model: "claude-opus-5-5"}
	same := p
	same.BaseURL, same.Model, same.Name = "HTTPS://API.anthropic.com/", "claude-haiku-5-5", "renamed"
	if !bytes.Equal(p.SecretAAD(), same.SecretAAD()) {
		t.Fatal("a cosmetic change moved the binding")
	}
	for _, edit := range []func(q *AIProvider){
		func(q *AIProvider) { q.BaseURL = "https://evil.example" },
		func(q *AIProvider) { q.BaseURL = "http://api.anthropic.com" },
		func(q *AIProvider) { q.Protocol = "openai-chat" },
		func(q *AIProvider) { q.ProxyID = "px" },
		func(q *AIProvider) { q.ID = "p2" },
	} {
		q := p
		edit(&q)
		if bytes.Equal(p.SecretAAD(), q.SecretAAD()) {
			t.Fatalf("%+v kept the binding", q)
		}
	}
}

func TestOnlyAClaudeCodeProviderIsLocalOnly(t *testing.T) {
	if !(AIProvider{Protocol: AIClaudeCode}).LocalOnly() || (AIProvider{Protocol: "anthropic"}).LocalOnly() || (AIProvider{Protocol: "openai-chat"}).LocalOnly() {
		t.Fatal("LocalOnly picks the wrong providers")
	}
	if !slices.Contains(AIProtocols, AIClaudeCode) {
		t.Fatal("the protocol is not offered")
	}
}
