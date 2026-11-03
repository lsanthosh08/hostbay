package store

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"unicode"
)

// VPN is a VPN profile hosts reach their servers through (R1, R2). Profiles
// are their own collection so sync carries their one sealed field, Secret,
// bound to SecretAAD like a proxy's password (KTD4). A host names its
// profile by ID; a missing profile is an error, never a direct connection.
// Program paths of external clients mean nothing on another computer, so
// they live in Local, not here.
type VPN struct {
	Meta
	Name    string `json:"name"`
	Type    string `json:"type"` // a row of the supported-types table, such as "wireguard" or "openvpn"
	Tier    string `json:"tier"` // VPNTierApp, VPNTierSystem or VPNTierExternal
	Gateway string `json:"gateway,omitempty"`
	Port    int    `json:"port,omitempty"`
	User    string `json:"user,omitempty"`
	Auth    string `json:"auth,omitempty"` // the sign-in method, such as "password", "certificate" or "otp"
	// AuthGroup is the gateway group an OpenConnect sign-in picks, for a gateway that offers several.
	AuthGroup string `json:"authGroup,omitempty"`
	// WireGuard: the tunnel's own addresses (CIDR) and the server's public
	// key. Neither is secret.
	Address []string `json:"address,omitempty"`
	PeerKey string   `json:"peerKey,omitempty"`

	Routes     []string `json:"routes,omitempty"`     // CIDRs sent through the VPN
	DNSServers []string `json:"dnsServers,omitempty"` // resolvers inside the VPN
	DNSDomains []string `json:"dnsDomains,omitempty"` // names resolved through DNSServers
	MTU        int      `json:"mtu,omitempty"`        // 0 is DefaultVPNMTU
	// Tags are free text, matched without regard to case and shared with
	// host tags (R6).
	Tags          []string `json:"tags,omitempty"`
	AutoReconnect bool     `json:"autoReconnect,omitempty"`
	// AutoApply is nil until the user chooses; AutoApplies says what nil means.
	AutoApply   *bool  `json:"autoApply,omitempty"`   // apply the routes on connect
	RoutingMode string `json:"routingMode,omitempty"` // how the routes are applied; "" is per destination
	Client      string `json:"client,omitempty"`      // external: the installed client the profile names (R3)

	// Secret is every profile secret as one VPNSecrets JSON object, sealed
	// ("enc:v1:...").
	Secret string `json:"secret,omitempty"`
}

// BadVPNText reports whether s holds a control character, or with spaces
// any whitespace, either of which could add a line to an engine
// configuration.
func BadVPNText(s string, spaces bool) bool {
	return strings.IndexFunc(s, func(r rune) bool { return unicode.IsControl(r) || spaces && unicode.IsSpace(r) }) >= 0
}

