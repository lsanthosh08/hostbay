package store

import (
	"os"
	"path/filepath"
	"testing"
)

// Each group is one item with the group's name as its ID, so the same group
// on two computers is the same item for sync (KTD37).
func TestSettingsGroupsPersistByName(t *testing.T) {
	s := openTestStore(t)
	edit(t, s, func(d *Data) error {
		d.PutSettings(SettingsGroup{Meta: Meta{ID: GroupAppearance}, Theme: "light", FontSize: 15})
		return nil
	})
	d := edit(t, s, func(d *Data) error {
		g := *d.SettingsGroup(GroupAppearance)
		g.FontSize = 16
		d.PutSettings(g)
		return nil
	})
	if len(d.Settings.Items) != 1 {
		t.Fatalf("settings = %+v, want one appearance group", d.Settings.Items)
	}
	g := d.SettingsGroup(GroupAppearance)
	if g.Theme != "light" || g.FontSize != 16 || g.Revision != 2 || g.DeviceID != s.DeviceID() {
		t.Fatalf("appearance = %+v", g)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "settings.json")); err != nil {
		t.Fatalf("settings.json not written: %v", err)
	}
}

// No ai group, an empty mode and a mode this build does not know all read
// as confirm all, the strictest (AE15).
func TestAIModeDefaultsToConfirmAll(t *testing.T) {
	d := &Data{}
	if m := d.AIMode(); m != AIConfirmAll {
		t.Fatalf("no ai group: %q", m)
	}
	for _, mode := range []string{"", "bypass", "anything"} {
		d.Settings.Items = []SettingsGroup{{Meta: Meta{ID: GroupAI}, ExecutionMode: mode}}
		if m := d.AIMode(); m != AIConfirmAll {
			t.Fatalf("mode %q reads as %q", mode, m)
		}
	}
	d.Settings.Items[0].ExecutionMode = AIDangerousOnly
	if m := d.AIMode(); m != AIDangerousOnly {
		t.Fatalf("dangerous-only reads as %q", m)
	}
}

func TestLocalSettingsPersistOutsideData(t *testing.T) {
	s := openTestStore(t)
	on := true
	if err := s.EditLocal(func(l *Local) error { l.AISuggestions = &on; return nil }); err != nil {
		t.Fatal(err)
	}
	l, err := s.Local()
	if err != nil || l.AISuggestions == nil || !*l.AISuggestions {
		t.Fatalf("local = %+v, %v", l, err)
	}
	d, err := s.Data()
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range d.docs() {
		if doc.name == localName {
			t.Fatal("local.json is one of the synced and backed-up documents")
		}
	}
}
