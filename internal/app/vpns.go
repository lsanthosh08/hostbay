package app

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
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

	err error // failed: the coded error to return to callers
}

var (
	// vpnRetryMin and vpnRetryMax bound the wait between reconnects (R18).
	vpnRetryMin = time.Second
	vpnRetryMax = time.Minute
)

const wireGuardPort = 51820

// vpnDetectEvery is how often external profiles' states follow detection.
const vpnDetectEvery = 5 * time.Second

// VPNService saves VPN profiles and runs the app-scoped ones, one
// hostbay-vpn helper per connected profile (KTD1).
type VPNService struct {
	st         *store.Store
	vault      *secrets.Vault
	helper     func() (string, error) // vpn.HelperPath; tests point it at a fake
	detect     func(context.Context) (detect.Snapshot, error)
	emit       func(name string, data any)
	stopDetect context.CancelFunc // ends the detection loop

	refreshing sync.Mutex // one detection at a time, so states apply in order

	mu      sync.Mutex
	runs    map[string]*vpnRun // profile ID -> run from connect until disconnect or the end
	states  map[string]VPNState
	changed chan struct{} // closed and replaced on every state change
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
}

func NewVPNService(st *store.Store, vault *secrets.Vault) *VPNService {
	return &VPNService{
		st: st, vault: vault, helper: vpn.HelperPath, detect: detect.Detect, emit: emitEvent,
		runs: map[string]*vpnRun{}, states: map[string]VPNState{}, changed: make(chan struct{}),
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
	err := s.st.Edit(func(d *store.Data) error {
		v := store.VPN{Meta: store.Meta{ID: profile.ID}}
		if e := d.VPN(profile.ID); profile.ID != "" && e != nil {
			v = *e
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
	if err := s.st.Edit(func(d *store.Data) error { return d.PurgeVPN(id) }); err != nil {
		return err
	}
	if err := s.st.EditLocal(func(l *store.Local) error {
		delete(l.VPNClients, id)
		delete(l.VPNIfaces, id)
		return nil
	}); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.states, id)
	s.mu.Unlock()
	return nil
}

// checkVPN refuses a profile whose fields could not work or could add lines
// to the engine configuration Hostbay writes from them.
func checkVPN(v VPN) error {
	bad := func(s string, spaces bool) bool {
		return strings.IndexFunc(s, func(r rune) bool { return unicode.IsControl(r) || spaces && unicode.IsSpace(r) }) >= 0
	}
	switch {
	case strings.TrimSpace(v.Name) == "" || bad(v.Name, false):
		return errors.New("name the VPN profile")
	case v.Type == "" || bad(v.Type, true):
		return errors.New("choose the VPN type")
	case v.Tier != store.VPNTierApp && v.Tier != store.VPNTierSystem && v.Tier != store.VPNTierExternal:
		return errors.New("the VPN tier must be app, system or external")
	case len(v.Gateway) > 253 || bad(v.Gateway, true) || bad(v.PeerKey, true):
		return errors.New("enter the VPN gateway and keys without spaces or line breaks")
	case v.Port < 0 || v.Port > 65535:
		return errors.New("the VPN port must be between 1 and 65535")
	case v.MTU != 0 && (v.MTU < 576 || v.MTU > 65535):
		return errors.New("the MTU must be between 576 and 65535")
	case bad(v.User, false) || bad(v.Auth, true) || bad(v.RoutingMode, true) || bad(v.Client, false) || bad(v.Iface, false):
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
	for _, p := range append(slices.Clone(v.Address), v.Routes...) {
		if _, err := netip.ParsePrefix(p); err != nil {
			return fmt.Errorf("%q is not a network such as 10.0.0.0/24", p)
		}
	}
	for _, a := range v.DNSServers {
		if _, err := netip.ParseAddr(a); err != nil {
			return fmt.Errorf("DNS server %q is not an IP address", a)
		}
	}
	for _, n := range v.DNSDomains {
		if n == "" || bad(n, true) || strings.Contains(n, ",") {
			return fmt.Errorf("DNS domain %q is not a domain name", n)
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
	if d, err := s.st.Data(); err == nil && d.VPN(id) != nil && d.VPN(id).Tier == store.VPNTierExternal {
		// Its own app connects it; Hostbay only sees the tunnel (KTD7).
		s.mu.Lock()
		st := s.states[id]
		s.mu.Unlock()
		return st, apperr.New(apperr.VPNFailed, fmt.Sprintf("VPN %s connects in its own app; open it there", d.VPN(id).Name)).
			With("vpnId", id).With("reason", "external")
	}
	s.mu.Lock()
	if s.runs[id] == nil {
		r := &vpnRun{stop: make(chan struct{}), by: by}
		s.runs[id] = r
		s.set(VPNState{ID: id, Status: VPNConnecting})
		go s.run(id, r)
	}
	s.mu.Unlock()
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

// run brings the profile up and keeps it up: a dropped profile reconnects,
// with backoff, while its auto-reconnect setting is on (R18).
func (s *VPNService) run(id string, r *vpnRun) {
	wait, wasUp := vpnRetryMin, false
	for {
		end, err := s.attempt(id, r)
		var m proto.Msg
		if err == nil {
			select {
			case m = <-end:
			case <-r.stop:
				return
			}
		}
		up := m.State == proto.Dropped
		if up {
			wait = vpnRetryMin
		}
		wasUp = wasUp || up

		d, _ := s.st.Data()
		again := wasUp && d != nil && d.VPN(id) != nil && d.VPN(id).AutoReconnect
		s.mu.Lock()
		if s.runs[id] != r { // disconnected
			s.mu.Unlock()
			return
		}
		st := VPNState{ID: id, Status: VPNFailed, Reason: m.Reason, err: err}
		if err != nil {
			st.Reason = err.Error()
		}
		if wasUp {
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

// attempt starts one helper for the profile as saved now. Its states update
// the profile's; the failed or dropped message that ends it comes back on
// the returned channel.
func (s *VPNService) attempt(id string, r *vpnRun) (<-chan proto.Msg, error) {
	start, err := s.startMsg(id)
	if err != nil {
		return nil, err
	}
	path, err := s.helper()
	if err != nil {
		return nil, err
	}
	a := &vpnAttempt{user: start.SocksUser, pass: start.SocksPass, end: make(chan proto.Msg, 1)}
	s.mu.Lock()
	r.att = a
	s.mu.Unlock()
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
	return a.end, nil
}

// report applies one helper message to the profile's state.
func (s *VPNService) report(id string, r *vpnRun, a *vpnAttempt, m proto.Msg) {
	if m.State == proto.Failed || m.State == proto.Dropped {
		select { // run sets the state, knowing whether it reconnects
		case a.end <- m:
		default: // only the first end counts
		}
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runs[id] != r || r.att != a {
		return
	}
	switch {
	case m.T == proto.Need:
		s.set(VPNState{ID: id, Status: VPNSigningIn, Need: m.Need})
	case m.State == proto.Connecting && s.states[id].Status != VPNConnecting:
		s.set(VPNState{ID: id, Status: VPNConnecting})
	case m.State == proto.Connected:
		a.port = m.SocksPort
		s.set(VPNState{ID: id, Status: VPNConnected, Address: m.Address, Routes: m.Routes, Since: time.Now().UnixMilli()})
	}
}

// startMsg builds the helper's start message from the saved profile, with
// its secrets opened and a fresh SOCKS login.
func (s *VPNService) startMsg(id string) (proto.Msg, error) {
	d, err := s.st.Data()
	if err != nil {
		return proto.Msg{}, err
	}
	v := d.VPN(id)
	if v == nil {
		return proto.Msg{}, apperr.New(apperr.VPNFailed, "This VPN profile no longer exists").With("vpnId", id).With("reason", "missing")
	}
	if v.Tier != store.VPNTierApp || v.Type != proto.TypeWireGuard {
		return proto.Msg{}, apperr.New(apperr.VPNFailed, fmt.Sprintf("Hostbay cannot connect %s profiles (tier %s) yet", v.Type, v.Tier)).
			With("vpnId", id).With("reason", "unsupported")
	}
	var sec store.VPNSecrets
	if v.Secret != "" {
		b, err := s.vault.Open(v.Secret, v.SecretAAD())
		if err != nil {
			var ae *apperr.Error
			if errors.As(err, &ae) {
				ae.With("vpnId", id)
			}
			return proto.Msg{}, err
		}
		if err := json.Unmarshal(b, &sec); err != nil {
			return proto.Msg{}, err
		}
	}
	return proto.Msg{
		T: proto.Start, Type: v.Type, Config: wireGuardConfig(v),
		Secrets:   map[string]string{"privateKey": sec.PrivateKey, "presharedKey": sec.PresharedKey},
		SocksUser: rand.Text(), SocksPass: rand.Text(),
	}, nil
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
	if r := s.runs[id]; r != nil {
		delete(s.runs, id)
		close(r.stop)
		if r.att != nil && r.att.h != nil {
			r.att.h.Close()
		}
	}
	// An external profile's state is its tunnel's, which this cannot end.
	if st, known := s.states[id]; known && st.Iface == "" && st.Status != VPNDisconnected {
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
	_, _, err := s.labelNow(ctx)
	return err
}

// labelNow is refresh, returning what detection saw and each tunnel's
// label: the routing engine (U11) routes only through labeled tunnels.
func (s *VPNService) labelNow(ctx context.Context) (detect.Snapshot, []detect.Labeled, error) {
	s.refreshing.Lock()
	defer s.refreshing.Unlock()
	d, err := s.st.Data()
	if err != nil {
		return detect.Snapshot{}, nil, err
	}
	l, err := s.st.Local()
	if err != nil {
		return detect.Snapshot{}, nil, err
	}
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
	// A profile moved off the external tier keeps no tunnel.
	for id, st := range s.states {
		if v := d.VPN(id); st.Iface != "" && (v == nil || v.Tier != store.VPNTierExternal) {
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
	snap, _, err := s.labelNow(ctx)
	if err != nil {
		return nil, err
	}
	d, err := s.st.Data()
	if err != nil {
		return nil, err
	}
	l, err := s.st.Local()
	if err != nil {
		return nil, err
	}
	names := func(v store.VPN) func(detect.VPN) bool {
		return func(f detect.VPN) bool {
			return strings.EqualFold(f.Name, v.Name) || strings.EqualFold(f.Name, v.Client)
		}
	}
	// shortcut: only hosts count as users; per-destination routes (U11) must be added here.
	used := func(id string) bool {
		return slices.ContainsFunc(d.Hosts.Items, func(h store.Host) bool { return store.Deref(h.VPNID) == id })
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
