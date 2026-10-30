package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/wailsapp/wails/v3/pkg/application"

	"hostbay/internal/apperr"
	"hostbay/internal/secrets"
	"hostbay/internal/store"
	"hostbay/internal/vpn"
	"hostbay/internal/vpn/detect"
	"hostbay/internal/vpn/proto"
)

// VPN is a VPN profile for Settings and the VPN panel (R1); see store.VPN.
// Like a proxy's password, its secrets are never sent back, only whether
// any are saved. On save, a nil or empty secret keeps the saved one.
type VPN struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Type          string   `json:"type"`
	Tier          string   `json:"tier"`
	Gateway       string   `json:"gateway,omitempty"`
	Port          int      `json:"port,omitempty"`
	User          string   `json:"user,omitempty"`
	Auth          string   `json:"auth,omitempty"`
	Address       []string `json:"address,omitempty"`
	PeerKey       string   `json:"peerKey,omitempty"`
	Routes        []string `json:"routes,omitempty"`
	DNSServers    []string `json:"dnsServers,omitempty"`
	DNSDomains    []string `json:"dnsDomains,omitempty"`
	MTU           int      `json:"mtu,omitempty"`
	Tags          []string `json:"tags,omitempty"`
	AutoReconnect bool     `json:"autoReconnect,omitempty"`
	AutoApply     bool     `json:"autoApply,omitempty"`
	RoutingMode   string   `json:"routingMode,omitempty"`
	Client        string   `json:"client,omitempty"`
	// This computer's own, kept in store.Local and never synced: the
	// external client program LaunchVPNClient opens, and the tunnel
	// interface the profile is pinned to (R24).
	ClientPath string `json:"clientPath,omitempty"`
	Iface      string `json:"iface,omitempty"`

	Password     *string `json:"password,omitempty"`
	PrivateKey   *string `json:"privateKey,omitempty"`
	PresharedKey *string `json:"presharedKey,omitempty"`
	AuthKey      *string `json:"authKey,omitempty"`
	HasSecret    bool    `json:"hasSecret,omitempty"`
}

// VPN statuses (R9). connecting, signing_in, connected and a dropped
// profile waiting to reconnect hold a helper run.
const (
	VPNDisconnected = "disconnected"
	VPNConnecting   = "connecting"
	VPNSigningIn    = "signing_in" // Need says what the helper waits for
	VPNConnected    = "connected"
	VPNFailed       = "failed"  // never came up; Reason says why
	VPNDropped      = "dropped" // was up and ended; Reason says why
)

// VPNState is a profile's live state. It never carries the SOCKS port or
// login, which stay between Hostbay and the helper.
type VPNState struct {
	ID      string   `json:"id"`
	Status  string   `json:"status"`
	Reason  string   `json:"reason,omitempty"`
	Need    string   `json:"need,omitempty"`    // signing_in: "otp"
	Address []string `json:"address,omitempty"` // connected: the tunnel's own addresses
	Routes  []string `json:"routes,omitempty"`  // connected: networks reached through it
	Since   int64    `json:"since,omitempty"`   // connected: when it came up, Unix milliseconds
	Iface   string   `json:"iface,omitempty"`   // connected external profile: its tunnel's interface
	URL     string   `json:"url,omitempty"`     // signing_in at a browser: the sign-in page, only ever http or https
	Code    string   `json:"code,omitempty"`    // failed: a proto.Code* reason the UI acts on
	// ServerCert is the pin the gateway presented, with cert-unknown and
	// cert-changed: what TrustVPNCert accepts (R21).
	ServerCert string `json:"serverCert,omitempty"`

	err error // failed: the coded error to return to callers
}

var (
	// vpnRetryMin and vpnRetryMax bound the wait between reconnects (R18).
	vpnRetryMin = time.Second
	vpnRetryMax = time.Minute
	// vpnStableUp is how long a profile must stay up before a drop starts
	// the backoff over: one that drops at once (a password rejected after
	// the first connect) keeps backing off, so it cannot lock an account.
	vpnStableUp = 2 * time.Minute
)

const wireGuardPort = 51820

// vpnDetectEvery is how often external profiles' states follow detection.
const vpnDetectEvery = 5 * time.Second

// VPNService saves VPN profiles and runs the app-scoped ones, one
// hostbay-vpn helper per connected profile (KTD1).
type VPNService struct {
	st      *store.Store
	vault   *secrets.Vault
	helper  func() (string, error) // vpn.HelperPath; tests point it at a fake
	detect  func(context.Context) (detect.Snapshot, error)
	emit    func(name string, data any)
	openURL func(url string) // the default browser, for a browser sign-in
	// checkConnect refuses a system-wide connect the routing plan cannot
	// place (R13); RoutingService sets it.
	checkConnect func(id string) error
	// connecting is held across a system-wide connect's checkConnect and
	// its run's start, so two connects never both pass the check (R13).
	connecting sync.Mutex
	// tunnels is the helper connection system-wide tunnels run through, or
	// nil while there is none; RoutingService sets it.
	tunnels    func() tunnelClient
	osvpn      osVPN              // the system's own VPN service, for IKEv2 and L2TP
	stopDetect context.CancelFunc // ends the detection loop

	refreshing sync.Mutex // one detection at a time, so states apply in order

	mu      sync.Mutex
	runs    map[string]*vpnRun // profile ID -> run from connect until disconnect or the end
	states  map[string]VPNState
	changed chan struct{} // closed and replaced on every state change
	// exited is closed once the profile's latest helper has exited; the
	// next one starts only after, so two never share a profile's state.
	exited map[string]chan struct{}
}

