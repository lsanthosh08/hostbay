package sync

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"hostbay/internal/store"
)

// addAIProvider saves a provider on d with its key sealed there.
func (d *device) addAIProvider(t *testing.T, key string) string {
	t.Helper()
	var id string
	d.edit(t, func(data *store.Data) error {
		p := store.AIProvider{Meta: store.Meta{ID: store.NewID()}, Name: "Claude", Protocol: "anthropic", BaseURL: "https://api.anthropic.com", Model: "claude-opus-5-5"}
		sealed, err := d.vault.Seal([]byte(key), p.SecretAAD())
		p.APIKey = sealed
		id = data.PutAIProvider(p).ID
		return err
	})
	return id
}

func (d *device) aiKey(t *testing.T, id string) (string, error) {
	t.Helper()
	p := d.data(t).AIProvider(id)
	if p == nil {
		return "", errors.New("no provider")
	}
	k, err := d.vault.Open(p.APIKey, p.SecretAAD())
	return string(k), err
}

// AI providers sync as their own collection, their sealed key re-wrapped so
// it opens on the other computer (KTD31), on a first sync that merges too.
func TestAIProvidersSyncWithTheirKey(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	pid := a.addAIProvider(t, "sk-ant-sample-key")
	a.sync(t)
	b.addHost(t, "only-b", "") // B has data, so its first sync asks and merges
	if _, err := b.eng.Sync(context.Background(), "merge"); err != nil {
		t.Fatal(err)
	}
	if k, err := b.aiKey(t, pid); err != nil || k != "sk-ant-sample-key" {
		t.Fatalf("key on B = %q, %v", k, err)
	}
	for _, p := range b.data(t).AIProviders.Items {
		if strings.Contains(p.APIKey, "sk-ant") {
			t.Fatal("the key is readable in B's store")
		}
	}
	a.edit(t, func(d *store.Data) error { return d.PurgeAIProvider(pid) })
	a.sync(t)
	b.sync(t)
	if b.data(t).AIProvider(pid) != nil {
		t.Fatal("a purged provider came back")
	}
}

// claudeCode saves a Claude Code provider on d, as the app's setup does.
func (d *device) addClaudeCode(t *testing.T) string {
	t.Helper()
	var id string
	d.edit(t, func(data *store.Data) error {
		id = data.PutAIProvider(store.AIProvider{Meta: store.Meta{ID: store.NewID()}, Name: "Claude Code", Protocol: store.AIClaudeCode,
			Model: "sonnet", ExecPath: "/Users/sample/.local/bin/claude", Consented: true}).ID
		return nil
	})
	return id
}

// A Claude Code provider is a program path on this computer: it is never in
// what is uploaded, a pull does not delete it, and the other computer never
// receives it (R6, AE4).
func TestAClaudeCodeProviderNeverLeavesItsComputerAndSurvivesSync(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	cc := a.addClaudeCode(t)
	api := a.addAIProvider(t, "sk-ant-sample-key")
	a.sync(t)
	b.addHost(t, "only-b", "")
	if _, err := b.eng.Sync(context.Background(), "merge"); err != nil {
		t.Fatal(err)
	}
	a.addHost(t, "later", "")
	a.sync(t)
	b.sync(t)
	a.sync(t) // a full round trip

	files, _, err := remote.Fetch(context.Background())
	if err != nil || len(files) == 0 {
		t.Fatalf("fetch: %v", err)
	}
	for _, f := range files {
		_, plain, err := Open(f.Data, a.eng.Key)
		if err != nil {
			t.Fatal(err)
		}
		for _, leak := range []string{"claude-code", "execPath", "/Users/sample", cc} {
			if strings.Contains(string(plain), leak) {
				t.Fatalf("the sync payload holds %q", leak)
			}
		}
	}
	got := a.data(t).AIProvider(cc)
	if got == nil || got.ExecPath != "/Users/sample/.local/bin/claude" || !got.Consented {
		t.Fatalf("the pull deleted or changed the local provider: %+v", got)
	}
	if b.data(t).AIProvider(cc) != nil {
		t.Fatal("the Claude Code provider reached the other computer")
	}
	if b.data(t).AIProvider(api) == nil {
		t.Fatal("the API provider stopped syncing")
	}
}

