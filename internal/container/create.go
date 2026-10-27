package container

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"

	"hostbay/internal/admin"
)

// CreateOptions is the create form (KTD27): no privileged, host-namespace,
// capability or device options exist, so none can be asked for.
type CreateOptions struct {
	Image   string        `json:"image"`
	Name    string        `json:"name"`
	Ports   []PortOption  `json:"ports"`
	Env     []string      `json:"env"`     // KEY=value
	Volumes []MountOption `json:"volumes"` // Source is a named volume
	Binds   []MountOption `json:"binds"`   // Source is a folder of the host
	Restart string        `json:"restart"` // "", no, always, unless-stopped, on-failure
}

// PortOption publishes ContainerPort on HostAddress:HostPort. An empty
// address is 127.0.0.1, so a port is never opened wider by default.
type PortOption struct {
	HostAddress   string `json:"hostAddress"`
	HostPort      int    `json:"hostPort"`
	ContainerPort int    `json:"containerPort"`
	Protocol      string `json:"protocol"` // tcp (the default) or udp
}

type MountOption struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"readOnly"`
}

var envKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// volumeName is Docker's own volume-name rule. The engines take a source
// starting with '.' as a host path, which would bind a folder unconfirmed.
var volumeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// CreateArgs checks o and builds the run command, every value in its own
// argument and the image after "--".
func (e *Engine) CreateArgs(o CreateOptions) ([]string, error) {
	if err := validImage(o.Image); err != nil {
		return nil, err
	}
	args := []string{"run", "-d"}
	if o.Name != "" {
		if err := admin.ValidToken(o.Name, "_.-"); err != nil {
			return nil, err
		}
		args = append(args, "--name", o.Name)
	}
	for _, p := range o.Ports {
		v, err := p.flag()
		if err != nil {
			return nil, err
		}
		args = append(args, "-p", v)
	}
	for _, kv := range o.Env {
		// A bare NAME would copy the engine CLI's own variable (Hostbay's
		// environment on this computer) into the container.
		k, _, ok := strings.Cut(kv, "=")
		if !ok || !envKey.MatchString(k) || strings.ContainsAny(kv, "\x00\r\n") {
			return nil, fmt.Errorf("invalid environment variable: %q", kv)
		}
		args = append(args, "-e", kv)
	}
	for _, m := range o.Volumes {
		if !volumeName.MatchString(m.Source) {
			return nil, fmt.Errorf("invalid volume name: %q", m.Source)
		}
		v, err := m.flag()
		if err != nil {
			return nil, err
		}
		args = append(args, "-v", v)
	}
	for _, m := range o.Binds {
		if !hostPath(m.Source) {
			return nil, fmt.Errorf("a bind mount needs an absolute host folder: %q", m.Source)
		}
		v, err := m.flag()
		if err != nil {
			return nil, err
		}
		args = append(args, "-v", v)
	}
	switch o.Restart {
	case "", "no":
	case "always", "unless-stopped", "on-failure":
		if !e.Caps().RestartPolicy {
			return nil, fmt.Errorf("%s has no restart policy", e.Kind)
		}
		args = append(args, "--restart", o.Restart)
	default:
		return nil, fmt.Errorf("invalid restart policy: %q", o.Restart)
	}
	return e.argv(append(args, "--", o.Image)...), nil
}

// Create runs a new container and returns its ID.
func (e *Engine) Create(ctx context.Context, o CreateOptions) (string, error) {
	argv, err := e.CreateArgs(o)
	if err != nil {
		return "", err
	}
	r, err := e.Runner.Run(ctx, argv, nil)
	if err != nil {
		return "", err
	}
	if r.Code == nil || *r.Code != 0 {
		msg := strings.TrimSpace(r.Stderr)
		if msg == "" {
			msg = "create failed"
		}
		return "", errors.New(msg)
	}
	return strings.TrimSpace(r.Stdout), nil
}

// flag writes the port as addr:host:container[/udp], always with the address.
func (p PortOption) flag() (string, error) {
	addr := p.HostAddress
	if addr == "" {
		addr = "127.0.0.1"
	}
	ip := net.ParseIP(addr)
	if ip == nil {
		return "", fmt.Errorf("invalid host address: %q", addr)
	}
	if ip.To4() == nil {
		addr = "[" + ip.String() + "]"
	}
	if p.HostPort < 1 || p.HostPort > 65535 || p.ContainerPort < 1 || p.ContainerPort > 65535 {
		return "", fmt.Errorf("invalid port: %d:%d", p.HostPort, p.ContainerPort)
	}
	v := addr + ":" + strconv.Itoa(p.HostPort) + ":" + strconv.Itoa(p.ContainerPort)
	switch p.Protocol {
	case "", "tcp":
	case "udp":
		v += "/udp"
	default:
		return "", fmt.Errorf("invalid protocol: %q", p.Protocol)
	}
	return v, nil
}

// flag writes the mount as source:target[:ro]; a target is an absolute
// path in the container with no colon or comma to split it.
func (m MountOption) flag() (string, error) {
	if !strings.HasPrefix(m.Target, "/") || strings.ContainsAny(m.Target, ":,\x00\r\n") {
		return "", fmt.Errorf("invalid container path: %q", m.Target)
	}
	v := m.Source + ":" + m.Target
	if m.ReadOnly {
		v += ":ro"
	}
	return v, nil
}

// hostPath accepts an absolute folder: /srv/data, or C:\data on Windows,
// whose drive colon the engine's -v parser expects. No other colon, comma
// or line break, which would split the flag.
func hostPath(p string) bool {
	rest := p
	if len(p) > 2 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') {
		rest = p[2:]
	}
	return (strings.HasPrefix(p, "/") || rest != p) && !strings.ContainsAny(rest, ":,\x00\r\n")
}
