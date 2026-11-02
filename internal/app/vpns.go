package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

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
	AuthGroup     string   `json:"authGroup,omitempty"`
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
		ID: v.ID, Name: v.Name, Type: v.Type, Tier: v.Tier, Gateway: v.Gateway, Port: v.Port, User: v.User, Auth: v.Auth, AuthGroup: v.AuthGroup,
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
		v.AuthGroup = profile.AuthGroup
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

// checkVPN is store.CheckVPN plus the profile's name and routing mode, and
// this computer's own fields: its interface and client program.
func checkVPN(v VPN) error {
	bad := store.BadVPNText
	if strings.TrimSpace(v.Name) == "" || bad(v.Name, false) {
		return errors.New("name the VPN profile")
	}
	if err := store.CheckVPN(store.VPN{Type: v.Type, Tier: v.Tier, Gateway: v.Gateway, Port: v.Port, User: v.User, Auth: v.Auth, AuthGroup: v.AuthGroup,
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