// Whatever arrives through a pull, a first-sync merge or a restored conflict
// passes collection.set: a Claude Code record is dropped, the two local-only
// fields are cleared on every other record, and the computer's own Claude
// Code provider stays.
func TestAnIncomingRecordCannotChooseAProgramToRun(t *testing.T) {
	d := newDevice(t, &memBackend{})
	own := d.addClaudeCode(t)
	apiID := d.addAIProvider(t, "sk-ant-sample-key")
	var c collection
	for _, x := range collections {
		if x.name == "aiproviders" {
			c = x
		}
	}
	raw := func(v store.AIProvider) Record {
		b, _ := json.Marshal(v)
		r, err := record(b)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	foreign := store.Meta{ID: store.NewID(), Revision: 5}
	other := store.Meta{ID: store.NewID(), Revision: 5}
	existing := d.data(t).AIProvider(apiID).Meta
	existing.Revision += 10 // a newer remote copy of a provider already here
	in := Set{Items: []Record{
		raw(store.AIProvider{Meta: foreign, Name: "Planted", Protocol: store.AIClaudeCode, Model: "sonnet", ExecPath: "/tmp/evil/claude", Consented: true}),
		raw(store.AIProvider{Meta: other, Name: "API", Protocol: "anthropic", BaseURL: "https://api.anthropic.com", Model: "m", ExecPath: "/tmp/evil/claude", Consented: true}),
		raw(store.AIProvider{Meta: existing, Name: "Flipped", Protocol: store.AIClaudeCode, Model: "sonnet", ExecPath: "/tmp/evil/claude", Consented: true}),
	}}
	d.edit(t, func(data *store.Data) error { return c.set(data, in, false) })
	data := d.data(t)
	if data.AIProvider(foreign.ID) != nil {
		t.Fatal("a planted Claude Code record was stored")
	}
	if p := data.AIProvider(other.ID); p == nil || p.ExecPath != "" || p.Consented {
		t.Fatalf("an API provider arrived with a program path: %+v", p)
	}
	if p := data.AIProvider(apiID); p == nil || p.Protocol != "anthropic" || p.Name != "Claude" {
		t.Fatalf("a remote record flipped an API provider into Claude Code: %+v", p)
	}
	if p := data.AIProvider(own); p == nil || p.ExecPath == "" || !p.Consented {
		t.Fatalf("this computer's own Claude Code provider was lost: %+v", p)
	}
	if got, _ := c.get(data); len(got.Items) != 2 { // upload sees the two API providers only
		t.Fatalf("upload holds %d providers", len(got.Items))
	}
}

// encoding/json matches keys without regard to case, so a record can name its
// protocol, program and consent in any spelling and still decode into them:
// the filter has to read a record the way the store will.
func TestARecordWithOddlyCasedKeysCannotChooseAProgramToRun(t *testing.T) {
	d := newDevice(t, &memBackend{})
	own := d.addClaudeCode(t)
	var c collection
	for _, x := range collections {
		if x.name == "aiproviders" {
			c = x
		}
	}
	rec := func(js string) Record {
		r, err := record(json.RawMessage(js))
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	ownMeta := d.data(t).AIProvider(own).Meta
	in := Set{Items: []Record{
		rec(`{"id":"cased-1","revision":5,"name":"x","Protocol":"claude-code","model":"sonnet","ExecPath":"/tmp/evil/claude","Consented":true}`),
		rec(`{"id":"cased-2","revision":5,"name":"x","protocol":"anthropic","baseUrl":"https://api.anthropic.com","model":"m","EXECPATH":"/tmp/evil/claude","CONSENTED":true}`),
		rec(`{"id":"cased-3","revision":5,"name":"x","protocol":"anthropic","Protocol":"claude-code","model":"m","execpath":"/tmp/evil/claude","consented":true}`),
		// a record that reuses the ID of this computer's own provider
		rec(`{"id":"` + ownMeta.ID + `","revision":99,"name":"Shadow","protocol":"openai-chat","baseUrl":"https://example.com","model":"m"}`),
	}}
	d.edit(t, func(data *store.Data) error { return c.set(data, in, false) })
	data := d.data(t)
	for _, id := range []string{"cased-1", "cased-3"} {
		if data.AIProvider(id) != nil {
			t.Fatalf("a Claude Code record with differently cased keys was stored (%s)", id)
		}
	}
	if p := data.AIProvider("cased-2"); p == nil || p.ExecPath != "" || p.Consented {
		t.Fatalf("an API provider arrived with a program path: %+v", p)
	}
	n := 0
	for _, p := range data.AIProviders.Items {
		if p.ID == ownMeta.ID {
			n++
			if p.Protocol != store.AIClaudeCode || p.ExecPath == "" || !p.Consented {
				t.Fatalf("this computer's provider was replaced: %+v", p)
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d providers with this computer's provider ID, want 1", n)
	}
}