// vpnRun is one connected profile, across reconnects.
type vpnRun struct {
	stop chan struct{} // closed by DisconnectVPN
	att  *vpnAttempt   // the current helper
	by   string        // who connected it: "" the user, or an MCP token's principal (U14)
}

// vpnAttempt is one helper process.
type vpnAttempt struct {
	h          *vpn.Helper // nil while starting
	user, pass string      // the per-run SOCKS login (KTD2)
	port       int         // the SOCKS port, once connected
	end        chan proto.Msg
	exited     chan struct{} // closed once the helper has exited, or never started
	nodeAAD    []byte        // Tailscale: what the node's state is sealed to, as the profile was at start
	pinned     string        // OpenConnect: the gateway pin the start carried
	sys        systemRun     // a system-wide run, in place of a helper
	iface      string        // a system-wide run's tunnel interface, once up
}

// close ends the attempt's helper or system-wide run, without waiting.
func (a *vpnAttempt) close() {
	if a.h != nil {
		a.h.Close()
	}
	if a.sys != nil {
		a.sys.Close()
	}
}

func NewVPNService(st *store.Store, vault *secrets.Vault) *VPNService {
	return &VPNService{
		st: st, vault: vault, helper: vpn.HelperPath, detect: detect.Detect, emit: emitEvent, openURL: openInBrowser, osvpn: systemOSVPN,
		runs: map[string]*vpnRun{}, states: map[string]VPNState{}, changed: make(chan struct{}), exited: map[string]chan struct{}{},
	}
}

// openInBrowser opens u in the user's default browser.
func openInBrowser(u string) {
	if a := application.Get(); a != nil {
		a.Browser.OpenURL(u)
	}
}

func vpnToUI(v store.VPN) VPN {
	return VPN{
		ID: v.ID, Name: v.Name, Type: v.Type, Tier: v.Tier, Gateway: v.Gateway, Port: v.Port, User: v.User, Auth: v.Auth,
		Address: v.Address, PeerKey: v.PeerKey, Routes: v.Routes, DNSServers: v.DNSServers, DNSDomains: v.DNSDomains,
		MTU: v.MTU, Tags: v.Tags, AutoReconnect: v.AutoReconnect, AutoApply: v.AutoApply, RoutingMode: v.RoutingMode,
		Client: v.Client, HasSecret: v.Secret != "",
	}
}

// ListVPNs lists the saved profiles.
func (s *VPNService) ListVPNs() ([]VPN, error) {
	d, err := s.st.Data()
	if err != nil {
		return nil, err
	}
	l, err := s.st.Local()
	if err != nil {
		return nil, err
	}
	out := []VPN{}
	for _, v := range d.VPNs.Items {
		out = append(out, withLocal(vpnToUI(v), l))
	}
	return out, nil
}

// withLocal adds this computer's own fields to v.
func withLocal(v VPN, l store.Local) VPN {
	v.ClientPath, v.Iface = l.VPNClients[v.ID], l.VPNIfaces[v.ID]
	return v
}

// SaveVPN adds or updates a profile. New secrets are sealed together with
// the saved ones they do not replace, bound to the profile's type, gateway,
// user and tier (R4): after any of those change, saved secrets no longer
// open and must be entered again. A connected profile picks up the change
// the next time it connects.
func (s *VPNService) SaveVPN(profile VPN) (VPN, error) {
	if err := checkVPN(profile); err != nil {
		return VPN{}, err
	}
	var out store.VPN
	moved := false // another tailnet, whose node this is not
	err := s.st.Edit(func(d *store.Data) error {
		v := store.VPN{Meta: store.Meta{ID: profile.ID}}
		if e := d.VPN(profile.ID); profile.ID != "" && e != nil {
			v = *e
			moved = e.Type != profile.Type || e.Gateway != profile.Gateway
		} else if v.ID == "" {
			v.ID = store.NewID()
		}
		v.Name, v.Type, v.Tier, v.Gateway, v.Port, v.User, v.Auth = profile.Name, profile.Type, profile.Tier, profile.Gateway, profile.Port, profile.User, profile.Auth
		v.Address, v.PeerKey, v.Routes, v.DNSServers, v.DNSDomains = profile.Address, profile.PeerKey, profile.Routes, profile.DNSServers, profile.DNSDomains
		v.MTU, v.Tags, v.AutoReconnect, v.AutoApply, v.RoutingMode, v.Client = profile.MTU, profile.Tags, profile.AutoReconnect, profile.AutoApply, profile.RoutingMode, profile.Client
		set := func(p *string) bool { return p != nil && *p != "" }
		if set(profile.Password) || set(profile.PrivateKey) || set(profile.PresharedKey) || set(profile.AuthKey) {
			var sec store.VPNSecrets
			if v.Secret != "" {
				// Ones that no longer open (moved gateway, say) are dropped.
				if b, err := s.vault.Open(v.Secret, v.SecretAAD()); err == nil {
					json.Unmarshal(b, &sec)
				}
			}
			for _, f := range []struct{ in, saved *string }{
				{profile.Password, &sec.Password}, {profile.PrivateKey, &sec.PrivateKey}, {profile.PresharedKey, &sec.PresharedKey}, {profile.AuthKey, &sec.AuthKey},
			} {
				if set(f.in) {
					*f.saved = *f.in
				}
			}
			b, _ := json.Marshal(sec)
			sealed, err := s.vault.Seal(b, v.SecretAAD())
			if err != nil {
				return err
			}
			v.Secret = sealed
		}
		out = d.PutVPN(v)
		return nil
	})
	if err != nil {
		return VPN{}, err
	}
	err = s.st.EditLocal(func(l *store.Local) error {
		l.VPNClients = setLocal(l.VPNClients, out.ID, profile.ClientPath)
		l.VPNIfaces = setLocal(l.VPNIfaces, out.ID, profile.Iface)
		if moved {
			delete(l.VPNNodeStates, out.ID)
		}
		return nil
	})
	v := vpnToUI(out)
	v.ClientPath, v.Iface = profile.ClientPath, profile.Iface
	return v, err
}

