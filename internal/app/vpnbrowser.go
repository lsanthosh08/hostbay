package app

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"

	"hostbay/internal/apperr"
	"hostbay/internal/importer"
	"hostbay/internal/store"
	"hostbay/internal/vpn"
	"hostbay/internal/vpn/browser"
)

// Browser routing (R33, KTD11): a Chromium-family browser profile mapped
// to an app-scoped VPN profile launches through that VPN's no-login
// loopback SOCKS listener. Mappings live in store.Local, on this computer.

var (
	// installedBrowsers and startBrowser are swapped out in tests, so no
	// test finds or starts a real browser.
	installedBrowsers = browser.Installed
	startBrowser      = func(c *exec.Cmd) error {
		if err := c.Start(); err != nil {
			return fmt.Errorf("cannot start the browser: %w", err)
		}
		go c.Wait() // reap it whenever it quits
		return nil
	}
)

// browserIDs are the browsers a mapping can name.
var browserIDs = []string{"chrome", "chromium", "edge", "brave"}

// InstalledBrowser is a browser on this computer with its profiles, for the
// mapping editor. Its path never leaves Go.
type InstalledBrowser struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Profiles []browser.Profile `json:"profiles"`
}

// InstalledBrowsers lists the Chromium-family browsers on this computer and
// their profiles; one whose profiles cannot be read lists none.
func (s *VPNService) InstalledBrowsers() []InstalledBrowser {
	out := []InstalledBrowser{}
	for _, b := range installedBrowsers() {
		p, err := browser.Profiles(b)
		if err != nil {
			p = []browser.Profile{}
		}
		out = append(out, InstalledBrowser{ID: b.ID, Name: b.Name, Profiles: p})
	}
	return out
}

// BrowserMappings lists the saved browser profile mappings.
func (s *VPNService) BrowserMappings() ([]store.BrowserMapping, error) {
	l, err := s.st.Local()
	if err != nil {
		return nil, err
	}
	return append([]store.BrowserMapping{}, l.BrowserMappings...), nil
}

// SaveBrowserMapping saves m, replacing the browser profile's mapping if it
// has one. Any saved VPN profile is accepted; launch says when one has no
// SOCKS endpoint (AE6).
func (s *VPNService) SaveBrowserMapping(m store.BrowserMapping) error {
	m.Profile = strings.TrimSpace(m.Profile)
	if !slices.Contains(browserIDs, m.Browser) {
		return fmt.Errorf("unknown browser %q", m.Browser)
	}
	if m.Profile == "" {
		return errors.New("choose a browser profile")
	}
	d, err := s.st.Data()
	if err != nil {
		return err
	}
	if d.VPN(m.VPN) == nil {
		return errors.New("choose a saved VPN profile")
	}
	return s.st.EditLocal(func(l *store.Local) error {
		l.BrowserMappings = putMapping(l.BrowserMappings, m)
		return nil
	})
}

func putMapping(all []store.BrowserMapping, m store.BrowserMapping) []store.BrowserMapping {
	if i := slices.IndexFunc(all, sameProfile(m.Browser, m.Profile)); i >= 0 {
		all[i] = m
		return all
	}
	return append(all, m)
}

func sameProfile(browserID, profile string) func(store.BrowserMapping) bool {
	return func(m store.BrowserMapping) bool { return m.Browser == browserID && m.Profile == profile }
}

// DeleteBrowserMapping removes the browser profile's mapping.
func (s *VPNService) DeleteBrowserMapping(browserID, profile string) error {
	return s.st.EditLocal(func(l *store.Local) error {
		l.BrowserMappings = slices.DeleteFunc(l.BrowserMappings, sameProfile(browserID, profile))
		return nil
	})
}