// CheckVPN refuses a profile whose fields could not work or could add lines
// to the engine configuration Hostbay writes from them. Its name and routing
// mode, and the fields kept in Local, are the caller's to check.
func CheckVPN(v VPN) error {
	bad := BadVPNText
	switch {
	case v.Type == "" || bad(v.Type, true):
		return errors.New("no VPN type")
	case v.Tier != VPNTierApp && v.Tier != VPNTierSystem && v.Tier != VPNTierExternal:
		return fmt.Errorf("tier %q is not app, system or external", v.Tier)
	case len(v.Gateway) > 253 || bad(v.Gateway, true) || strings.HasPrefix(v.Gateway, "-") || bad(v.PeerKey, true):
		return fmt.Errorf("the gateway %q is not a valid address", v.Gateway)
	case v.Port < 0 || v.Port > 65535:
		return fmt.Errorf("port %d is out of range", v.Port)
	case v.MTU != 0 && (v.MTU < 576 || v.MTU > 65535):
		return fmt.Errorf("MTU %d is out of range", v.MTU)
	case bad(v.User, false) || bad(v.AuthGroup, false) || bad(v.Auth, true) || bad(v.Client, false):
		return errors.New("a field contains a line break or control character")
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

// VPNSecrets is what a VPN's Secret holds once opened: sync carries one
// sealed field per collection, so the secrets travel together (KTD4).
type VPNSecrets struct {
	Password     string `json:"password,omitempty"`
	PrivateKey   string `json:"privateKey,omitempty"`   // a WireGuard key, or a client certificate's key
	PresharedKey string `json:"presharedKey,omitempty"` // WireGuard or IPsec
	AuthKey      string `json:"authKey,omitempty"`      // Tailscale or Headscale
	// OpenVPN holds an imported profile's directives that have no typed
	// field (cipher, verify-x509-name, tls-crypt and the like) and its
	// inline certificates, one directive or <block> per line. Import keeps
	// only allowlisted directives, but sync and restore carry any text, so
	// whatever builds a config from it checks every line again (R43).
	OpenVPN string `json:"openvpn,omitempty"`
}

// VPN tiers (R2): inside Hostbay for Hostbay traffic only, the whole
// computer through the privileged helper, or a vendor client Hostbay
// detects and routes.
const (
	VPNTierApp      = "app"
	VPNTierSystem   = "system"
	VPNTierExternal = "external"
)

// DefaultVPNMTU stops SSH stalls on tunnels that drop large packets (R28).
const DefaultVPNMTU = 1280

// HeldVPNID stands in for a host's VPN that sync changed and this computer
// has not confirmed. It names no profile, so the host cannot connect until
// the change is confirmed or the host is edited here.
const HeldVPNID = "held"

// TunnelMTU is the MTU the profile's tunnel uses.
func (v *VPN) TunnelMTU() int {
	if v.MTU == 0 {
		return DefaultVPNMTU
	}
	return v.MTU
}

// SecretAAD binds the profile's secrets to where and how they are used:
// after the profile is pointed elsewhere, signs in as someone else, moves
// tier or trusts another server key they no longer open (R4). The peer key
// is the server's identity, and a synced change to it alone raises no hold.
func (v *VPN) SecretAAD() []byte {
	return []byte("vpn\x00" + v.ID + "\x00" + v.Type + "\x00" + v.Gateway + "\x00" + v.User + "\x00" + v.Tier + "\x00" + v.PeerKey)
}

// NodeStateAAD binds a Tailscale node's sealed state (Local.VPNNodeStates)
// to its profile and tailnet: another profile, type or control server never
// opens it. Its prefix keeps it apart from SecretAAD.
func (v *VPN) NodeStateAAD() []byte {
	return []byte("vpn-node\x00" + v.ID + "\x00" + v.Type + "\x00" + v.Gateway)
}

// VPN returns the profile with id, or nil.
func (d *Data) VPN(id string) *VPN {
	if i := find(d.VPNs.Items, id); i >= 0 {
		return &d.VPNs.Items[i]
	}
	return nil
}

// PutVPN saves v as a local change and returns it with its new Meta.
func (d *Data) PutVPN(v VPN) VPN { return put(d, &d.VPNs, v) }

// AddVPN adds v, keeping any Meta it carries.
func (d *Data) AddVPN(v VPN) VPN { return add(d, &d.VPNs, v) }

// PurgeVPN removes a profile, leaving a tombstone for sync. Hosts that
// named it fail to connect until they name another.
func (d *Data) PurgeVPN(id string) error {
	if !purge(d, &d.VPNs, id) {
		return errors.New("VPN profile not found")
	}
	return nil
}

// AutoApplies reports whether the profile's routes apply as it connects. A
// profile that never chose follows its tier: an external client installs its
// own routes, often a full tunnel, and only Hostbay's plan can undo them.
func (v VPN) AutoApplies() bool {
	if v.AutoApply != nil {
		return *v.AutoApply
	}
	return v.Tier == VPNTierExternal
}