// setLocal sets m[id] to v, or removes it when v is "".
func setLocal(m map[string]string, id, v string) map[string]string {
	if v == "" {
		delete(m, id)
		return m
	}
	if m == nil {
		m = map[string]string{}
	}
	m[id] = v
	return m
}

// DeleteVPN disconnects the profile and removes it, leaving a tombstone for
// sync. Hosts that named it fail to connect until they name another.
func (s *VPNService) DeleteVPN(id string) error {
	if err := s.DisconnectVPN(id); err != nil {
		return err
	}
	s.waitExited(id) // a closing node writes its state; it must not outlive the profile
	if err := s.st.Edit(func(d *store.Data) error { return d.PurgeVPN(id) }); err != nil {
		return err
	}
	if err := s.st.EditLocal(func(l *store.Local) error {
		delete(l.VPNClients, id)
		delete(l.VPNIfaces, id)
		delete(l.VPNNodeStates, id)
		delete(l.VPNNarrow, id)
		return nil
	}); err != nil {
		return err
	}
	if err := os.RemoveAll(s.nodeDir(id)); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.states, id)
	delete(s.exited, id)
	s.mu.Unlock()
	return nil
}

// SignInAgainVPN disconnects a Tailscale profile and forgets its node, so
// the next connect signs in afresh (R20).
func (s *VPNService) SignInAgainVPN(id string) error {
	if err := s.DisconnectVPN(id); err != nil {
		return err
	}
	s.waitExited(id)
	return s.st.EditLocal(func(l *store.Local) error {
		delete(l.VPNNodeStates, id)
		return nil
	})
}

// waitExited waits until the profile's latest helper has exited, so nothing
// it writes while closing lands after what the caller does next.
func (s *VPNService) waitExited(id string) {
	s.mu.Lock()
	c := s.exited[id]
	s.mu.Unlock()
	if c != nil {
		<-c
	}
}

// nodeDir is a Tailscale profile's own folder, which holds what tsnet
// writes besides the node's keys. A profile ID from sync is not trusted as
// a path.
func (s *VPNService) nodeDir(id string) string {
	name := id
	if !safeName.MatchString(id) {
		sum := sha256.Sum256([]byte(id))
		name = hex.EncodeToString(sum[:])
	}
	return filepath.Join(s.st.Dir(), "vpn", name)
}

var safeName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// checkVPN is store.CheckVPN plus the profile's name and routing mode, and
// this computer's own fields: its interface and client program.
func checkVPN(v VPN) error {
	bad := func(s string, spaces bool) bool {
		return strings.IndexFunc(s, func(r rune) bool { return unicode.IsControl(r) || spaces && unicode.IsSpace(r) }) >= 0
	}
	if strings.TrimSpace(v.Name) == "" || bad(v.Name, false) {
		return errors.New("name the VPN profile")
	}
	if err := store.CheckVPN(store.VPN{Type: v.Type, Tier: v.Tier, Gateway: v.Gateway, Port: v.Port, User: v.User, Auth: v.Auth,
		Address: v.Address, PeerKey: v.PeerKey, Routes: v.Routes, DNSServers: v.DNSServers, DNSDomains: v.DNSDomains, MTU: v.MTU, Client: v.Client}); err != nil {
		return err
	}
	switch {
	case bad(v.RoutingMode, true) || bad(v.Iface, false):
		return errors.New("VPN profile fields cannot hold line breaks")
	case v.ClientPath != "" && (bad(v.ClientPath, false) || !filepath.IsAbs(v.ClientPath)):
		return errors.New("choose the VPN client program by its full path")
	}
	if v.ClientPath != "" {
		// A refresh drops a profile whose client program is gone (R23), so
		// a path that never existed would be dropped at once.
		if _, err := os.Stat(v.ClientPath); err != nil {
			return fmt.Errorf("the VPN client program %s is not on this computer", v.ClientPath)
		}
	}
	return nil
}

