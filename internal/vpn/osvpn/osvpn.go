// Package osvpn connects IKEv2 and L2TP profiles through the operating
// system's own VPN service (U12): scutil --nc on macOS, rasdial on Windows,
// nmcli on Linux. The system keeps the VPN's configuration and secrets;
// Hostbay passes only the service's name, as one argument, and runs no
// shell. It needs no administrator rights, so Hostbay runs it itself.
package osvpn

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// Status is an OS VPN service's state.
type Status struct {
	Connected bool
	Iface     string // connected: the tunnel's interface
}

// commands is one system's VPN tool.
type commands struct {
	up, down, status func(name string) []string
	parse            func(name, out string) Status
}

var systems = map[string]commands{
	"darwin": {
		up:     func(n string) []string { return []string{"/usr/sbin/scutil", "--nc", "start", n} },
		down:   func(n string) []string { return []string{"/usr/sbin/scutil", "--nc", "stop", n} },
		status: func(n string) []string { return []string{"/usr/sbin/scutil", "--nc", "status", n} },
		parse: func(_, out string) Status {
			first, rest, _ := strings.Cut(out, "\n")
			if strings.TrimSpace(first) != "Connected" {
				return Status{}
			}
			st := Status{Connected: true}
			for l := range strings.Lines(rest) {
				if k, v, ok := strings.Cut(l, ":"); ok && strings.TrimSpace(k) == "InterfaceName" {
					st.Iface = strings.TrimSpace(v)
				}
			}
			return st
		},
	},
	"windows": {
		up:     func(n string) []string { return []string{"rasdial.exe", n} },
		down:   func(n string) []string { return []string{"rasdial.exe", n, "/disconnect"} },
		status: func(string) []string { return []string{"rasdial.exe"} },
		// rasdial's own words are localized, so only a line that is the
		// entry's name counts; Windows names its RAS adapter after the entry.
		parse: func(n, out string) Status {
			for l := range strings.Lines(out) {
				if strings.TrimSpace(l) == n {
					return Status{Connected: true, Iface: n}
				}
			}
			return Status{}
		},
	},
	"linux": {
		up:   func(n string) []string { return []string{"nmcli", "connection", "up", "id", n} },
		down: func(n string) []string { return []string{"nmcli", "connection", "down", "id", n} },
		status: func(n string) []string {
			return []string{"nmcli", "-t", "-f", "GENERAL.STATE,GENERAL.IP-IFACE", "connection", "show", "id", n}
		},
		parse: func(_, out string) Status {
			var st Status
			for l := range strings.Lines(out) {
				k, v, _ := strings.Cut(strings.TrimSpace(l), ":")
				switch k {
				case "GENERAL.STATE":
					st.Connected = v == "activated"
				case "GENERAL.IP-IFACE":
					st.Iface = v
				}
			}
			if !st.Connected {
				return Status{}
			}
			return st
		},
	},
}

// platform and run are the system and the command runner; tests replace
// them.
var (
	platform = runtime.GOOS
	run      = func(ctx context.Context, name string, args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
		return string(out), err
	}
)

// Connect starts the OS VPN service name. It returns once the system took
// the request; State says when the tunnel is up.
func Connect(ctx context.Context, name string) error {
	_, err := do(ctx, name, func(c commands) func(string) []string { return c.up })
	return err
}

// Disconnect stops the OS VPN service name.
func Disconnect(ctx context.Context, name string) error {
	_, err := do(ctx, name, func(c commands) func(string) []string { return c.down })
	return err
}

// State reads the OS VPN service name's state.
func State(ctx context.Context, name string) (Status, error) {
	out, err := do(ctx, name, func(c commands) func(string) []string { return c.status })
	if err != nil {
		return Status{}, err
	}
	return systems[platform].parse(name, out), nil
}

func do(ctx context.Context, name string, pick func(commands) func(string) []string) (string, error) {
	c, ok := systems[platform]
	if !ok {
		return "", errors.New("this system has no VPN service Hostbay can drive")
	}
	if name == "" || len(name) > 256 || strings.HasPrefix(name, "-") || strings.HasPrefix(name, "/") ||
		strings.ContainsFunc(name, func(r rune) bool { return r < ' ' || r == 0x7f }) {
		return "", fmt.Errorf("%q is not the name of a VPN in the system's settings", name)
	}
	argv := pick(c)(name)
	out, err := run(ctx, argv[0], argv[1:]...)
	if err != nil {
		if msg := strings.TrimSpace(out); msg != "" {
			return out, fmt.Errorf("%s: %s", name, msg)
		}
		return out, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}
