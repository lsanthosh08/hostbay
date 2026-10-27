package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSkillsPersistAndLeaveTombstones(t *testing.T) {
	s := openTestStore(t)
	var id string
	d := edit(t, s, func(d *Data) error {
		id = d.PutSkill(Skill{Name: "deploy-check", Kind: SkillKind, Body: "Check the deploy.\n\tThen report.", AgentAuthored: true}).ID
		return nil
	})
	got := d.Skill(id)
	if got == nil || got.Revision != 1 || got.Body != "Check the deploy.\n\tThen report." || !got.AgentAuthored {
		t.Fatalf("saved skill = %+v", got)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "skills.json")); err != nil {
		t.Fatalf("skills.json not written: %v", err)
	}
	d = edit(t, s, func(d *Data) error { return d.PurgeSkill(id) })
	if d.Skill(id) != nil || len(d.Skills.Tombstones) != 1 || d.Skills.Tombstones[0].ID != id {
		t.Fatalf("after purge: items %+v, tombstones %+v", d.Skills.Items, d.Skills.Tombstones)
	}
}

// Two skills can share a name after a sync merge; /name always picks the
// same one, the lowest ID.
func TestSkillNamedIsDeterministic(t *testing.T) {
	d := &Data{Skills: Collection[Skill]{Items: []Skill{
		{Meta: Meta{ID: "b"}, Name: "explain", Kind: TemplateKind, Body: "B"},
		{Meta: Meta{ID: "a"}, Name: "explain", Kind: TemplateKind, Body: "A"},
		{Meta: Meta{ID: "c"}, Name: "other", Kind: SkillKind, Body: "C"},
	}}}
	if s := d.SkillNamed("explain"); s == nil || s.ID != "a" {
		t.Fatalf("SkillNamed = %+v", s)
	}
	if d.SkillNamed("missing") != nil {
		t.Fatal("found a skill that does not exist")
	}
}

func TestCheckSkill(t *testing.T) {
	ok := Skill{Name: "deploy-check", Kind: SkillKind, Body: "line one\nline two\ttab"}
	if err := CheckSkill(ok); err != nil {
		t.Fatalf("a good skill was refused: %v", err)
	}
	bad := map[string]Skill{
		"empty name":      {Name: "", Kind: SkillKind, Body: "x"},
		"slash in name":   {Name: "a/b", Kind: SkillKind, Body: "x"},
		"space in name":   {Name: "a b", Kind: SkillKind, Body: "x"},
		"capital in name": {Name: "Deploy", Kind: SkillKind, Body: "x"},
		"long name":       {Name: strings.Repeat("a", 65), Kind: SkillKind, Body: "x"},
		"unknown kind":    {Name: "a", Kind: "macro", Body: "x"},
		"empty body":      {Name: "a", Kind: SkillKind, Body: " \n "},
		"escape in body":  {Name: "a", Kind: SkillKind, Body: "red \x1b[31m"},
		"CR in body":      {Name: "a", Kind: SkillKind, Body: "a\rb"},
		"bidi in body":    {Name: "a", Kind: SkillKind, Body: "a‮b"},
		"zero width":      {Name: "a", Kind: SkillKind, Body: "a​b"},
		"huge body":       {Name: "a", Kind: SkillKind, Body: strings.Repeat("x", 64<<10+1)},
		"control in desc": {Name: "a", Kind: SkillKind, Body: "x", Description: "a\nb"},
	}
	for what, s := range bad {
		if CheckSkill(s) == nil {
			t.Errorf("%s: accepted", what)
		}
	}
}
