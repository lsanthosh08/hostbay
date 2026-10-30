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
// interface's MTU. Every route it adds and every vendor route it deletes is
// journaled first and swept back when Hostbay disconnects or the helper
// restarts (journal.go).
package netd

import (
	"net/netip"
	"regexp"
)

// Version is bumped on any incompatible change to the messages below. A
// helper and a Hostbay that disagree refuse each other with CodeVersion, and
// Hostbay offers to reinstall the helper.
const Version = 1

// Message tags.
const (
	Hello  = "hello"  // helper: opens the connection
	Add    = "add"    // Hostbay: ID, Route
	Delete = "delete" // Hostbay: ID, Route
	SetMTU = "mtu"    // Hostbay: ID, Iface, MTU
	Reply  = "reply"  // helper: ID, and Code and Reason on failure
	Event  = "event"  // helper: the system route table changed

	// Reserved for system-wide profiles (U12). Today's helper answers them
	// with CodeUnsupported.
	OpenVPNStart     = "openvpn-start"
	OpenVPNStop      = "openvpn-stop"
	OpenConnectStart = "openconnect-start"
	OpenConnectStop  = "openconnect-stop"
	WireGuardStart   = "wireguard-start"
	WireGuardStop    = "wireguard-stop"
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
	// ID pairs a reply with its request; events carry none.
	ID uint64 `json:"id,omitempty"`

	Route *Route `json:"route,omitempty"`
	Iface string `json:"iface,omitempty"`
	MTU   int    `json:"mtu,omitempty"`

	Code   string `json:"code,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// ifaceName admits interface names as each system spells them, Windows
// friendly names with spaces included, and nothing a shell or an option
// parser would act on: no leading dash, no quotes, separators or control
// characters. The helper also requires the interface to exist.
var ifaceName = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N} ._()#*-]{0,127}$`)

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
	default:
		return invalid("Unknown request %q.", m.T)
	}
	return nil
}
