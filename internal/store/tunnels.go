package store

import "errors"

// Tunnel is a saved port forward on a host (R23). Kind is "local" (-L),
// "remote" (-R) or "dynamic" (-D, SOCKS). For local and dynamic tunnels
// Bind is on this computer and Target is dialled from the host; for remote
// tunnels Bind is on the host and Target is dialled from this computer.
type Tunnel struct {
	Meta
	HostID     string `json:"hostId"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	BindHost   string `json:"bindHost,omitempty"` // "" means 127.0.0.1
	BindPort   int    `json:"bindPort"`
	TargetHost string `json:"targetHost,omitempty"`
	TargetPort int    `json:"targetPort,omitempty"`
	// AllowExternal must be set to listen beyond loopback (R33).
	AllowExternal bool `json:"allowExternal,omitempty"`
}

// Tunnel returns the tunnel with id, or nil.
func (d *Data) Tunnel(id string) *Tunnel {
	if i := find(d.Tunnels.Items, id); i >= 0 {
		return &d.Tunnels.Items[i]
	}
	return nil
}

// PutTunnel saves t as a local change and returns it with its new Meta.
func (d *Data) PutTunnel(t Tunnel) Tunnel { return put(d, &d.Tunnels, t) }

// PurgeTunnel removes a tunnel, leaving a tombstone for sync.
func (d *Data) PurgeTunnel(id string) error {
	if !purge(d, &d.Tunnels, id) {
		return errors.New("tunnel not found")
	}
	return nil
}