// VPNStates lists every profile whose state is known; the rest are
// disconnected.
func (s *VPNService) VPNStates() []VPNState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []VPNState{}
	for _, st := range s.states {
		out = append(out, st)
	}
	return out
}

// ConnectedVPNs counts the profiles Hostbay connected, so quitting can ask
// first (R19).
func (s *VPNService) ConnectedVPNs() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runs)
}

// active lists the profiles that are up or hold a run: one connecting,
// signing in or waiting to reconnect takes its routes too (R13).
func (s *VPNService) active() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for id := range s.runs {
		ids = append(ids, id)
	}
	for id, st := range s.states {
		if st.Status == VPNConnected && s.runs[id] == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

// set records and announces a state; call it with s.mu held so events go
// out in order.
func (s *VPNService) set(st VPNState) VPNState {
	s.states[st.ID] = st
	s.emit(VPNStateEvent, st)
	close(s.changed)
	s.changed = make(chan struct{})
	return st
}

// ConnectVPN connects the profile and waits until it is up, or failed; any
// failure is also shown as the profile's state. A profile already
// connecting is waited on, and one already up returns its state, so any
// number of callers share one helper. ctx bounds only the wait: the
// connection lives until DisconnectVPN, quit, or its own end.
func (s *VPNService) ConnectVPN(ctx context.Context, id string) (VPNState, error) {
	return s.connectAs(ctx, id, "")
}

// connectAs is ConnectVPN for by, who owns the run only if this call
// starts it.
func (s *VPNService) connectAs(ctx context.Context, id, by string) (VPNState, error) {
	var v *store.VPN // nil when unreadable: the run reports it
	if d, err := s.st.Data(); err == nil {
		v = d.VPN(id)
	}
	if v != nil && v.Tier == store.VPNTierExternal {
		// Its own app connects it; Hostbay only sees the tunnel (KTD7).
		s.mu.Lock()
		st := s.states[id]
		s.mu.Unlock()
		return st, apperr.New(apperr.VPNFailed, fmt.Sprintf("VPN %s connects in its own app; open it there", v.Name)).
			With("vpnId", id).With("reason", "external")
	}
	system := v != nil && vpnTier(*v) == store.VPNTierSystem && s.checkConnect != nil
	if system {
		s.connecting.Lock()
		if err := s.conflict(id); err != nil {
			s.connecting.Unlock()
			return VPNState{ID: id}, err
		}
	}
	s.mu.Lock()
	if s.runs[id] == nil {
		r := &vpnRun{stop: make(chan struct{}), by: by}
		s.runs[id] = r
		s.set(VPNState{ID: id, Status: VPNConnecting})
		go s.run(id, r)
	}
	s.mu.Unlock()
	if system {
		s.connecting.Unlock()
	}
	for {
		s.mu.Lock()
		st, running, changed := s.states[id], s.runs[id] != nil, s.changed
		s.mu.Unlock()
		switch {
		case st.Status == VPNConnected:
			return st, nil
		case !running:
			if st.err != nil {
				return st, st.err
			}
			msg := "VPN: " + st.Reason
			if st.Reason == "" {
				msg = "The VPN profile was disconnected."
			}
			return st, apperr.New(apperr.VPNFailed, msg).With("vpnId", id).With("reason", st.Status)
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return st, ctx.Err()
		}
	}
}

// conflict is checkConnect's refusal as the coded error connects return.
func (s *VPNService) conflict(id string) error {
	if err := s.checkConnect(id); err != nil {
		return apperr.New(apperr.VPNFailed, err.Error()).With("vpnId", id).With("reason", "conflict")
	}
	return nil
}

// refusedStart reports whether err is a start the profile itself refuses:
// a routing conflict, or settings no config may be built from. Retrying
// cannot fix either.
func refusedStart(err error) bool {
	var ae *apperr.Error
	return errors.As(err, &ae) && ae.Code == apperr.VPNFailed && (ae.Details["reason"] == "conflict" || ae.Details["reason"] == "invalid")
}

// checkSaved refuses a profile that sync or a restore saved with a value no
// config may carry, such as a line break that would add a WireGuard peer.
func checkSaved(v *store.VPN) error {
	if err := store.CheckVPN(*v); err != nil {
		return apperr.New(apperr.VPNFailed, fmt.Sprintf("VPN %s cannot connect: %v; fix it in the profile's settings", v.Name, err)).
			With("vpnId", v.ID).With("reason", "invalid")
	}
	return nil
}

// run brings the profile up and keeps it up: a dropped profile reconnects,
// with backoff, while its auto-reconnect setting is on (R18).
func (s *VPNService) run(id string, r *vpnRun) {
	wait, wasUp := vpnRetryMin, false
	for {
		end, err := s.attempt(id, r)
		var m proto.Msg
		var upFor time.Duration
		if err == nil {
			select {
			case m = <-end:
				s.mu.Lock()
				att := r.att
				if st := s.states[id]; st.Status == VPNConnected {
					upFor = time.Since(time.UnixMilli(st.Since))
				}
				s.mu.Unlock()
				att.close() // one that reported its end but lingers must not hold up the next
			case <-r.stop:
				return
			}
		}
		up := m.State == proto.Dropped
		if up && upFor >= vpnStableUp {
			wait = vpnRetryMin
		}
		wasUp = wasUp || up
		// A coded failure (an untrusted certificate, an openconnect too old)
		// or a refused start needs the user: retrying cannot fix it.
		needsUser := m.Code != "" || refusedStart(err)

		d, _ := s.st.Data()
		again := wasUp && !needsUser && d != nil && d.VPN(id) != nil && d.VPN(id).AutoReconnect
		s.mu.Lock()
		if s.runs[id] != r { // disconnected
			s.mu.Unlock()
			return
		}
		st := VPNState{ID: id, Status: VPNFailed, Reason: m.Reason, err: err}
		switch {
		case err != nil:
			st.Reason = err.Error()
		case m.Code != "":
			st = codedFailure(id, r.att.pinned, m)
		}
		if wasUp && !needsUser {
			st.Status, st.err = VPNDropped, nil
		}
		if !again {
			delete(s.runs, id)
		}
		s.set(st)
		s.mu.Unlock()
		if !again {
			return
		}
		select {
		case <-time.After(wait):
		case <-r.stop:
			return
		}
		wait = min(wait*2, vpnRetryMax)
		s.mu.Lock()
		if s.runs[id] != r {
			s.mu.Unlock()
			return
		}
		s.set(VPNState{ID: id, Status: VPNConnecting})
		s.mu.Unlock()
	}
}

// codedFailure is the failed state for a helper's coded failure, and the
// error ConnectVPN returns for it: its details carry the certificate the
// gateway presented and the one pinned, for the UI to ask (R21, AE8).
func codedFailure(id, pinned string, m proto.Msg) VPNState {
	st := VPNState{ID: id, Status: VPNFailed, Reason: m.Reason, Code: m.Code, ServerCert: m.ServerCert}
	if m.Code == proto.CodeNeedsSystemWide {
		st.Reason = "This VPN type needs system-wide mode on Windows, which runs through Hostbay's network helper."
	}
	e := apperr.New(apperr.VPNFailed, "VPN: "+st.Reason).With("vpnId", id).With("reason", m.Code)
	if m.ServerCert != "" {
		e.With("serverCert", m.ServerCert)
	}
	if m.Code == proto.CodeCertChanged && pinned != "" {
		e.With("pinned", pinned)
	}
	st.err = e
	return st
}

// attempt starts one helper for the profile as saved now. Its states update
// the profile's; the failed or dropped message that ends it comes back on
// the returned channel.
func (s *VPNService) attempt(id string, r *vpnRun) (<-chan proto.Msg, error) {
	a := &vpnAttempt{end: make(chan proto.Msg, 1), exited: make(chan struct{})}
	s.mu.Lock()
	prev := s.exited[id]
	s.exited[id], r.att = a.exited, a
	s.mu.Unlock()
	done, err := s.launch(id, r, a, prev)
	if err != nil {
		close(a.exited)
		return nil, err
	}
	go func() {
		<-done
		close(a.exited)
	}()
	return a.end, nil
}

// launch starts a's helper once the profile's previous one has exited, so
// two never run on one profile's state (a Tailscale node's), and the start
// carries what the last one wrote as it closed; a system-wide profile
// starts its system-wide run instead. It returns a channel closed once the
// helper or run has ended.
func (s *VPNService) launch(id string, r *vpnRun, a *vpnAttempt, prev <-chan struct{}) (<-chan struct{}, error) {
	if prev != nil {
		select {
		case <-prev:
		case <-r.stop:
			return nil, errors.New("the VPN profile was disconnected")
		}
	}
	d, err := s.st.Data()
	if err != nil {
		return nil, err
	}
	v := d.VPN(id)
	if v != nil && vpnTier(*v) == store.VPNTierSystem {
		// A reconnect is checked again: another profile may have taken its
		// routes while it was down (R13).
		if s.checkConnect != nil {
			if err := s.conflict(id); err != nil {
				return nil, err
			}
		}
		return s.launchSystem(id, r, a, v)
	}
	start, err := s.startMsgFor(id, v)
	if err != nil {
		return nil, err
	}
	path, err := s.helper()
	if err != nil {
		return nil, err
	}
	a.user, a.pass, a.pinned = start.SocksUser, start.SocksPass, v.PeerKey
	if v.Type == proto.TypeTailscale {
		a.nodeAAD = v.NodeStateAAD()
	}
	h, err := vpn.Start(context.Background(), path, start, func(m proto.Msg) { s.report(id, r, a, m) })
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	a.h = h
	stopped := s.runs[id] != r
	s.mu.Unlock()
	if stopped {
		h.Close()
	}
	return h.Done(), nil
}

// report applies one helper message to the profile's state.
func (s *VPNService) report(id string, r *vpnRun, a *vpnAttempt, m proto.Msg) {
	if m.T == proto.NodeState {
		// Even from a stale run: a node writes its state as it closes.
		s.keepNodeState(id, a, m.NodeState)
		return
	}
	if m.State == proto.Failed || m.State == proto.Dropped {
		select { // run sets the state, knowing whether it reconnects
		case a.end <- m:
		default: // only the first end counts
		}
		return
	}
	s.mu.Lock()
	if s.runs[id] != r || r.att != a {
		s.mu.Unlock()
		return
	}
	open := ""
	switch {
	case m.T == proto.Need:
		st := VPNState{ID: id, Status: VPNSigningIn, Need: m.Need}
		if m.Need == proto.NeedBrowser && webURL(m.URL) {
			st.URL, open = m.URL, m.URL
		}
		s.set(st)
	case m.State == proto.Connecting && s.states[id].Status != VPNConnecting:
		s.set(VPNState{ID: id, Status: VPNConnecting})
	case m.State == proto.Connected:
		a.port = m.SocksPort
		s.set(VPNState{ID: id, Status: VPNConnected, Address: m.Address, Routes: m.Routes, Iface: a.iface, Since: time.Now().UnixMilli()})
	}
	s.mu.Unlock()
	if open != "" {
		s.openURL(open)
	}
}

// webURL reports whether u is an http or https URL with a host: the only
// sign-in page a helper may have opened.
func webURL(u string) bool {
	p, err := url.Parse(u)
	return err == nil && (p.Scheme == "http" || p.Scheme == "https") && p.Host != ""
}

// keepNodeState seals a Tailscale node's state into this computer's local
// settings, which sync and backups never carry (KTD3). It is never logged.
// A state for a profile deleted, or moved to another tailnet, since the
// run began is dropped.
func (s *VPNService) keepNodeState(id string, a *vpnAttempt, state string) {
	if a.nodeAAD == nil || state == "" {
		return
	}
	d, err := s.st.Data()
	if err != nil {
		return
	}
	if v := d.VPN(id); v == nil || !bytes.Equal(v.NodeStateAAD(), a.nodeAAD) {
		return
	}
	sealed, err := s.vault.Seal([]byte(state), a.nodeAAD)
	if err != nil {
		return
	}
	s.st.EditLocal(func(l *store.Local) error {
		l.VPNNodeStates = setLocal(l.VPNNodeStates, id, sealed)
		return nil
	})
}

// openConnectConfig and tailnetConfig are the engines' Config, field for
// field: each refuses or ignores anything else.
type openConnectConfig struct {
	Gateway    string `json:"gateway"`
	User       string `json:"user,omitempty"`
	ServerCert string `json:"serverCert,omitempty"`
	Browser    bool   `json:"browser,omitempty"`
}

type tailnetConfig struct {
	Dir        string `json:"dir"`
	ControlURL string `json:"controlURL"`
	Hostname   string `json:"hostname"`
}

// startMsg builds the helper's start message from the saved profile, with
// its secrets opened and a fresh SOCKS login. It returns the profile as it
// was read.
func (s *VPNService) startMsg(id string) (proto.Msg, *store.VPN, error) {
	d, err := s.st.Data()
	if err != nil {
		return proto.Msg{}, nil, err
	}
	v := d.VPN(id)
	m, err := s.startMsgFor(id, v)
	if err != nil {
		return proto.Msg{}, nil, err
	}
	return m, v, nil
}

// startMsgFor is startMsg from profile id as already read, nil when it is
// gone.
func (s *VPNService) startMsgFor(id string, v *store.VPN) (proto.Msg, error) {
	if v == nil {
		return proto.Msg{}, apperr.New(apperr.VPNFailed, "This VPN profile no longer exists").With("vpnId", id).With("reason", "missing")
	}
	if err := checkSaved(v); err != nil {
		return proto.Msg{}, err
	}
	if v.Tier != store.VPNTierApp || v.Type != proto.TypeWireGuard && v.Type != proto.TypeTailscale && !proto.IsOpenConnect(v.Type) {
		return proto.Msg{}, apperr.New(apperr.VPNFailed, fmt.Sprintf("Hostbay cannot connect %s profiles (tier %s) yet", v.Type, v.Tier)).
			With("vpnId", id).With("reason", "unsupported")
	}
	if proto.IsOpenConnect(v.Type) && v.Auth == "certificate" {
		return proto.Msg{}, apperr.New(apperr.VPNFailed, "Hostbay cannot sign in to this VPN with a client certificate yet; choose another sign-in in the profile").
			With("vpnId", id).With("reason", "unsupported")
	}
	sec, err := s.openSecrets(v)
	if err != nil {
		return proto.Msg{}, err
	}
	m := proto.Msg{T: proto.Start, Type: v.Type, SocksUser: rand.Text(), SocksPass: rand.Text()}
	switch v.Type {
	case proto.TypeWireGuard:
		m.Config = wireGuardConfig(v)
		m.Secrets = map[string]string{"privateKey": sec.PrivateKey, "presharedKey": sec.PresharedKey}
	case proto.TypeTailscale:
		dir := s.nodeDir(id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return proto.Msg{}, err
		}
		node, err := s.nodeState(v)
		if err != nil {
			return proto.Msg{}, err
		}
		host, _ := os.Hostname()
		b, _ := json.Marshal(tailnetConfig{Dir: dir, ControlURL: v.Gateway, Hostname: nodeHostname(host)})
		m.Config = string(b)
		m.Secrets = map[string]string{"authKey": sec.AuthKey, "nodeState": node}
	default:
		c := openConnectConfig{Gateway: openConnectGateway(v.Gateway, v.Port), User: v.User, ServerCert: v.PeerKey,
			Browser: v.Type == proto.TypeAnyConnect && v.Auth == "browser"}
		pass := sec.Password
		if c.Browser {
			pass = "" // a stray line would answer openconnect's next prompt
		}
		b, _ := json.Marshal(c)
		m.Config = string(b)
		m.Secrets = map[string]string{"password": pass}
	}
	return m, nil
}

// openSecrets opens v's saved secrets.
func (s *VPNService) openSecrets(v *store.VPN) (store.VPNSecrets, error) {
	var sec store.VPNSecrets
	if v.Secret == "" {
		return sec, nil
	}
	b, err := s.vault.Open(v.Secret, v.SecretAAD())
	if err != nil {
		var ae *apperr.Error
		if errors.As(err, &ae) {
			ae.With("vpnId", v.ID)
		}
		return sec, err
	}
	return sec, json.Unmarshal(b, &sec)
}

// nodeState opens the Tailscale profile's saved node. One that no longer
// opens (sealed for another control server, say) is forgotten: the node
// signs in again.
func (s *VPNService) nodeState(v *store.VPN) (string, error) {
	l, err := s.st.Local()
	if err != nil || l.VPNNodeStates[v.ID] == "" {
		return "", err
	}
	b, err := s.vault.Open(l.VPNNodeStates[v.ID], v.NodeStateAAD())
	if undecryptable(err) {
		return "", s.st.EditLocal(func(l *store.Local) error {
			delete(l.VPNNodeStates, v.ID)
			return nil
		})
	}
	return string(b), err
}

// nodeHostname is a Tailscale node's name from this computer's host name:
// one DNS label ending in -hostbay.
func nodeHostname(host string) string {
	const suffix = "-hostbay"
	host, _, _ = strings.Cut(host, ".")
	name := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, strings.ToLower(host))
	name = strings.Trim(name, "-")
	if len(name) > 63-len(suffix) {
		name = strings.TrimRight(name[:63-len(suffix)], "-")
	}
	if name == "" {
		return "hostbay"
	}
	return name + suffix
}

