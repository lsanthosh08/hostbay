package store

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// A rule's custom secret is bound to the rule and to where its host points,
// so editing the host's hostname, port or user, or moving the sealed value
// to another rule, leaves it unopenable (KTD39).
func TestPostLoginAADFollowsTheRuleAndTheHost(t *testing.T) {
	h := Host{Meta: Meta{ID: "h1"}, Hostname: "web.example", Port: 22, User: "root"}
	aad := PostLoginAAD("r1", &h)
	if !bytes.HasPrefix(aad, []byte("hostbay postlogin\x00r1\x00")) || !bytes.HasSuffix(aad, h.SecretAAD()) {
		t.Fatalf("aad = %q", aad)
	}
	if bytes.Equal(aad, PostLoginAAD("r2", &h)) {
		t.Fatal("two rules share a binding")
	}
	h.Hostname = "elsewhere.example"
	if bytes.Equal(aad, PostLoginAAD("r1", &h)) {
		t.Fatal("the binding ignores the hostname")
	}
}

func TestPostLoginRulesPersistAndALocalSaveConfirms(t *testing.T) {
	s := openTestStore(t)
	var id string
	edit(t, s, func(d *Data) error {
		r := d.AddPostLogin(PostLoginRule{HostID: "h1", Steps: []PostLoginStep{{Reply: "uptime"}}, Held: true})
		id = r.ID
		d.AddPostLogin(PostLoginRule{HostID: "h2"})
		return nil
	})
	d := edit(t, s, func(d *Data) error {
		r := *d.PostLoginRule(id)
		r.Steps = append(r.Steps, PostLoginStep{Expect: "Password:", Kind: ReplyPassword})
		d.PutPostLogin(r)
		return nil
	})
	rules := d.PostLoginRules("h1")
	if len(rules) != 1 || len(rules[0].Steps) != 2 || rules[0].Held || !rules[0].Steps[1].Secret() {
		t.Fatalf("rules = %+v", rules)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "postlogin.json")); err != nil {
		t.Fatal(err)
	}
}
