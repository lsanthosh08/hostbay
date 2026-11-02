package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"hostbay/internal/apperr"
	"hostbay/internal/store"
	"hostbay/internal/vpn/detect"
	"hostbay/internal/vpn/sase"
)

// vpnDetectEvery is how often external profiles' states follow detection.
const vpnDetectEvery = 5 * time.Second

// ServiceStartup starts following external profiles' tunnels: while any
// external profile is saved, detection runs every vpnDetectEvery and their
// states change as their tunnels come and go (KTD7).
func (s *VPNService) ServiceStartup(context.Context, application.ServiceOptions) error {
	ctx, stop := context.WithCancel(context.Background())
	s.stopDetect = stop
	tick := func() {
		if d, err := s.st.Data(); err == nil && slices.ContainsFunc(d.VPNs.Items, func(v store.VPN) bool { return v.Tier == store.VPNTierExternal }) {
			s.refresh(ctx)
		}
	}
	go func() {
		tick()
		t := time.NewTicker(vpnDetectEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				tick()
			}
		}
	}()
	return nil
}

// refresh runs detection now and moves each external profile between
// disconnected and connected as its tunnel is labeled (R24): one detection
// cannot tell apart stays disconnected and asks for a pin.
func (s *VPNService) refresh(ctx context.Context) error {
	d, err := s.st.Data()
	if err != nil {
		return err
	}
	l, err := s.st.Local()
	if err != nil {
		return err
	}
	_, _, err = s.labelNow(ctx, d, l)
	s.reconcile(d)
	return err
}

// reconcile stops the run of a profile that is gone: sync can delete one
// while its tunnel is up, and nothing else would end it.
func (s *VPNService) reconcile(d *store.Data) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.runs {
		if d.VPN(id) == nil {
			s.stop(id)
		}
	}
}

// labelNow is refresh for the profiles in d and l, returning what detection
// saw and each tunnel's label: the routing engine (U11) routes only through
// labeled tunnels.
func (s *VPNService) labelNow(ctx context.Context, d *store.Data, l store.Local) (detect.Snapshot, []detect.Labeled, error) {
	s.refreshing.Lock()
	defer s.refreshing.Unlock()
	snap, err := s.detect(ctx)
	if err != nil {
		return detect.Snapshot{}, nil, err
	}
	var ps []detect.Profile
	for _, v := range d.VPNs.Items {
		if v.Tier == store.VPNTierExternal {
			ps = append(ps, detect.Profile{
				ID: v.ID, Names: []string{v.Name, v.Client}, Shape: detect.ShapeFor(v.Type),
				// Only the tunnel's own addresses: a route names where traffic
				// goes, which a tunnel of another VPN may hold too.
				Ranges: v.Address, Iface: l.VPNIfaces[v.ID],
			})
		}
	}
	// Hostbay's own tunnels (a system-wide run through the helper) have no
	// scutil owner, so by shape or range they could be taken for an external
	// profile's.
	s.mu.Lock()
	own := map[string]bool{}
	for id, st := range s.states {
		if s.runs[id] != nil && st.Iface != "" {
			own[st.Iface] = true
		}
	}
	s.mu.Unlock()
	tunnels := slices.DeleteFunc(slices.Clone(snap.Tunnels), func(t detect.Tunnel) bool { return own[t.Iface] })
	labels := detect.Label(tunnels, ps)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range ps {
		st := VPNState{ID: p.ID, Status: VPNDisconnected}
		if i := slices.IndexFunc(labels, func(t detect.Labeled) bool { return t.Profile == p.ID }); i >= 0 {
			st = VPNState{ID: p.ID, Status: VPNConnected, Iface: labels[i].Iface, Address: []string{labels[i].Local}, Since: time.Now().UnixMilli()}
		} else if slices.ContainsFunc(labels, func(t detect.Labeled) bool {
			return t.Profile == "" && (t.Shape == "" || p.Shape == "" || t.Shape == p.Shape)
		}) {
			st.Reason = "Hostbay cannot tell which tunnel is this VPN's; pin its interface in the profile's settings"
		}
		old, known := s.states[p.ID]
		if old.Status == VPNConnected && old.Iface == st.Iface {
			st.Since = old.Since
		}
		if known && old.Status == st.Status && old.Iface == st.Iface && old.Reason == st.Reason || !known && st.Status == VPNDisconnected && st.Reason == "" {
			continue
		}
		s.set(st)
	}
	for id := range s.extBy {
		if s.states[id].Status != VPNConnected {
			delete(s.extBy, id) // its app, or the VPN itself, ended it
		}
	}
	// A profile moved off the external tier keeps no tunnel; a system-wide
	// run's tunnel is its own.
	for id, st := range s.states {
		if v := d.VPN(id); st.Iface != "" && s.runs[id] == nil && (v == nil || v.Tier != store.VPNTierExternal) {
			s.set(VPNState{ID: id, Status: VPNDisconnected})
		}
	}
	return snap, labels, nil
}

