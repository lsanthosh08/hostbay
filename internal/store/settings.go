package store

import (
	"slices"
	"sync"
)

// Settings groups (KTD37). Each is one item of the settings collection,
// with its name as its ID, so sync merges each group on its own and the
// same group on two computers is the same item.
const (
	GroupAppearance = "appearance"
	GroupLanguage   = "language"
	GroupAI         = "ai"
	GroupKeyboard   = "keyboard"
)

// SettingsGroup is one synced settings group. Every group's fields sit at
// the top level, so sync keeps a field a newer Hostbay added; each group
// uses only its own.
type SettingsGroup struct {
	Meta
	// appearance
	Theme    string `json:"theme,omitempty"`    // the UI theme
	FontSize int    `json:"fontSize,omitempty"` // terminal font size in px
	// The terminal palette: "" matches the UI theme; otherwise a built-in
	// theme in src/theme/terminalThemes.ts or a custom theme's ID (KTD41).
	TerminalTheme     string      `json:"terminalTheme,omitempty"`
	CustomThemes      []TermTheme `json:"customThemes,omitempty"`
	BackgroundOpacity int         `json:"backgroundOpacity,omitempty"` // the background image's opacity in percent; 0 is the default
	// language
	Language string `json:"language,omitempty"`
	// ai: Go reads and enforces it; the page only shows it.
	ExecutionMode string `json:"executionMode,omitempty"`
	// keyboard: action ID to its binding overrides (KTD40)
	Keys map[string]KeyBinding `json:"keys,omitempty"`
}

// KeyBinding overrides an action's default shortcut per platform. nil keeps
// the default there, and "" leaves the action unbound.
type KeyBinding struct {
	Mac   *string `json:"mac,omitempty"`
	Other *string `json:"other,omitempty"`
}

// TermTheme is a custom terminal palette: background, foreground, cursor,
// selection and the 16 ANSI colours, by xterm's names.
type TermTheme struct {
	ID     string            `json:"id"`
	Name   string            `json:"name"`
	Type   string            `json:"type"` // "dark" or "light"
	Colors map[string]string `json:"colors"`
}

// AI execution modes that sync carries, strictest first (KTD33). Bypass is
// device-local and never one of them.
const (
	AIConfirmAll     = "confirm-all"
	AIDangerousWrite = "dangerous-write"
	AIDangerousOnly  = "dangerous-only"
)

var aiModes = []string{AIConfirmAll, AIDangerousWrite, AIDangerousOnly}

// AIModeRank orders modes by how much runs without asking; confirm all is
// 0. "" is confirm all, and a mode this build does not know ranks weakest,
// so sync never applies it unconfirmed.
func AIModeRank(mode string) int {
	if mode == "" {
		return 0
	}
	if i := slices.Index(aiModes, mode); i >= 0 {
		return i
	}
	return len(aiModes)
}

// SettingsGroup returns the group named id, or nil.
func (d *Data) SettingsGroup(id string) *SettingsGroup {
	if i := find(d.Settings.Items, id); i >= 0 {
		return &d.Settings.Items[i]
	}
	return nil
}

// PutSettings saves g as a local change.
func (d *Data) PutSettings(g SettingsGroup) SettingsGroup { return put(d, &d.Settings, g) }

// AddSettings adds g, keeping any Meta it carries.
func (d *Data) AddSettings(g SettingsGroup) SettingsGroup { return add(d, &d.Settings, g) }

// AIMode is the execution mode in force: confirm all unless the ai group
// names a mode this build knows.
func (d *Data) AIMode() string {
	if g := d.SettingsGroup(GroupAI); g != nil && slices.Contains(aiModes, g.ExecutionMode) {
		return g.ExecutionMode
	}
	return AIConfirmAll
}

// localName is the device-local settings document. It is not in Data, so
// sync and backup never carry it (KTD37).
const localName = "local.json"

