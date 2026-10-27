package app

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"hostbay/internal/apperr"
	"hostbay/internal/sshx"
	"hostbay/internal/store"
)

func proxyToUI(p store.Proxy) Proxy {
	u := Proxy{ID: p.ID, Name: p.Name, Type: p.Type, Host: p.Host, Port: p.Port, User: p.User}
	if p.Password != "" {
		u.Password, u.HasPassword = new(string), true
	}
	return u
}

// ListProxies lists the saved proxies for Settings and the host editor.
func (s *HostService) ListProxies() ([]Proxy, error) {
	d, err := s.st.Data()
	if err != nil {
		return nil, err
	}
	out := []Proxy{}
	for _, p := range d.Proxies.Items {
		out = append(out, proxyToUI(p))
	}
	return out, nil
}

// SaveProxy adds or updates a proxy (R49). An empty or absent password
// keeps the saved one; a new one is sealed to the proxy's type, address
// and user (KTD6), so after any of those change the saved one must be
// entered again.
func (s *HostService) SaveProxy(proxy Proxy) (Proxy, error) {
	if err := checkProxy(proxy); err != nil {
		return Proxy{}, err
	}
	var out store.Proxy
	err := s.st.Edit(func(d *store.Data) error {
		p := store.Proxy{Meta: store.Meta{ID: proxy.ID}}
		if e := d.Proxy(proxy.ID); proxy.ID != "" && e != nil {
			p = *e
		} else if p.ID == "" {
			p.ID = store.NewID()
		}
		p.Name, p.Type, p.Host, p.Port, p.User = proxy.Name, proxy.Type, proxy.Host, proxy.Port, proxy.User
		if proxy.Password != nil && *proxy.Password != "" {
			sealed, err := s.vault.Seal([]byte(*proxy.Password), p.SecretAAD())
			if err != nil {
				return err
			}
			p.Password = sealed
		}
		out = d.PutProxy(p)
		return nil
	})
	return proxyToUI(out), err
}

// DeleteProxy removes a proxy, leaving a tombstone for sync. Hosts that
// used it fail to connect until they name another; none falls back to a
// direct connection.
func (s *HostService) DeleteProxy(id string) error {
	return s.st.Edit(func(d *store.Data) error { return d.PurgeProxy(id) })
}

// checkProxy refuses a proxy that could not work or whose fields could
// break the SOCKS5 or HTTP CONNECT request.
func checkProxy(p Proxy) error {
	pw := ""
	if p.Password != nil {
		pw = *p.Password
	}
	switch {
	case strings.TrimSpace(p.Name) == "":
		return errors.New("name the proxy")
	case !slices.Contains(store.ProxyTypes, p.Type):
		return errors.New("the proxy type must be socks5 or http")
	case p.Host == "" || len(p.Host) > 253 || strings.IndexFunc(p.Host, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0:
		return errors.New("enter the proxy's host name or address")
	case p.Port < 1 || p.Port > 65535:
		return errors.New("the proxy port must be between 1 and 65535")
	case len(p.User) > 255 || len(pw) > 255:
		return errors.New("the proxy user name and password are limited to 255 bytes")
	case strings.ContainsAny(p.User+pw, "\r\n\x00"):
		return errors.New("the proxy user name and password cannot hold line breaks")
	case p.Type == "http" && strings.Contains(p.User, ":"):
		return errors.New("an HTTP proxy user name cannot contain a colon")
	}
	return nil
}

// proxyFor resolves the proxy h dials through, its password unsealed, or
// nil when h names none. A proxy that is missing, waits for confirmation
// or whose password no longer opens is an error, never a direct
// connection (KTD22).
func (c *HostCredentials) proxyFor(d *store.Data, h *store.Host) (*sshx.Proxy, error) {
	if !h.Proxied() {
		return nil, nil
	}
	if *h.ProxyID == store.HeldProxyID {
		return nil, apperr.New(apperr.ProxyFailed, "This host's proxy was set on another computer and waits for your confirmation in Sync settings").
			With("step", "proxy").With("reason", "held")
	}
	if d.Proxy(*h.ProxyID) == nil {
		return nil, apperr.New(apperr.ProxyFailed, "This host's proxy no longer exists; choose another in the host's settings").
			With("step", "proxy").With("reason", "missing")
	}
	return c.proxyByID(d, *h.ProxyID)
}

// proxyByID is proxyFor for anything else that names a proxy by ID, such
// as an AI provider: missing or unopenable is an error.
func (c *HostCredentials) proxyByID(d *store.Data, id string) (*sshx.Proxy, error) {
	p := d.Proxy(id)
	if p == nil {
		return nil, apperr.New(apperr.ProxyFailed, "This proxy no longer exists; choose another in Settings").
			With("step", "proxy").With("reason", "missing")
	}
	out := &sshx.Proxy{Type: p.Type, Host: p.Host, Port: p.Port, User: p.User}
	if p.Password != "" {
		pw, err := c.vault.Open(p.Password, p.SecretAAD())
		if err != nil {
			var ae *apperr.Error
			if errors.As(err, &ae) {
				ae.With("proxyId", p.ID)
			}
			return nil, err
		}
		out.Password = string(pw)
	}
	return out, nil
}

// proxyKey names the proxy's revision in a connection key, so editing the
// proxy opens a fresh connection.
func proxyKey(d *store.Data, h *store.Host) string {
	if !h.Proxied() {
		return ""
	}
	if p := d.Proxy(*h.ProxyID); p != nil {
		return "~" + p.ID + "@" + strconv.FormatInt(p.Revision, 10)
	}
	return "~" + *h.ProxyID
}
