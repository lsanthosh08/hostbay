package app

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"hostbay/internal/importer"
	"hostbay/internal/importer/tunnelhub"
	"hostbay/internal/importer/vpnconf"
	"hostbay/internal/store"
)

// VPNDraft is one profile a VPN import read, for the review flow (R42).
// Like VPN, it never carries a secret; HasSecret says the file held some.
type VPNDraft struct {
	Profile VPN `json:"profile"`
	// Ignored lists what the file held that was dropped, such as a script
	// hook (R43).
	Ignored []string `json:"ignored"`
	// AskPassword: the file signs in with a password it does not hold, so
	// it is asked for on first connect (R44).
	AskPassword bool `json:"askPassword"`
}

// VPNImportPreview is what a VPN configuration file holds, before anything
// is saved. Token names the parsed file for CommitVPNImport.
type VPNImportPreview struct {
	Token   string             `json:"token"`
	Drafts  []VPNDraft         `json:"drafts"`
	Skipped []importer.Skipped `json:"skipped"`
	Notes   []string           `json:"notes"`
}

// vpnImports holds each previewed file, secrets included, until it is
// committed; the secrets never go to the UI.
// One nobody commits or cancels is dropped after vpnImportTTL.
var vpnImports = struct {
	sync.Mutex
	m map[string]pendingImport
}{m: map[string]pendingImport{}}

type pendingImport struct {
	res importer.Result
	at  time.Time
}

const vpnImportTTL = 10 * time.Minute

// sweepVPNImports drops the previews that outlived vpnImportTTL; the caller
// holds vpnImports.
func sweepVPNImports() {
	for t, p := range vpnImports.m {
		if time.Since(p.at) > vpnImportTTL {
			delete(vpnImports.m, t)
		}
	}
}

// CancelVPNImport forgets a previewed file, and the secrets read from it,
// when the user closes the review without saving.
func (s *HostService) CancelVPNImport(token string) {
	vpnImports.Lock()
	delete(vpnImports.m, token)
	vpnImports.Unlock()
}

// PreviewVPNImport reads a VPN configuration file or bundle folder (R38),
// or a TunnelHub vpn-config.json (R41), and returns its profiles for review
// without saving anything.
func (s *HostService) PreviewVPNImport(path string) (VPNImportPreview, error) {
	parse := vpnconf.Parse
	if strings.EqualFold(filepath.Ext(path), ".json") {
		parse = tunnelhub.Parse
	}
	res, err := parse(path, "")
	if err != nil {
		return VPNImportPreview{}, err
	}
	pre := VPNImportPreview{Token: store.NewID(), Drafts: []VPNDraft{}, Skipped: res.Skipped, Notes: res.Notes}
	for i, p := range res.Bundle.VPNs {
		pre.Drafts = append(pre.Drafts, VPNDraft{Profile: vpnToUI(p), Ignored: res.VPNs[i].Ignored, AskPassword: res.VPNs[i].AskPassword})
	}
	vpnImports.Lock()
	sweepVPNImports()
	vpnImports.m[pre.Token] = pendingImport{res, time.Now()}
	vpnImports.Unlock()
	return pre, nil
}

// CommitVPNImport saves the drafts the user kept, with their name, tags,
// routes and DNS as edited in review, and drops the rest, so a skipped
// draft leaves nothing behind. Every other field and the secrets come from
// the file. An empty keep closes the import without saving. A refused edit
// leaves the import open to fix.
func (s *HostService) CommitVPNImport(token string, keep []VPN) (importer.Report, error) {
	vpnImports.Lock()
	defer vpnImports.Unlock()
	sweepVPNImports()
	pend, ok := vpnImports.m[token]
	res := pend.res
	if !ok {
		return importer.Report{}, errors.New("this import is no longer open; import the file again")
	}
	var vpns []store.VPN
	var meta []importer.VPNImport
	for _, k := range keep {
		for i, p := range res.Bundle.VPNs {
			if p.ID != k.ID || slices.ContainsFunc(vpns, func(q store.VPN) bool { return q.ID == p.ID }) {
				continue
			}
			p.Name, p.Tags, p.Routes, p.DNSServers, p.DNSDomains = strings.TrimSpace(k.Name), k.Tags, k.Routes, k.DNSServers, k.DNSDomains
			p.RoutingMode, p.AutoApply, p.Domains = k.RoutingMode, k.AutoApply, cleanDomains(k.Domains)
			if err := checkVPN(vpnToUI(p)); err != nil {
				return importer.Report{}, fmt.Errorf("%s: %w", p.Name, err)
			}
			vpns, meta = append(vpns, p), append(meta, res.VPNs[i])
		}
	}
	delete(vpnImports.m, token)
	res.Bundle.VPNs, res.VPNs = vpns, meta
	return importer.Import(s.st, s.vault, res)
}

// ScanVPNConfigs lists the VPN configurations installed clients keep on
// this computer for the user to pick from (R39), without reading files
// this user cannot read.
func (s *HostService) ScanVPNConfigs() []vpnconf.Found {
	return vpnconf.Scan()
}