// iface is the interface of external profile id's tunnel, labeled afresh:
// a utun number a stale label named may by now be another VPN's.
func (s *VPNService) iface(ctx context.Context, id, name string) (string, error) {
	if err := s.refresh(ctx); err != nil {
		return "", apperr.New(apperr.VPNFailed, fmt.Sprintf("VPN %s: Hostbay could not look for its tunnel: %v", name, err)).With("vpnId", id).With("reason", "detect")
	}
	s.mu.Lock()
	st := s.states[id]
	s.mu.Unlock()
	if st.Status != VPNConnected {
		msg := fmt.Sprintf("VPN %s is not connected; connect it in its own app, then try again", name)
		if st.Reason != "" {
			msg = fmt.Sprintf("VPN %s: %s", name, st.Reason)
		}
		return "", apperr.New(apperr.VPNDown, msg).With("vpnId", id)
	}
	return st.Iface, nil
}

// VPNTunnels lists the tunnel interfaces up on this computer now, for the
// profile form to offer when a tunnel must be pinned. It changes nothing.
func (s *VPNService) VPNTunnels(ctx context.Context) ([]detect.Tunnel, error) {
	snap, err := s.detect(ctx)
	if err != nil {
		return nil, err
	}
	if snap.Tunnels == nil {
		return []detect.Tunnel{}, nil
	}
	return snap.Tunnels, nil
}

// DetectVPNs looks for VPNs on this computer now and lists the ones no
// profile names yet, to offer as external profiles (R22). It also drops an
// external profile whose client program this computer saved and has since
// removed, that detection no longer sees and no host uses (R23); a profile
// with no client program here was set up elsewhere and stays.
func (s *VPNService) DetectVPNs(ctx context.Context) ([]detect.VPN, error) {
	d, err := s.st.Data()
	if err != nil {
		return nil, err
	}
	l, err := s.st.Local()
	if err != nil {
		return nil, err
	}
	snap, _, err := s.labelNow(ctx, d, l)
	if err != nil {
		return nil, err
	}
	names := func(v store.VPN) func(detect.VPN) bool {
		return func(f detect.VPN) bool {
			// The app's own path names it too: Harmony SASE installs as "Check Point SASE".
			return strings.EqualFold(f.Name, v.Name) || strings.EqualFold(f.Name, v.Client) || (f.Client != "" && f.Client == l.VPNClients[v.ID])
		}
	}
	// Hosts and routes both count as users (R23).
	used := func(id string) bool {
		v := d.VPN(id)
		return slices.ContainsFunc(d.Hosts.Items, func(h store.Host) bool { return store.Deref(h.VPNID) == id }) ||
			(v != nil && len(v.Routes) > 0) || len(l.VPNNarrow[id]) > 0
	}
	for _, v := range d.VPNs.Items {
		path := l.VPNClients[v.ID]
		if _, err := os.Stat(path); v.Tier != store.VPNTierExternal || path == "" || !errors.Is(err, os.ErrNotExist) ||
			slices.ContainsFunc(snap.VPNs, names(v)) || used(v.ID) {
			continue
		}
		if err := s.DeleteVPN(v.ID); err != nil {
			return nil, err
		}
	}
	out := []detect.VPN{}
	for _, f := range snap.VPNs {
		if !slices.ContainsFunc(d.VPNs.Items, func(v store.VPN) bool { return names(v)(f) }) {
			f.Connected = f.Connected || detect.CLIConnected(ctx, f.Type)
			out = append(out, f)
		}
	}
	return out, nil
}