// openConnectGateway adds port to gw unless gw names one: openconnect takes
// host, host:port and either followed by /path.
func openConnectGateway(gw string, port int) string {
	host, path, slash := strings.Cut(gw, "/")
	if _, _, err := net.SplitHostPort(host); err != nil && port != 0 {
		host = net.JoinHostPort(strings.Trim(host, "[]"), strconv.Itoa(port))
	}
	if slash {
		return host + "/" + path
	}
	return host
}

// wireGuardConfig writes the profile as a wg-quick .conf without its keys,
// which travel in the start message's secrets.
func wireGuardConfig(v *store.VPN) string {
	var b strings.Builder
	line := func(k string, vs ...string) {
		if len(vs) > 0 && vs[0] != "" {
			fmt.Fprintf(&b, "%s = %s\n", k, strings.Join(vs, ", "))
		}
	}
	b.WriteString("[Interface]\n")
	line("Address", v.Address...)
	line("DNS", append(slices.Clone(v.DNSServers), v.DNSDomains...)...)
	line("MTU", strconv.Itoa(v.TunnelMTU()))
	b.WriteString("[Peer]\n")
	line("PublicKey", v.PeerKey)
	port := v.Port
	if port == 0 {
		port = wireGuardPort
	}
	line("Endpoint", net.JoinHostPort(v.Gateway, strconv.Itoa(port)))
	line("AllowedIPs", v.Routes...)
	return b.String()
}

