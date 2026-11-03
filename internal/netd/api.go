// Package netd is hostbay-netd, Hostbay's privileged helper (KTD8), and the
// client Hostbay uses to reach it.
//
// The helper runs as root (a launchd daemon, a systemd unit or a Windows
// service) and serves one local socket that only the installing user's
// Hostbay may use (auth.go). Messages are newline-delimited JSON tagged by
// "t", the convention of the hostbay-vpn helper (internal/vpn/proto): the
// helper opens with hello, Hostbay sends typed requests and the helper
// answers each with a reply carrying the same id, and streams event whenever
// the system route table changes. Every message carries the protocol version.
//
// The helper does only what KTD8 names, each argument validated here and
// each route kept within bounds it checks itself (server.go): add or delete a
// route through a tunnel interface or the local default gateway, set a tunnel
// interface's MTU, start or stop a system-wide tunnel: OpenVPN
// from a configuration it writes itself, OpenConnect on a Wintun adapter on
// Windows, or WireGuard on a real TUN, and point the system's automatic
// proxy at Hostbay's loopback PAC. Every route it adds, every vendor route
// it deletes and every proxy setting it changes is journaled first and
// swept back when Hostbay disconnects or the helper restarts (journal.go);
// every tunnel stops then too.
package netd

import (
	"net/netip"
	"regexp"
	"strconv"

	"hostbay/internal/vpn/openvpn"
)

// Version is bumped on any incompatible change to the messages below, or to
// what the helper accepts in them (3: OpenVPN profiles need a CA or peer
// fingerprint, and peer-fingerprint is allowed; 4: static-challenge is
// allowed, for profiles that ask for a one-time code; 5: the automatic proxy
// can be set and cleared). A helper and a Hostbay that
// disagree refuse each other with CodeVersion, and Hostbay offers to
// reinstall the helper.
const Version = 5

// Message tags.
const (
	Hello  = "hello"  // helper: opens the connection
	Add    = "add"    // Hostbay: ID, Route
	Delete = "delete" // Hostbay: ID, Route
	SetMTU = "mtu"    // Hostbay: ID, Iface, MTU
	Reply  = "reply"  // helper: ID, and Code and Reason on failure
	Event  = "event"  // helper: the system route table changed

	// System-wide tunnels (U12). Each start names a handle Hostbay chose in
	// Tunnel; the helper's TunnelEvent messages and Hostbay's StopTunnel
	// name it too. A helper built without tunnels answers CodeUnsupported.
	OpenVPNStart     = "openvpn-start"     // Hostbay: Tunnel, OpenVPN; reply: Mgmt, MgmtPass
	OpenConnectStart = "openconnect-start" // Hostbay: Tunnel, OpenConnect (Windows)
	WireGuardStart   = "wireguard-start"   // Hostbay: Tunnel, WireGuard
	StopTunnel       = "tunnel-stop"       // Hostbay: Tunnel
	TunnelEvent      = "tunnel"            // helper: Tunnel, State, and Iface and Address when up, Reason when ended

	// The system's automatic proxy: SetProxy points every enabled network
	// service at URL, Hostbay's PAC; ClearProxy puts back what each had.
	SetProxy   = "proxy-set"   // Hostbay: URL
	ClearProxy = "proxy-clear" // Hostbay
)

// Tunnel event states.
const (
	TunnelUp    = "up"
	TunnelEnded = "ended"
)

// Reply codes. An empty code is success.
const (
	CodeVersion     = "version"     // the two sides speak different protocol versions
	CodeDenied      = "denied"      // the caller is not the paired Hostbay
	CodeBusy        = "busy"        // another Hostbay connection is open
	CodeInvalid     = "invalid"     // the request is malformed
	CodeRefused     = "refused"     // the request is well formed but outside the helper's bounds
	CodeFailed      = "failed"      // the system refused the change
	CodeUnsupported = "unsupported" // this helper does not perform that operation

	// Returned by Install and Uninstall, never by the helper.
	CodeCancelled   = "cancelled"   // the user dismissed the admin prompt
	CodeUnavailable = "unavailable" // this build cannot install the helper
)

// MTU bounds: 576 is the IPv4 minimum every host must accept; 9000 is a
// jumbo frame.
const (
	MinMTU = 576
	MaxMTU = 9000
)

// Error is a coded failure from the helper or from installing it. Reason is
// safe to show.
type Error struct {
	Code   string
	Reason string
}

func (e *Error) Error() string { return e.Reason }

// Route names a route in a request. Dst is a CIDR with no host bits. An add
// goes either through Iface, which must be a tunnel interface, or, with
// Gateway set, through the local default gateway, which the helper finds in
// the route table itself rather than taking an address from the caller. A
// delete with Gateway set removes the route Hostbay added that way.
type Route struct {
	Dst     string `json:"dst"`
	Iface   string `json:"iface,omitempty"`
	Gateway bool   `json:"gateway,omitempty"`
}