// LaunchVPNClient opens external profile id's client program, the one saved
// on this computer: a synced name never chooses what Hostbay runs (KTD4).
func (s *VPNService) LaunchVPNClient(id string) error {
	l, err := s.st.Local()
	if err != nil {
		return err
	}
	path := l.VPNClients[id]
	if path == "" || !filepath.IsAbs(path) {
		return errors.New("choose this VPN's client program in the profile's settings first")
	}
	cmd := exec.Command(path)
	if runtime.GOOS == "darwin" {
		cmd = exec.Command("open", "-a", path)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("could not open %s: %w", path, err)
	}
	go cmd.Wait()
	return nil
}

// canDriveClient reports whether Hostbay can connect and disconnect external
// profile id through its client's command line, with no window.
func (s *VPNService) canDriveClient(id string) bool {
	d, err := s.st.Data()
	if err != nil {
		return false
	}
	l, err := s.st.Local()
	if err != nil {
		return false
	}
	v := d.VPN(id)
	return v != nil && v.Tier == store.VPNTierExternal && v.Type == "perimeter81" && sase.Bin(l.VPNClients[id]) != ""
}

// connectClientAs is ConnectVPNClient for by, an MCP client, who then owns
// the connection: only it may disconnect it.
func (s *VPNService) connectClientAs(ctx context.Context, id, by string) error {
	if !s.canDriveClient(id) {
		return errors.New("this VPN connects in its own app")
	}
	if err := s.ConnectVPNClient(ctx, id); err != nil {
		return err
	}
	s.mu.Lock()
	if s.states[id].Status == VPNConnected {
		s.extBy[id] = by
	}
	s.mu.Unlock()
	return nil
}

// disconnectClientAs ends what connectClientAs started, and nothing else.
func (s *VPNService) disconnectClientAs(ctx context.Context, id, by string) error {
	s.mu.Lock()
	owner, ok := s.extBy[id]
	s.mu.Unlock()
	if !ok || owner != by {
		return errors.New("this VPN profile is not connected by this client")
	}
	if err := s.DisconnectVPNClient(ctx, id); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.extBy, id)
	s.mu.Unlock()
	return nil
}

// ConnectVPNClient connects external profile id through its client program's
// own command line when it has one (Harmony SASE, Perimeter 81), so no app
// window opens; any other client is opened for the user to connect (R3).
func (s *VPNService) ConnectVPNClient(ctx context.Context, id string) error {
	return s.driveClient(ctx, id, sase.Connect, s.LaunchVPNClient)
}

// DisconnectVPNClient disconnects it the same way. A client Hostbay cannot
// drive is left to the user, whose app holds the connection.
func (s *VPNService) DisconnectVPNClient(ctx context.Context, id string) error {
	return s.driveClient(ctx, id, sase.Disconnect, func(string) error {
		return errors.New("Disconnect this VPN in its own app.")
	})
}

func (s *VPNService) driveClient(ctx context.Context, id string, do func(context.Context, string) error, other func(string) error) error {
	d, err := s.st.Data()
	if err != nil {
		return err
	}
	l, err := s.st.Local()
	if err != nil {
		return err
	}
	v := d.VPN(id)
	if v == nil || v.Tier != store.VPNTierExternal {
		return errors.New("this profile is not an external VPN")
	}
	bin := ""
	if v.Type == "perimeter81" {
		bin = sase.Bin(l.VPNClients[id])
	}
	if bin == "" {
		return other(id)
	}
	if err := do(ctx, bin); err != nil {
		return err
	}
	// Its tunnel takes a moment to show; the next detection pass sees it.
	return s.refresh(ctx)
}