// AnswerVPN sends the one-time code a signing-in profile waits for (R20).
func (s *VPNService) AnswerVPN(id, code string) error {
	s.mu.Lock()
	r := s.runs[id]
	if r == nil || r.att == nil || r.att.h == nil || s.states[id].Status != VPNSigningIn {
		s.mu.Unlock()
		return errors.New("this VPN profile is not waiting for a code")
	}
	h := r.att.h
	s.mu.Unlock()
	return h.Answer(code) // the helper reports connecting again

}

// pinPattern is a gateway certificate pin as openconnect's --servercert takes it.
var pinPattern = regexp.MustCompile(`^pin-sha256:[A-Za-z0-9+/]{43}=$`)

// TrustVPNCert pins the gateway certificate an OpenConnect profile's last
// connect found untrusted, once the user accepted it (R21): only the pin
// the gateway presented. A changed certificate is refused, never trusted in
// place of the pinned one (AE8): ForgetVPNCert first, and the next connect
// asks again.
func (s *VPNService) TrustVPNCert(id, pin string) error {
	s.mu.Lock()
	st := s.states[id]
	s.mu.Unlock()
	if st.Code == proto.CodeCertChanged {
		return apperr.New(apperr.HostKeyChanged, "This VPN gateway's certificate changed, so Hostbay refuses it. Forget the saved certificate only if your VPN administrator confirms the change.").
			With("vpnId", id).With("reason", proto.CodeCertChanged)
	}
	if !pinPattern.MatchString(pin) || pin != st.ServerCert || st.Code != proto.CodeCertUnknown {
		return errors.New("this VPN gateway did not present that certificate; connect again to check it")
	}
	return s.repin(id, pin)
}

