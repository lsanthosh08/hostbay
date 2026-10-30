package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"sync/atomic"

	"github.com/wailsapp/wails/v3/pkg/application"

	"hostbay/internal/store"
)

// SettingsService reads and writes settings for the page (KTD37). The page
// may change appearance, language and keyboard shortcuts; the ai group changes only through
// its own verified path (U23), and of the device-local settings only AI
// suggestions, per-host logging and keeping running in the menu bar.
type SettingsService struct {
	st         *store.Store
	pickFolder func() (string, error)
	restart    func()
}

func NewSettingsService(st *store.Store) *SettingsService {
	return &SettingsService{st: st, restart: Relaunch, pickFolder: func() (string, error) {
		return PickDataFolder("Choose an empty folder for Hostbay's data")
	}}
}

// PickDataFolder asks for a folder in Go's dialog; "" means cancelled.
func PickDataFolder(title string) (string, error) {
	return application.Get().Dialog.OpenFile().CanChooseFiles(false).CanChooseDirectories(true).
		CanCreateDirectories(true).SetTitle(title).PromptForSingleSelection()
}

var relaunch atomic.Bool

// Relaunch quits Hostbay and has main start it again once the store lock
// and the single-instance lock are released (RelaunchRequested).
func Relaunch() {
	relaunch.Store(true)
	if a := application.Get(); a != nil {
		go a.Quit() // after the calling binding has answered
	}
}

// RelaunchRequested reports whether quitting should start Hostbay again.
func RelaunchRequested() bool { return relaunch.Load() }

// DataFolder is where Hostbay keeps its data (R72), for Settings > General.
type DataFolder struct {
	Path    string `json:"path"`
	Default bool   `json:"default"`           // the folder Hostbay starts out with
	Pending string `json:"pending,omitempty"` // a move that runs at the next start
	Notice  string `json:"notice,omitempty"`  // why a move failed at this start
}

func (s *SettingsService) DataFolder() (DataFolder, error) {
	pending, err := s.st.PendingMove()
	return DataFolder{Path: s.st.Dir(), Default: s.st.Dir() == s.st.Home(), Pending: pending, Notice: s.st.Notice()}, err
}

// DataFolderMove asks, in Go's dialog, for a folder to move Hostbay's data
// to, checks it and restarts Hostbay, which copies the data at start before
// anything opens it (KTD42). A cancelled dialog changes nothing and returns
// false.
func (s *SettingsService) DataFolderMove() (bool, error) {
	dir, err := s.pickFolder()
	if err != nil || dir == "" {
		return false, err
	}
	if err := s.st.RequestMove(dir); err != nil {
		return false, err
	}
	s.restart()
	return true, nil
}

// uiThemes are the themes in src/ui/theme.tsx.
var uiThemes = []string{"dark", "light", "deep-blue"}

// A language tag such as "en", "zh-Hans" or "pt-BR".
var languageTag = regexp.MustCompile(`^[a-z]{2,3}(-[A-Za-z0-9]{2,8})?$`)

var groups = []string{store.GroupAppearance, store.GroupLanguage, store.GroupAI, store.GroupKeyboard}

// An action ID from src/actions/registry.ts, and a binding as
// src/actions/shortcuts.ts writes it: modifiers, then a letter or a
// KeyboardEvent code.
var (
	actionID   = regexp.MustCompile(`^[a-z][A-Za-z0-9.]{0,63}$`)
	keyBinding = regexp.MustCompile(`^((Meta|Ctrl|Alt|Shift)\+){0,4}[A-Za-z][A-Za-z0-9]{0,23}$`)
)

// Get returns the group named id; one never saved comes back empty.
func (s *SettingsService) Get(id string) (store.SettingsGroup, error) {
	if !slices.Contains(groups, id) {
		return store.SettingsGroup{}, fmt.Errorf("unknown settings group %q", id)
	}
	d, err := s.st.Data()
	if err != nil {
		return store.SettingsGroup{}, err
	}
	if g := d.SettingsGroup(id); g != nil {
		return *g, nil
	}
	return store.SettingsGroup{Meta: store.Meta{ID: id}}, nil
}

// Set saves the page's change to one group. Only that group's own fields
// are taken, and Meta never comes from the page.
func (s *SettingsService) Set(in store.SettingsGroup) error {
	switch in.ID {
	case store.GroupAppearance:
		if err := checkAppearance(in.Theme, in.FontSize); err != nil {
			return err
		}
		if err := checkTerminalAppearance(in); err != nil {
			return err
		}
	case store.GroupLanguage:
		if in.Language != "" && !languageTag.MatchString(in.Language) {
			return fmt.Errorf("%q is not a language", in.Language)
		}
	case store.GroupAI:
		return errors.New("the AI execution mode cannot be changed here")
	case store.GroupKeyboard:
		if err := checkKeys(in.Keys); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown settings group %q", in.ID)
	}
	return s.st.Edit(func(d *store.Data) error {
		g := store.SettingsGroup{Meta: store.Meta{ID: in.ID}}
		cur := d.SettingsGroup(in.ID)
		if cur != nil {
			g = *cur
		}
		switch in.ID {
		case store.GroupAppearance:
			g.Theme, g.FontSize = in.Theme, in.FontSize
			g.TerminalTheme, g.CustomThemes, g.BackgroundOpacity = in.TerminalTheme, in.CustomThemes, in.BackgroundOpacity
		case store.GroupLanguage:
			g.Language = in.Language
		case store.GroupKeyboard:
			g.Keys = in.Keys
			if len(g.Keys) == 0 {
				g.Keys = nil
			}
		}
		if cur == nil || !reflect.DeepEqual(*cur, g) { // an unchanged group keeps its revision
			d.PutSettings(g)
		}
		return nil
	})
}

