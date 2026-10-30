package openconnect

import (
	"context"
	"errors"
	"net"

	"hostbay/internal/vpn/proto"
)

// Engine on Windows only says the profile must run system-wide:
// openconnect's no-admin mode (--script-tun) does not exist there
// (tun-win32.c), so these types connect through the privileged helper
// (U12).
type Engine struct{ launch }

// New checks a start message as on other platforms.
func New(typ, conf string, secrets map[string]string) (*Engine, error) {
	l, err := prepare(typ, conf, secrets)
	if err != nil {
		return nil, err
	}
	return &Engine{l}, nil
}

// Run reports failed with CodeNeedsSystemWide.
func (e *Engine) Run(ctx context.Context, report func(proto.Msg)) {
	report(proto.Msg{T: proto.State, State: proto.Failed, Code: proto.CodeNeedsSystemWide,
		Reason: "On Windows, OpenConnect VPNs cannot run inside Hostbay; connect this profile system-wide."})
}

// Dial always fails: the engine never connects.
func (e *Engine) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	return nil, errors.New("the VPN is not connected")
}

// Child has no modes on Windows.
func Child(arg string) error {
	return errors.New("hostbay-vpn takes no arguments on Windows")
}