// ForgetVPNCert removes an OpenConnect profile's pinned gateway certificate
// after OS verification, as removing a changed host key or certificate
// does (KTD25); the next connect asks to trust whatever the gateway
// presents.
func (s *VPNService) ForgetVPNCert(ctx context.Context, id string) error {
	if err := s.vault.Gate(ctx, "forget the VPN gateway's saved certificate"); err != nil {
		return err
	}
	return s.repin(id, "")
}

// repin sets an OpenConnect profile's gateway pin, its PeerKey, which its
// secrets are sealed to: they are opened under the old pin and sealed again
// under the new, so the saved password survives. If they cannot be opened
// (a locked vault) nothing changes.
func (s *VPNService) repin(id, pin string) error {
	return s.st.Edit(func(d *store.Data) error {
		v := d.VPN(id)
		if v == nil || !proto.IsOpenConnect(v.Type) {
			return errors.New("this VPN profile has no gateway certificate")
		}
		var plain []byte
		if v.Secret != "" {
			b, err := s.vault.Open(v.Secret, v.SecretAAD())
			if err != nil && !undecryptable(err) {
				return err
			}
			plain = b // nil when they no longer open, which stays so
		}
		v.PeerKey = pin
		if plain != nil {
			sealed, err := s.vault.Seal(plain, v.SecretAAD())
			if err != nil {
				return err
			}
			v.Secret = sealed
		}
		d.PutVPN(*v)
		return nil
	})
}