func checkKeys(keys map[string]store.KeyBinding) error {
	if len(keys) > 500 {
		return errors.New("too many shortcuts")
	}
	for id, b := range keys {
		if !actionID.MatchString(id) {
			return fmt.Errorf("%q is not an action", id)
		}
		for _, k := range []*string{b.Mac, b.Other} {
			if k != nil && *k != "" && !keyBinding.MatchString(*k) {
				return fmt.Errorf("%q is not a shortcut", *k)
			}
		}
	}
	return nil
}

func checkAppearance(theme string, fontSize int) error {
	if theme != "" && !slices.Contains(uiThemes, theme) {
		return fmt.Errorf("%q is not a theme", theme)
	}
	if fontSize != 0 && (fontSize < 8 || fontSize > 40) {
		return fmt.Errorf("font size %d is not between 8 and 40", fontSize)
	}
	return nil
}

// paletteKeys are a terminal palette's colours, as src/theme/terminalThemes.ts
// names them.
var paletteKeys = []string{"background", "foreground", "cursor", "selection",
	"black", "red", "green", "yellow", "blue", "magenta", "cyan", "white",
	"brightBlack", "brightRed", "brightGreen", "brightYellow", "brightBlue", "brightMagenta", "brightCyan", "brightWhite"}

var (
	termThemeID   = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)
	customThemeID = regexp.MustCompile(`^custom-[A-Za-z0-9-]{1,40}$`)
	hexColor      = regexp.MustCompile(`^#[0-9a-fA-F]{6}([0-9a-fA-F]{2})?$`)
)

// checkTerminalAppearance checks the terminal theme, custom palettes and
// image opacity. Colours are hex only, since they reach xterm and CSS.
func checkTerminalAppearance(g store.SettingsGroup) error {
	if g.TerminalTheme != "" && !termThemeID.MatchString(g.TerminalTheme) && !customThemeID.MatchString(g.TerminalTheme) {
		return fmt.Errorf("%q is not a terminal theme", g.TerminalTheme)
	}
	if g.BackgroundOpacity < 0 || g.BackgroundOpacity > 100 {
		return fmt.Errorf("opacity %d is not between 0 and 100", g.BackgroundOpacity)
	}
	if len(g.CustomThemes) > 100 {
		return errors.New("too many custom themes")
	}
	for _, c := range g.CustomThemes {
		if !customThemeID.MatchString(c.ID) || c.Name == "" || len(c.Name) > 100 || (c.Type != "dark" && c.Type != "light") {
			return fmt.Errorf("custom theme %q is not valid", c.Name)
		}
		for _, k := range paletteKeys {
			if _, ok := c.Colors[k]; !ok {
				return fmt.Errorf("custom theme %q needs all %d colours", c.Name, len(paletteKeys))
			}
		}
		// A newer Hostbay may add a colour, which sync carries here.
		if len(c.Colors) > 64 {
			return fmt.Errorf("custom theme %q has too many colours", c.Name)
		}
		for k, v := range c.Colors {
			if !hexColor.MatchString(v) {
				return fmt.Errorf("custom theme %q: %s is not a colour", c.Name, k)
			}
		}
	}
	return nil
}

// ImportLegacy saves the theme and font size the page kept in localStorage
// before settings moved into Go. It runs once per store, even with nothing
// to hand over, since the page writes its theme to localStorage on every
// start; it also does nothing once any setting exists, here or synced from
// another computer, and skips a value that is not valid.
func (s *SettingsService) ImportLegacy(theme string, fontSize int) error {
	if s.st.SettingsImported() {
		return nil
	}
	err := s.st.Edit(func(d *store.Data) error {
		if len(d.Settings.Items) > 0 || len(d.Settings.Tombstones) > 0 {
			return nil
		}
		g := store.SettingsGroup{Meta: store.Meta{ID: store.GroupAppearance}}
		if checkAppearance(theme, 0) == nil {
			g.Theme = theme
		}
		if checkAppearance("", fontSize) == nil {
			g.FontSize = fontSize
		}
		if g.Theme != "" || g.FontSize != 0 {
			d.PutSettings(g)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return s.st.MarkSettingsImported()
}

// GetLocal returns this computer's own settings, except sealed Tailscale
// node keys, which stay in Go.
func (s *SettingsService) GetLocal() (store.Local, error) {
	l, err := s.st.Local()
	l.VPNNodeStates = nil
	return l, err
}

// SetLocal changes AI suggestions, per-host logging and keeping running in
// the menu bar. Any other key is
// refused and nothing is saved: allowlist entries, MCP and the log folder
// change only through their own Go paths.
func (s *SettingsService) SetLocal(patch map[string]any) error {
	raw, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	var p struct {
		AISuggestions *bool           `json:"aiSuggestions"`
		HostLogging   map[string]bool `json:"hostLogging"`
		KeepRunning   *bool           `json:"keepRunning"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return fmt.Errorf("this setting cannot be changed here: %w", err)
	}
	return s.st.EditLocal(func(l *store.Local) error {
		if p.AISuggestions != nil {
			l.AISuggestions = p.AISuggestions
		}
		if p.KeepRunning != nil {
			l.KeepRunning = *p.KeepRunning
		}
		for id, on := range p.HostLogging {
			if !on {
				delete(l.HostLogging, id)
				continue
			}
			if l.HostLogging == nil {
				l.HostLogging = map[string]bool{}
			}
			l.HostLogging[id] = true
		}
		return nil
	})
}
