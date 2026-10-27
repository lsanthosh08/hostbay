package app

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"hostbay/internal/store"
)

func newSettingsService(t *testing.T) (*SettingsService, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "Hostbay")
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return NewSettingsService(st), dir
}

func TestPageSetsAppearanceAndLanguage(t *testing.T) {
	s, _ := newSettingsService(t)
	if err := s.Set(store.SettingsGroup{Meta: store.Meta{ID: store.GroupAppearance}, Theme: "light", FontSize: 15}); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(store.SettingsGroup{Meta: store.Meta{ID: store.GroupLanguage}, Language: "de"}); err != nil {
		t.Fatal(err)
	}
	ap, err := s.Get(store.GroupAppearance)
	if err != nil || ap.Theme != "light" || ap.FontSize != 15 {
		t.Fatalf("appearance = %+v, %v", ap, err)
	}
	if lang, err := s.Get(store.GroupLanguage); err != nil || lang.Language != "de" {
		t.Fatalf("language = %+v, %v", lang, err)
	}
	for _, bad := range []store.SettingsGroup{
		{Meta: store.Meta{ID: store.GroupAppearance}, Theme: "evil"},
		{Meta: store.Meta{ID: store.GroupAppearance}, FontSize: 400},
		{Meta: store.Meta{ID: store.GroupLanguage}, Language: "../x"},
		{Meta: store.Meta{ID: "nope"}},
	} {
		if err := s.Set(bad); err == nil {
			t.Errorf("Set(%+v) accepted", bad)
		}
	}
}

// Go enforces the AI execution mode; the page only shows it (KTD37).
func TestPageCannotSetTheAIMode(t *testing.T) {
	s, _ := newSettingsService(t)
	if err := s.Set(store.SettingsGroup{Meta: store.Meta{ID: store.GroupAI}, ExecutionMode: store.AIDangerousOnly}); err == nil {
		t.Fatal("the bridge set the ai group")
	}
	// Another group's item cannot carry the mode in either.
	if err := s.Set(store.SettingsGroup{Meta: store.Meta{ID: store.GroupAppearance}, Theme: "dark", ExecutionMode: store.AIDangerousOnly}); err != nil {
		t.Fatal(err)
	}
	d, err := s.st.Data()
	if err != nil {
		t.Fatal(err)
	}
	if d.AIMode() != store.AIConfirmAll || d.SettingsGroup(store.GroupAppearance).ExecutionMode != "" {
		t.Fatalf("mode = %q, settings %+v", d.AIMode(), d.Settings.Items)
	}
}