// socks is where Hostbay dials through the connected app-scoped profile id:
// the helper's loopback SOCKS5 address and this run's login, looked up at
// each dial so a reconnect never leaves a stale port behind (KTD2). None of
// it may reach the UI, an event or a log.
func (s *VPNService) socks(id string) (addr, user, pass string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.runs[id]
	if r == nil || r.att == nil || s.states[id].Status != VPNConnected {
		return "", "", "", apperr.New(apperr.VPNDown, "This VPN profile is not connected").With("vpnId", id)
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(r.att.port)), r.att.user, r.att.pass, nil
}

// connected reports whether profile id's run is up.
func (s *VPNService) connected(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runs[id] != nil && s.states[id].Status == VPNConnected
}

// systemIface is the tunnel interface of system-wide profile id's run.
func (s *VPNService) systemIface(id, name string) (string, error) {
	s.mu.Lock()
	st, running := s.states[id], s.runs[id] != nil
	s.mu.Unlock()
	switch {
	case !running || st.Status != VPNConnected:
		return "", apperr.New(apperr.VPNDown, fmt.Sprintf("VPN %s is not connected; connect it, then try again", name)).With("vpnId", id)
	case st.Iface == "":
		return "", apperr.New(apperr.VPNFailed, fmt.Sprintf("VPN %s is connected, but Hostbay cannot tell which interface is its tunnel", name)).
			With("vpnId", id).With("reason", "iface")
	}
	return st.Iface, nil
}

// DisconnectVPN stops the profile's helper. Disconnecting a profile that is
// not connected clears a failed or dropped state.
func (s *VPNService) DisconnectVPN(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stop(id)
	return nil
}

// disconnectAs stops the profile only if by connected it, checked under the
// same lock, so a run the user started meanwhile is never stopped.
func (s *VPNService) disconnectAs(id, by string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.runs[id]; r == nil || r.by != by {
		return errors.New("this VPN profile is not connected by this client")
	}
	s.stop(id)
	return nil
}

// connectedBy is who connected the profile's run, if it has one.
func (s *VPNService) connectedBy(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.runs[id]; r != nil {
		return r.by, true
	}
	return "", false
}

// stop ends the profile's run; s.mu is held.
func (s *VPNService) stop(id string) {
	r := s.runs[id]
	if r != nil {
		delete(s.runs, id)
		close(r.stop)
		if r.att != nil {
			r.att.close()
		}
	}
	// An external profile's state is its tunnel's, which this cannot end;
	// a system-wide run's tunnel ends with it.
	if st, known := s.states[id]; known && (st.Iface == "" || r != nil) && st.Status != VPNDisconnected {
		s.set(VPNState{ID: id, Status: VPNDisconnected})
	}
}

// ServiceShutdown disconnects every profile when the app quits (R19).
func (s *VPNService) ServiceShutdown() error {
	if s.stopDetect != nil {
		s.stopDetect()
	}
	s.mu.Lock()
	var ids []string
	for id := range s.runs {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.DisconnectVPN(id)
	}
	// A closing Tailscale node writes its keys; quitting first loses them.
	for _, id := range ids {
		s.waitExited(id)
	}
	return nil
}

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
	return err
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
	labels := detect.Label(snap.Tunnels, ps)
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
			return strings.EqualFold(f.Name, v.Name) || strings.EqualFold(f.Name, v.Client)
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