// LaunchBrowser starts the mapped browser profile through its VPN,
// connecting the VPN first if it is down. It refuses a VPN with no SOCKS
// endpoint (AE6) and a browser that is already running, which would open
// the profile itself without the proxy (KTD11).
func (s *VPNService) LaunchBrowser(ctx context.Context, browserID, profile string) error {
	l, err := s.st.Local()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(l.BrowserMappings, sameProfile(browserID, profile))
	if i < 0 {
		return errors.New("this browser profile is not mapped to a VPN")
	}
	m := l.BrowserMappings[i]
	d, err := s.st.Data()
	if err != nil {
		return err
	}
	v := d.VPN(m.VPN)
	if v == nil {
		return errors.New("the VPN profile this browser profile maps to was deleted; choose another")
	}
	if v.Tier != store.VPNTierApp {
		return apperr.New(apperr.VPNFailed, fmt.Sprintf(
			"VPN %s has no SOCKS endpoint: it is not app-scoped, so Hostbay cannot send a browser through it. Map the profile to an app-scoped VPN.", v.Name)).
			With("vpnId", v.ID).With("reason", "no-socks")
	}
	all := installedBrowsers()
	bi := slices.IndexFunc(all, func(b browser.Browser) bool { return b.ID == m.Browser })
	if bi < 0 {
		return fmt.Errorf("browser %q is not installed on this computer", m.Browser)
	}
	b := all[bi]
	profiles, err := browser.Profiles(b)
	if err != nil {
		return err
	}
	// An imported mapping may name the profile as the browser shows it.
	pi := slices.IndexFunc(profiles, func(p browser.Profile) bool { return p.Dir == m.Profile })
	if pi < 0 {
		pi = slices.IndexFunc(profiles, func(p browser.Profile) bool { return p.Name == m.Profile })
	}
	if pi < 0 {
		return fmt.Errorf("%s has no profile %q", b.Name, m.Profile)
	}
	running := fmt.Errorf("%s is already running. Quit it, then launch again, so the profile opens through %s.", b.Name, v.Name)
	if browser.Running(b) {
		return running
	}
	if _, err := s.ConnectVPN(ctx, v.ID); err != nil {
		return err
	}
	var h *vpn.Helper
	s.mu.Lock()
	if r := s.runs[v.ID]; r != nil && r.att != nil {
		h = r.att.h
	}
	s.mu.Unlock()
	if h == nil {
		return apperr.New(apperr.VPNDown, "This VPN profile is not connected").With("vpnId", v.ID)
	}
	port, err := h.BrowserPort(ctx)
	if err != nil {
		return apperr.New(apperr.VPNFailed, "VPN "+v.Name+": "+err.Error()).With("vpnId", v.ID)
	}
	// Connecting can wait on a sign-in, long enough to open the browser meanwhile.
	if browser.Running(b) {
		return running
	}
	// A VPN that carries only some networks gets only those through the proxy.
	s.mu.Lock()
	live := s.states[v.ID].Routes
	s.mu.Unlock()
	return startBrowser(browser.Command(b, profiles[pi].Dir, port, slices.Concat(v.Routes, live), v.DNSDomains))
}

// saveImportedBrowsers saves an import's browser mappings, as Chrome's,
// for the VPNs it added, matched by the name the file gave them; it
// returns those saved. The rest name VPNs the user skipped.
func saveImportedBrowsers(st *store.Store, rep importer.Report) []importer.BrowserMapping {
	saved := []importer.BrowserMapping{}
	if len(rep.Browsers) == 0 {
		return saved
	}
	err := st.EditLocal(func(l *store.Local) error {
		for _, b := range rep.Browsers {
			if i := slices.IndexFunc(rep.VPNs, func(v importer.VPNImport) bool { return v.Name == b.VPN }); i >= 0 {
				l.BrowserMappings = putMapping(l.BrowserMappings, store.BrowserMapping{Browser: "chrome", Profile: b.Profile, VPN: rep.VPNs[i].ID})
				saved = append(saved, b)
			}
		}
		return nil
	})
	if err != nil {
		return []importer.BrowserMapping{}
	}
	return saved
}
