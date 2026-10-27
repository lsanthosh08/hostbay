package store

import (
	"errors"
	"strconv"
)

// Proxy is a SOCKS5 or HTTP CONNECT proxy that hosts dial through (R49).
// Proxies are their own collection so sync carries their one sealed field,
// Password, bound to SecretAAD like a host's (KTD6, KTD22). A host names
// its proxy by ID; a missing proxy is an error, never a direct connection.
type Proxy struct {
	Meta
	Name     string `json:"name"`
	Type     string `json:"type"` // "socks5" or "http"
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user,omitempty"`
	Password string `json:"password,omitempty"`
}

// HeldProxyID stands in for a host's proxy that sync changed and this
// computer has not confirmed. It names no proxy, so the host cannot connect
// until the change is confirmed or the host is edited here (KTD22).
const HeldProxyID = "held"

// ProxyTypes are the proxy types Hostbay dials through.
var ProxyTypes = []string{"socks5", "http"}

// SecretAAD binds the proxy's password to where it may be sent: after the
// proxy is pointed elsewhere it no longer opens.
func (p *Proxy) SecretAAD() []byte {
	return []byte("proxy\x00" + p.ID + "\x00" + p.Type + "\x00" + p.Host + "\x00" + strconv.Itoa(p.Port) + "\x00" + p.User)
}

// Proxy returns the proxy with id, or nil.
func (d *Data) Proxy(id string) *Proxy {
	if i := find(d.Proxies.Items, id); i >= 0 {
		return &d.Proxies.Items[i]
	}
	return nil
}

// PutProxy saves p as a local change and returns it with its new Meta.
func (d *Data) PutProxy(p Proxy) Proxy { return put(d, &d.Proxies, p) }

// AddProxy adds p, keeping any Meta it carries.
func (d *Data) AddProxy(p Proxy) Proxy { return add(d, &d.Proxies, p) }

// PurgeProxy removes a proxy, leaving a tombstone for sync. Hosts that
// named it fail to connect until they name another.
func (d *Data) PurgeProxy(id string) error {
	if !purge(d, &d.Proxies, id) {
		return errors.New("proxy not found")
	}
	return nil
}