// Local is this computer's own settings.
type Local struct {
	AISuggestions *bool           `json:"aiSuggestions,omitempty"`
	HostLogging   map[string]bool `json:"hostLogging,omitempty"` // host ID: log its sessions
	// The terminal background image's file name in backgrounds/. Only
	// AppearanceService sets it, from a file picked in Go's dialog.
	Background string `json:"background,omitempty"`
	// LogFolder is where session logs go, a folder picked in a dialog Go
	// opened; "" is logs/ in the data folder.
	LogFolder string `json:"logFolder,omitempty"`
	// Allowlist is the AI agent's per-host entries (KTD33), keyed by host
	// ID. They change only through the AI service's verified path.
	Allowlist map[string][]AllowEntry `json:"allowlist,omitempty"`
	// MCP is the MCP server's settings (KTD35). It changes only through
	// MCPService, behind its dialog, verification or folder picker.
	MCP MCPSettings `json:"mcp,omitzero"`
	// VPNClients are external VPN profiles' client programs on this
	// computer, keyed by profile ID. A path from sync or a backup would let
	// another computer choose what Hostbay runs, so it never leaves here (KTD4).
	VPNClients map[string]string `json:"vpnClients,omitempty"`
	// VPNIfaces pin external VPN profiles to a tunnel interface on this
	// computer, keyed by profile ID, for when detection cannot tell two
	// VPNs apart (R24). Interface names mean nothing on another computer.
	VPNIfaces map[string]string `json:"vpnIfaces,omitempty"`
	// KeepRunning hides the window when it closes instead of quitting, so
	// Hostbay and its VPNs stay up in the menu bar (KTD10).
	KeepRunning bool `json:"keepRunning,omitempty"`
}

// MCPSettings is this computer's MCP server. Its tokens' hashes are in
// their own file (internal/mcp), so the page never reads them.
type MCPSettings struct {
	Enabled bool   `json:"enabled,omitempty"`
	Port    int    `json:"port,omitempty"`   // 0 is the default port
	Folder  string `json:"folder,omitempty"` // the only local folder transfers use, picked in Go's dialog
	// Allowlist is exact commands that run without a card, by token ID and
	// then host ID (KTD36).
	Allowlist map[string]map[string][]AllowEntry `json:"allowlist,omitempty"`
	// VPNAllow is the VPN profiles a token connects and disconnects without
	// a card (KTD6), added only in Settings.
	VPNAllow []VPNAllowEntry `json:"vpnAllow,omitempty"`
}

// VPNAllowEntry lets one token connect and disconnect one VPN profile
// without a card while the profile keeps the type, gateway and tier it had
// when the user allowed it.
type VPNAllowEntry struct {
	Token   string `json:"token"`
	Profile string `json:"profile"`
	Type    string `json:"type"`
	Gateway string `json:"gateway"`
	Tier    string `json:"tier"`
}

// Matches reports whether e lets token act on v without a card: never for a
// system-wide profile, and only while v keeps the type, gateway and tier e
// was added for, whatever changed them, sync included.
func (e VPNAllowEntry) Matches(token string, v *VPN) bool {
	return v != nil && v.Tier != VPNTierSystem && e.Token == token && e.Profile == v.ID &&
		e.Type == v.Type && e.Gateway == v.Gateway && e.Tier == v.Tier
}

// AllowEntry is one command the AI agent may run without asking on a
// host: exactly Argv, or with Prefix, Argv followed by plain arguments.
type AllowEntry struct {
	Argv   []string `json:"argv"`
	Prefix bool     `json:"prefix,omitempty"`
}

var localMu sync.Mutex

// Local loads the device-local settings.
func (s *Store) Local() (Local, error) {
	localMu.Lock()
	defer localMu.Unlock()
	return Load[Local](s, localName)
}

// EditLocal changes the device-local settings through fn. If fn fails
// nothing is saved.
func (s *Store) EditLocal(fn func(l *Local) error) error {
	localMu.Lock()
	defer localMu.Unlock()
	l, err := Load[Local](s, localName)
	if err != nil {
		return err
	}
	if err := fn(&l); err != nil {
		return err
	}
	return Save(s, localName, l)
}