// Msg is any message in either direction.
type Msg struct {
	T string `json:"t"`
	V int    `json:"v"`
	// Build is the helper's own executable's SHA-256, in its hello: a helper
	// copied from an older Hostbay build speaks the same protocol but not the
	// same code, which only this tells apart.
	Build string `json:"build,omitempty"`
	// ID pairs a reply with its request; events carry none.
	ID uint64 `json:"id,omitempty"`

	Route *Route `json:"route,omitempty"`
	Iface string `json:"iface,omitempty"`
	MTU   int    `json:"mtu,omitempty"`
	URL   string `json:"url,omitempty"` // SetProxy: the PAC

	Tunnel      string           `json:"tunnel,omitempty"`
	OpenVPN     *openvpn.Profile `json:"openvpn,omitempty"`
	OpenConnect *OpenConnect     `json:"openconnect,omitempty"`
	WireGuard   *WireGuard       `json:"wireguard,omitempty"`
	// An OpenVPN start's reply: its management interface, a Unix socket
	// path, or on Windows a loopback host:port and its password.
	Mgmt     string `json:"mgmt,omitempty"`
	MgmtPass string `json:"mgmtPass,omitempty"`
	// A tunnel event's state, and the tunnel's own addresses when up.
	State   string   `json:"state,omitempty"`
	Address []string `json:"address,omitempty"`

	Code   string `json:"code,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// OpenConnect is a system-wide OpenConnect run (Windows): the hostbay-vpn
// start message's type, Config and password, checked the same way.
type OpenConnect struct {
	Type     string `json:"type"`
	Config   string `json:"config"`
	Password string `json:"password,omitempty"`
}

// WireGuard is a system-wide WireGuard tunnel: a wg-quick .conf without its
// keys, which travel beside it.
type WireGuard struct {
	Conf         string `json:"conf"`
	PrivateKey   string `json:"privateKey,omitempty"`
	PresharedKey string `json:"presharedKey,omitempty"`
}

// tunnelHandle admits the handles Hostbay chooses: they name files in the
// helper's directory.
var tunnelHandle = regexp.MustCompile(`^[A-Za-z0-9]{8,32}$`)

// ifaceName admits interface names as each system spells them, Windows
// friendly names with spaces included, and nothing a shell or an option
// parser would act on: no leading dash, no quotes, separators or control
// characters. The helper also requires the interface to exist.
var ifaceName = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N} ._()#*-]{0,127}$`)

// pacURL admits only Hostbay's own PAC on loopback: a fixed path with the
// token Hostbay chose and a version, which the sweep also recognizes as
// Hostbay's whatever its port or version.
var pacURL = regexp.MustCompile(`^http://127\.0\.0\.1:([1-9][0-9]{0,4})/hostbay-pac/[A-Za-z0-9_-]{16,64}/[0-9]{1,10}\.pac$`)

// ownPAC reports whether url is a PAC URL Hostbay sets; the app checks the
// URLs it builds against it as IsPACURL.
func ownPAC(url string) bool {
	m := pacURL.FindStringSubmatch(url)
	if m == nil {
		return false
	}
	port, _ := strconv.Atoi(m[1])
	return port <= 65535
}

// IsPACURL reports whether SetProxy accepts url.
func IsPACURL(url string) bool { return ownPAC(url) }

// parseCIDR parses a destination strictly: canonical form, no host bits, no
// zone, no IPv4-mapped IPv6.
func parseCIDR(s string) (netip.Prefix, *Error) {
	p, err := netip.ParsePrefix(s)
	if err != nil || p != p.Masked() || p.Addr().Is4In6() || p.String() != s {
		return netip.Prefix{}, invalid("%q is not a network in CIDR form.", s)
	}
	return p, nil
}

func validIface(s string) *Error {
	if !ifaceName.MatchString(s) {
		return invalid("%q is not an interface name.", s)
	}
	return nil
}

// validate checks a request's arguments before anything else looks at it.
func validate(m Msg) *Error {
	switch m.T {
	case Add, Delete:
		r := m.Route
		if r == nil {
			return invalid("The request names no route.")
		}
		if _, err := parseCIDR(r.Dst); err != nil {
			return err
		}
		if r.Gateway == (r.Iface != "") {
			return invalid("A route goes through either an interface or the default gateway.")
		}
		if r.Iface != "" {
			return validIface(r.Iface)
		}
	case SetMTU:
		if err := validIface(m.Iface); err != nil {
			return err
		}
		if m.MTU < MinMTU || m.MTU > MaxMTU {
			return invalid("MTU %d is outside %d to %d.", m.MTU, MinMTU, MaxMTU)
		}
	case OpenVPNStart, OpenConnectStart, WireGuardStart, StopTunnel:
		if !tunnelHandle.MatchString(m.Tunnel) {
			return invalid("%q is not a tunnel handle.", m.Tunnel)
		}
		sent := map[string]bool{OpenVPNStart: m.OpenVPN != nil, OpenConnectStart: m.OpenConnect != nil, WireGuardStart: m.WireGuard != nil}
		for t, set := range sent {
			if set != (t == m.T) {
				return invalid("The request carries the wrong tunnel settings.")
			}
		}
	case SetProxy:
		if !ownPAC(m.URL) {
			return invalid("%q is not Hostbay's proxy configuration address.", m.URL)
		}
	case ClearProxy:
	default:
		return invalid("Unknown request %q.", m.T)
	}
	return nil
}