// The page's local setter takes AI suggestions and per-host logging only.
// Allowlist entries, MCP and the log folder have their own Go paths.
func TestLocalSetterRefusesSecuritySettings(t *testing.T) {
	s, dir := newSettingsService(t)
	if err := s.SetLocal(map[string]any{"aiSuggestions": true, "hostLogging": map[string]any{"h1": true}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "local.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, patch := range []map[string]any{
		{"allowlist": map[string]any{"h1": []string{"rm -rf /"}}},
		{"mcpEnabled": true},
		{"logFolder": "/tmp/elsewhere"},
		{"aiSuggestions": false, "mcpEnabled": true},
		{"aiSuggestions": "yes"},
	} {
		if err := s.SetLocal(patch); err == nil {
			t.Errorf("SetLocal(%v) accepted", patch)
		}
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatalf("local.json changed:\n%s", after)
	}
	l, err := s.GetLocal()
	if err != nil || l.AISuggestions == nil || !*l.AISuggestions || !l.HostLogging["h1"] {
		t.Fatalf("local = %+v, %v", l, err)
	}
}

// The page sends what it kept in localStorage once; once settings exist,
// it changes nothing.
func TestLegacyImportRunsOnce(t *testing.T) {
	s, _ := newSettingsService(t)
	if err := s.ImportLegacy("light", 15); err != nil {
		t.Fatal(err)
	}
	if err := s.ImportLegacy("dark", 12); err != nil {
		t.Fatal(err)
	}
	ap, err := s.Get(store.GroupAppearance)
	if err != nil || ap.Theme != "light" || ap.FontSize != 15 || ap.Revision != 1 {
		t.Fatalf("appearance = %+v, %v", ap, err)
	}
}

// A fresh install has nothing to hand over; the theme the page writes to
// localStorage on its first run is not legacy, and must not become a
// setting that wins another computer's choice on the first sync.
func TestLegacyImportWithNothingStillCountsAsDone(t *testing.T) {
	s, _ := newSettingsService(t)
	if err := s.ImportLegacy("", 0); err != nil {
		t.Fatal(err)
	}
	if err := s.ImportLegacy("dark", 0); err != nil {
		t.Fatal(err)
	}
	if ap, err := s.Get(store.GroupAppearance); err != nil || ap.Revision != 0 {
		t.Fatalf("appearance = %+v, %v", ap, err)
	}
}

// The page only asks for a move: Go picks the folder in its own dialog, so
// the page cannot aim the data folder anywhere, and a cancelled dialog
// changes nothing.
func TestDataFolderMovePicksInGoAndRestarts(t *testing.T) {
	s, dir := newSettingsService(t)
	restarts := 0
	s.restart = func() { restarts++ }
	s.pickFolder = func() (string, error) { return "", nil }
	if moved, err := s.DataFolderMove(); moved || err != nil || restarts != 0 {
		t.Fatalf("cancelled pick: moved %v, err %v, restarts %d", moved, err, restarts)
	}
	notEmpty := t.TempDir()
	os.WriteFile(filepath.Join(notEmpty, "x"), nil, 0o600)
	s.pickFolder = func() (string, error) { return notEmpty, nil }
	if moved, err := s.DataFolderMove(); moved || err == nil || restarts != 0 {
		t.Fatalf("refused folder: moved %v, err %v, restarts %d", moved, err, restarts)
	}
	to := filepath.Join(t.TempDir(), "data")
	os.Mkdir(to, 0o700)
	s.pickFolder = func() (string, error) { return to, nil }
	if moved, err := s.DataFolderMove(); !moved || err != nil || restarts != 1 {
		t.Fatalf("good folder: moved %v, err %v, restarts %d", moved, err, restarts)
	}
	if f, err := s.DataFolder(); err != nil || f.Path != dir || !f.Default || f.Pending != to {
		t.Fatalf("DataFolder() = %+v, %v; want %s, default, pending %s", f, err, dir, to)
	}
}

func palette(c string) map[string]string {
	p := map[string]string{}
	for _, k := range paletteKeys {
		p[k] = c
	}
	return p
}

// Terminal themes, custom themes and the image opacity (KTD41) save with
// the appearance group, and sync carries them, so Go checks each one.
func TestPageSetsTerminalAppearance(t *testing.T) {
	s, _ := newSettingsService(t)
	custom := store.TermTheme{ID: "custom-1", Name: "Mine", Type: "dark", Colors: palette("#112233")}
	in := store.SettingsGroup{Meta: store.Meta{ID: store.GroupAppearance}, Theme: "deep-blue", TerminalTheme: "custom-1",
		CustomThemes: []store.TermTheme{custom}, BackgroundOpacity: 40}
	if err := s.Set(in); err != nil {
		t.Fatal(err)
	}
	ap, err := s.Get(store.GroupAppearance)
	if err != nil || ap.Theme != "deep-blue" || ap.TerminalTheme != "custom-1" || ap.BackgroundOpacity != 40 ||
		len(ap.CustomThemes) != 1 || ap.CustomThemes[0].Colors["brightWhite"] != "#112233" {
		t.Fatalf("appearance = %+v, %v", ap, err)
	}
	// A newer Hostbay may sync a palette with a colour this build lacks; it
	// must not stop every appearance save here.
	newer := palette("#112233")
	newer["cursorText"] = "#445566"
	if err := s.Set(store.SettingsGroup{Meta: store.Meta{ID: store.GroupAppearance},
		CustomThemes: []store.TermTheme{{ID: "custom-9", Name: "Newer", Type: "light", Colors: newer}}}); err != nil {
		t.Fatalf("a palette with an extra colour: %v", err)
	}
	if err := s.Set(in); err != nil {
		t.Fatal(err)
	}
	ap, _ = s.Get(store.GroupAppearance)
	rev := ap.Revision
	if err := s.Set(in); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.Get(store.GroupAppearance); again.Revision != rev {
		t.Error("an unchanged group got a new revision")
	}

	short := palette("#000000")
	delete(short, "cursor")
	bad := palette("#000000")
	bad["red"] = "red;background:url(x)"
	for name, g := range map[string]store.SettingsGroup{
		"theme id":      {TerminalTheme: "../x"},
		"opacity":       {BackgroundOpacity: 150},
		"missing color": {CustomThemes: []store.TermTheme{{ID: "custom-2", Name: "x", Type: "dark", Colors: short}}},
		"bad color":     {CustomThemes: []store.TermTheme{{ID: "custom-2", Name: "x", Type: "dark", Colors: bad}}},
		"no name":       {CustomThemes: []store.TermTheme{{ID: "custom-2", Type: "dark", Colors: palette("#000000")}}},
		"bad id":        {CustomThemes: []store.TermTheme{{ID: "dracula", Name: "x", Type: "dark", Colors: palette("#000000")}}},
	} {
		g.ID = store.GroupAppearance
		if err := s.Set(g); err == nil {
			t.Errorf("%s: Set accepted %+v", name, g)
		}
	}
}

// Keybinding overrides map an action ID to a binding per platform (KTD40);
// "" unbinds the action there, and an absent platform keeps its default.
func TestPageSetsKeyBindings(t *testing.T) {
	s, _ := newSettingsService(t)
	keys := map[string]store.KeyBinding{"palette.open": {Mac: new("Meta+Alt+KeyK")}, "tab.next": {Other: new("")}}
	if err := s.Set(store.SettingsGroup{Meta: store.Meta{ID: store.GroupKeyboard}, Keys: keys, Theme: "light"}); err != nil {
		t.Fatal(err)
	}
	kb, err := s.Get(store.GroupKeyboard)
	if err != nil || store.Deref(kb.Keys["palette.open"].Mac) != "Meta+Alt+KeyK" || kb.Keys["palette.open"].Other != nil ||
		kb.Keys["tab.next"].Other == nil || kb.Theme != "" || kb.Revision != 1 {
		t.Fatalf("keyboard = %+v, %v", kb, err)
	}
	// The same overrides again change nothing, so the group keeps its revision.
	if err := s.Set(store.SettingsGroup{Meta: store.Meta{ID: store.GroupKeyboard}, Keys: keys}); err != nil {
		t.Fatal(err)
	}
	if kb, _ := s.Get(store.GroupKeyboard); kb.Revision != 1 {
		t.Fatalf("unchanged keys raised the revision to %d", kb.Revision)
	}
	for _, bad := range []map[string]store.KeyBinding{
		{"../x": {Mac: new("Meta+K")}},
		{"palette.open": {Mac: new("Meta+<script>")}},
		{"palette.open": {Other: new("Hyper+K")}},
	} {
		if err := s.Set(store.SettingsGroup{Meta: store.Meta{ID: store.GroupKeyboard}, Keys: bad}); err == nil {
			t.Errorf("Set(%+v) accepted", bad)
		}
	}
}
