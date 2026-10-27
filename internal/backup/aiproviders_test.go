package backup

import (
	"bytes"
	"strings"
	"testing"

	"hostbay/internal/store"
)

// AI providers travel in a whole-store backup as version 4: an encrypted
// one carries the key and Restore seals it again; a plain one strips it
// (KTD37), and neither holds it readable (secret scan).
func TestAIProvidersRoundTripThroughBackup(t *testing.T) {
	const apiKey = "sk-ant-api03-sample-backup-key"
	st, v := newEnv(t)
	err := st.Edit(func(d *store.Data) error {
		p := store.AIProvider{Meta: store.Meta{ID: store.NewID()}, Name: "Claude", Protocol: "anthropic", BaseURL: "https://api.anthropic.com", Model: "claude-opus-5-5"}
		sealed, err := v.Seal([]byte(apiKey), p.SecretAAD())
		p.APIKey = sealed
		d.AddAIProvider(p)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := Export(data(t, st), v, "", "pass phrase")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte(apiKey)) {
		t.Fatal("encrypted export holds the key in the clear")
	}
	b, err := Read(out, "pass phrase")
	if err != nil || b.SchemaVersion != 4 || len(b.AIProviders) != 1 {
		t.Fatalf("schema %d, %d providers, %v", b.SchemaVersion, len(b.AIProviders), err)
	}
	st2, v2 := newEnv(t)
	for range 2 { // restoring twice adds nothing
		if err := st2.Edit(func(d *store.Data) error { _, _, err := Restore(d, v2, b); return err }); err != nil {
			t.Fatal(err)
		}
	}
	d := data(t, st2)
	if len(d.AIProviders.Items) != 1 {
		t.Fatalf("restored %d providers", len(d.AIProviders.Items))
	}
	p := d.AIProviders.Items[0]
	if k, err := v2.Open(p.APIKey, p.SecretAAD()); err != nil || string(k) != apiKey {
		t.Fatalf("restored key = %q, %v", k, err)
	}

	plain, err := Export(data(t, st), v, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(plain, []byte("enc:v1:")) || bytes.Contains(plain, []byte(apiKey)) || !bytes.Contains(plain, []byte("api.anthropic.com")) {
		t.Fatalf("plain export:\n%s", plain)
	}
}

// A Claude Code provider is a program path on one computer: a backup never
// holds one, and a file that does (hand-made, or from sync's first merge)
// cannot make Hostbay run a program (R6, AE4).
func TestABackupNeverCarriesAClaudeCodeProviderAndRestoreDropsOne(t *testing.T) {
	st, v := newEnv(t)
	err := st.Edit(func(d *store.Data) error {
		d.AddAIProvider(store.AIProvider{Meta: store.Meta{ID: store.NewID()}, Name: "Claude Code", Protocol: store.AIClaudeCode, Model: "sonnet", ExecPath: "/Users/sample/.local/bin/claude", Consented: true})
		d.AddAIProvider(store.AIProvider{Meta: store.Meta{ID: store.NewID()}, Name: "API", Protocol: "anthropic", BaseURL: "https://api.anthropic.com", Model: "m"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := Export(data(t, st), v, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte("claude-code")) || bytes.Contains(out, []byte("execPath")) || bytes.Contains(out, []byte("/Users/sample")) {
		t.Fatalf("the backup holds a Claude Code provider:\n%s", out)
	}

	// A file made by hand: one Claude Code record, and an API record with the two fields set.
	b := Bundle{SchemaVersion: 4, AIProviders: []store.AIProvider{
		{Meta: store.Meta{ID: "planted"}, Name: "Planted", Protocol: store.AIClaudeCode, Model: "sonnet", ExecPath: "/tmp/evil/claude", Consented: true},
		{Meta: store.Meta{ID: "api"}, Name: "API", Protocol: "anthropic", BaseURL: "https://api.anthropic.com", Model: "m", ExecPath: "/tmp/evil/claude", Consented: true},
	}}
	st2, v2 := newEnv(t)
	var notes []string
	if err := st2.Edit(func(d *store.Data) error { _, notes, err = Restore(d, v2, b); return err }); err != nil {
		t.Fatal(err)
	}
	d := data(t, st2)
	if d.AIProvider("planted") != nil {
		t.Fatal("a Claude Code provider was restored")
	}
	if p := d.AIProvider("api"); p == nil || p.ExecPath != "" || p.Consented {
		t.Fatalf("an API provider was restored with a program path: %+v", p)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "Claude Code") {
		t.Fatalf("notes = %q", notes)
	}
}
