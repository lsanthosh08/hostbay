package store

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Host is a saved connection. Password is a sealed secrets field
// ("enc:v1:..."), bound to SecretAAD; it is never plaintext on disk.
type Host struct {
	Meta
	Alias         string   `json:"alias"`
	Hostname      string   `json:"hostname"`
	Port          int      `json:"port"`
	User          string   `json:"user"`
	IdentityFile  string   `json:"identityFile"` // a path, or "keystore:<name>" for a key in the key store
	Tags          []string `json:"tags"`
	LastConnected string   `json:"lastConnected,omitempty"`
	FolderID      *string  `json:"folderId,omitempty"`
	Order         int      `json:"order,omitempty"`
	DeletedAt     string   `json:"deletedAt,omitempty"` // set while the host is in trash
	AuthType      *string  `json:"authType,omitempty"`
	Password      string   `json:"password,omitempty"`
	Kind          *string  `json:"kind,omitempty"` // nil or "ssh", "rdp", "telnet", "mosh", "tcp", "ftp", "smb", "webdav", "s3", "vnc", "spice", "postgres", "sqlite", "kubernetes", "redis", "mongodb", "elasticsearch", "mysql", "mssql", "oracle", "rqlite"
	RdpPort       *int     `json:"rdpPort,omitempty"`
	RdpSecurity   *string  `json:"rdpSecurity,omitempty"`
	Mac           *string  `json:"mac,omitempty"`
	JumpHostID    *string  `json:"jumpHostId,omitempty"` // the host this one is reached through

	// File-only kinds (R39–R42). User and Password are the FTP, SMB and
	// WebDAV sign-in, or S3's access key ID and secret key.
	TLS       *string `json:"tls,omitempty"`       // ftp: "explicit" (nil), "implicit" or "none" (unencrypted); vnc: "vencrypt" (nil) or "none"; spice: "tls" (nil) or "none"; database kinds: "verify" (nil) or "none"
	URL       *string `json:"url,omitempty"`       // webdav: the folder's URL; s3 and elasticsearch: the endpoint; rqlite: the node. http:// is unencrypted
	Domain    *string `json:"domain,omitempty"`    // smb: the NTLM domain
	Bucket    *string `json:"bucket,omitempty"`    // s3
	Region    *string `json:"region,omitempty"`    // s3: us-east-1 when unset
	PathStyle *bool   `json:"pathStyle,omitempty"` // s3: endpoint/bucket/key rather than bucket.endpoint/key

	// Connection extras (R48–R52). AuthType may also be "agent" or
	// "kerberos" for SSH.
	ProxyID      *string     `json:"proxyId,omitempty"`      // the Proxy every dial goes through; a missing one is an error
	VPNID        *string     `json:"vpnId,omitempty"`        // the VPN profile every dial goes through; a missing one is an error
	ForwardAgent *bool       `json:"forwardAgent,omitempty"` // ssh: the host may use this computer's agent while connected
	Algorithms   *Algorithms `json:"algorithms,omitempty"`   // ssh: legacy algorithms offered after the defaults
	Keepalive    *int        `json:"keepalive,omitempty"`    // ssh: seconds between keepalives; 0 or nil means 30
	LocalEcho    *bool       `json:"localEcho,omitempty"`    // tcp: show typed text locally

	// X11 (R47). Forwarding lets the host see and control this computer's
	// display while connected, so it is off unless set.
	X11        *bool   `json:"x11,omitempty"`        // ssh: forward X11 programs to this computer's X server
	X11Desktop *string `json:"x11Desktop,omitempty"` // ssh: what an X11 desktop runs; unset runs the first of startxfce4, mate-session or gnome-session

	// Database kinds (R56, KTD24). User and Password are the database
	// sign-in; SQLite has neither.
	Database *string `json:"database,omitempty"` // postgres, mysql, mssql: the database to open; oracle: the service name; redis: its number; mongodb: the auth database
	File     *string `json:"file,omitempty"`     // sqlite: the absolute path of a file on this computer

	// Kubernetes (R57, KTD28), filled from a kubeconfig context. URL is the
	// API server, Hostname and Port are taken from it, Password is the
	// sealed bearer token and IdentityFile a key-store entry holding the
	// client certificate and key. CAData, when set, is the only trust anchor.
	CAData        *string `json:"caData,omitempty"`        // PEM
	TLSServerName *string `json:"tlsServerName,omitempty"` // the name checked in the server's certificate, when not the URL's host
	Namespace     *string `json:"namespace,omitempty"`     // the namespace a tab opens on
}

// Algorithms are SSH algorithms a host opts in to on top of the defaults,
// for servers that offer nothing newer (R52).
type Algorithms struct {
	KeyExchanges []string `json:"kex,omitempty"`
	Ciphers      []string `json:"ciphers,omitempty"`
	MACs         []string `json:"macs,omitempty"`
	HostKeys     []string `json:"hostKeys,omitempty"`
}

// Names lists every algorithm a, in order: key exchanges, ciphers, MACs,
// host keys.
func (a *Algorithms) Names() []string {
	if a == nil {
		return nil
	}
	return slices.Concat(a.KeyExchanges, a.Ciphers, a.MACs, a.HostKeys)
}

// Forwards reports a host set to forward this computer's SSH agent.
func (h *Host) Forwards() bool { return h.ForwardAgent != nil && *h.ForwardAgent }

// ForwardsX11 reports a host set to forward X11 to this computer.
func (h *Host) ForwardsX11() bool { return h.X11 != nil && *h.X11 }

// Jumped reports a host reached through a jump host.
func (h *Host) Jumped() bool { return h.JumpHostID != nil && *h.JumpHostID != "" }

// Proxied reports a host that names a proxy.
func (h *Host) Proxied() bool { return h.ProxyID != nil && *h.ProxyID != "" }

// UsesVPN reports a host that names a VPN profile.
func (h *Host) UsesVPN() bool { return h.VPNID != nil && *h.VPNID != "" }

// MissingProxy stands in for a proxy or jump host an import names but does
// not carry. It names no saved proxy, so the host fails to connect until one
// is chosen, never connecting around it (KTD22, KTD24).
func MissingProxy() *string {
	id := "import-missing-" + NewID()
	return &id
}

// MissingVPN stands in for a VPN profile an import or backup names but
// does not carry, like MissingProxy.
func MissingVPN() *string { return MissingProxy() }

// FileKinds are the kinds that only browse and transfer files.
var FileKinds = []string{"ftp", "smb", "webdav", "s3"}

// DatabaseKinds are the kinds that open a database tab. They are not file
// kinds, so they may be reached through a jump host (KTD24).
var DatabaseKinds = []string{"postgres", "sqlite", "redis", "mongodb", "elasticsearch", "mysql", "mssql", "oracle", "rqlite"}

// IsDatabase reports a host of a database kind.
func (h *Host) IsDatabase() bool { return slices.Contains(DatabaseKinds, h.KindName()) }

// KindName is the host's kind, "ssh" when unset.
func (h *Host) KindName() string {
	if h.Kind == nil || *h.Kind == "" {
		return "ssh"
	}
	return *h.Kind
}

// FTPTLS is an FTP host's TLS mode: "explicit", "implicit" or "none".
func (h *Host) FTPTLS() string { return h.tlsOr("explicit") }

// VNCSecurity is a VNC host's security mode: "vencrypt" or "none" (KTD18).
func (h *Host) VNCSecurity() string { return h.tlsOr("vencrypt") }

// SpiceSecurity is a SPICE host's security mode: "tls", the port is a
// SPICE tls-port, or "none" (KTD18).
func (h *Host) SpiceSecurity() string { return h.tlsOr("tls") }

// DatabaseTLS is a database host's TLS mode: "verify" or "none" (KTD25).
func (h *Host) DatabaseTLS() string { return h.tlsOr("verify") }

// tlsOr is the host's TLS field, or def when unset.
func (h *Host) tlsOr(def string) string {
	if h.TLS == nil || *h.TLS == "" {
		return def
	}
	return *h.TLS
}

// Plaintext reports a host set to connect unencrypted: plain FTP, a WebDAV
// or S3 http:// URL, plain VNC or SPICE that is not carried over a jump
// host's SSH connection (KTD18), or a database host set to TLS "none" or
// an http:// Elasticsearch URL. The
// hop from a jump host to a database is cleartext too, so only a database
// on the jump host itself (a loopback Hostname) escapes that (KTD25).
func (h *Host) Plaintext() bool {
	switch h.KindName() {
	case "sqlite":
		return false
	case "postgres", "redis", "mongodb", "mysql", "mssql", "oracle":
		return h.DatabaseTLS() == "none" && !(h.Jumped() && isLoopback(h.Hostname))
	case "elasticsearch", "rqlite": // the URL says how it connects; http:// is its "none"
		u, err := url.Parse(Deref(h.URL))
		return err == nil && u.Scheme == "http" && !(h.Jumped() && isLoopback(u.Hostname()))
	case "ftp":
		return h.FTPTLS() == "none"
	case "vnc":
		return h.VNCSecurity() == "none" && !h.Jumped()
	case "spice":
		return h.SpiceSecurity() == "none" && !h.Jumped()
	case "webdav", "s3":
		return h.URL != nil && strings.HasPrefix(*h.URL, "http://")
	}
	return false
}

// SecretAAD binds a host's sealed secrets to where they may be sent (KTD6):
// after the host is pointed elsewhere they no longer open. File kinds also
// bind their kind and how they connect (TLS mode, URL with its scheme,
// domain), so a password saved for FTPS does not open for plain FTP, nor
// one saved for VeNCrypt or a SPICE TLS port for plain VNC or SPICE
// (KTD18, KTD22), a database host its kind and TLS mode, or its URL for
// Elasticsearch (KTD25), and a Kubernetes host its kind and server URL
// (KTD28).
// Existing kinds keep the binding they always had, so
// passwords already sealed still open.
func (h *Host) SecretAAD() []byte {
	aad := h.ID + "\x00" + h.Hostname + "\x00" + strconv.Itoa(h.Port) + "\x00" + h.User
	switch k := h.KindName(); k {
	case "ftp":
		aad += "\x00" + k + "\x00" + h.FTPTLS()
	case "webdav", "s3":
		aad += "\x00" + k + "\x00" + Deref(h.URL)
	case "smb":
		aad += "\x00" + k + "\x00" + Deref(h.Domain)
	case "vnc":
		aad += "\x00" + k + "\x00" + h.VNCSecurity()
	case "spice":
		aad += "\x00" + k + "\x00" + h.SpiceSecurity()
	case "postgres", "sqlite", "redis", "mongodb", "mysql", "mssql", "oracle":
		aad += "\x00" + k + "\x00" + h.DatabaseTLS()
	case "kubernetes", "elasticsearch", "rqlite":
		aad += "\x00" + k + "\x00" + Deref(h.URL)
	}
	return []byte(aad)
}

// isLoopback reports localhost or a loopback address.
func isLoopback(host string) bool {
	ip := net.ParseIP(host)
	return strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback())
}

// Deref is *s, or "" when s is nil.
func Deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Tombstone records a purged item so sync can propagate the purge.
type Tombstone struct {
	Meta
	Deleted bool `json:"deleted"`
}

// Collection is one document of records plus the tombstones of purged ones.
type Collection[T any] struct {
	Items      []T         `json:"items"`
	Tombstones []Tombstone `json:"tombstones,omitempty"`
}

func (m *Meta) meta() *Meta { return m }

type record[T any] interface {
	*T
	meta() *Meta
}

func find[T any, P record[T]](items []T, id string) int {
	for i := range items {
		if P(&items[i]).meta().ID == id {
			return i
		}
	}
	return -1
}

// Data is every host-management document. Change it only inside Store.Edit,
// which saves what changed.
type Data struct {
	Hosts   Collection[Host]
	Folders Collection[Folder]
	Keys    Collection[Key]
	// Scripts and Bookmarks have nil Items until the user first saves them;
	// ScriptList and BookmarkPaths show the defaults until then.
	Scripts   Collection[Script]
	Bookmarks Collection[Bookmark]
	Tunnels   Collection[Tunnel]
	Proxies   Collection[Proxy]
	// VPNs carry one sealed secret each (KTD4).
	VPNs      Collection[VPN]
	Settings  Collection[SettingsGroup]
	PostLogin Collection[PostLoginRule]
	// Saved split-pane layouts (R65).
	Workspaces Collection[Workspace]
	// AIProviders carry a sealed API key each (KTD31).
	AIProviders Collection[AIProvider]
	// Skills and prompt templates (R61).
	Skills Collection[Skill]

	st *Store
}

func (d *Data) docs() []struct {
	name string
	v    any
} {
	return []struct {
		name string
		v    any
	}{
		{"hosts.json", &d.Hosts},
		{"folders.json", &d.Folders},
		{"keys.json", &d.Keys},
		{"scripts.json", &d.Scripts},
		{"bookmarks.json", &d.Bookmarks},
		{"tunnels.json", &d.Tunnels},
		{"proxies.json", &d.Proxies},
		{"vpns.json", &d.VPNs},
		{"settings.json", &d.Settings},
		{"postlogin.json", &d.PostLogin},
		{"workspaces.json", &d.Workspaces},
		{"aiproviders.json", &d.AIProviders},
		{"skills.json", &d.Skills},
	}
}

// NewID returns a fresh record ID. Callers that seal a secret bound to the
// ID need it before the record is added.
func NewID() string { return rand.Text() }

// editMu serialises Edit; the lock file already keeps a store to one process.
var editMu sync.Mutex

// Data loads every document.
func (s *Store) Data() (*Data, error) {
	d := &Data{st: s}
	for _, doc := range d.docs() {
		raw, err := Load[json.RawMessage](s, doc.name)
		if err != nil {
			return nil, err
		}
		if raw != nil {
			if err := json.Unmarshal(raw, doc.v); err != nil {
				return nil, fmt.Errorf("%s: %w", doc.name, err)
			}
		}
	}
	return d, nil
}

// Edit loads the documents, runs fn and saves each document fn changed. If
// fn fails nothing is saved.
func (s *Store) Edit(fn func(d *Data) error) error {
	editMu.Lock()
	defer editMu.Unlock()
	d, err := s.Data()
	if err != nil {
		return err
	}
	var before [][]byte
	for _, doc := range d.docs() {
		b, err := json.Marshal(doc.v)
		if err != nil {
			return err
		}
		before = append(before, b)
	}
	if err := fn(d); err != nil {
		return err
	}
	for i, doc := range d.docs() {
		b, err := json.Marshal(doc.v)
		if err != nil {
			return err
		}
		if !bytes.Equal(b, before[i]) {
			if err := Save[any](s, doc.name, doc.v); err != nil {
				return err
			}
		}
	}
	return nil
}

// add appends item, giving it an ID if it has none. An item that carries no
// revision yet is touched; one that has one (from a backup) keeps its Meta,
// so an old backup never wins a later sync (KTD12). A tombstone for the same
// ID is dropped, since the item is back.
func add[T any, P record[T]](d *Data, c *Collection[T], item T) T {
	m := P(&item).meta()
	if m.ID == "" {
		m.ID = NewID()
	}
	if m.Revision == 0 {
		d.st.Touch(m)
	}
	if i := find(c.Tombstones, m.ID); i >= 0 {
		c.Tombstones = append(c.Tombstones[:i], c.Tombstones[i+1:]...)
	}
	c.Items = append(c.Items, item)
	return item
}

// put replaces the item with the same ID, or adds it, and touches it.
func put[T any, P record[T]](d *Data, c *Collection[T], item T) T {
	m := P(&item).meta()
	if m.ID == "" {
		m.ID = NewID()
	}
	d.st.Touch(m)
	if i := find[T, P](c.Items, m.ID); i >= 0 {
		c.Items[i] = item
		return item
	}
	return add[T, P](d, c, item)
}

// purge removes the item with id and leaves a tombstone one revision past it.
func purge[T any, P record[T]](d *Data, c *Collection[T], id string) bool {
	i := find[T, P](c.Items, id)
	if i < 0 {
		return false
	}
	m := *P(&c.Items[i]).meta()
	d.st.Touch(&m)
	c.Tombstones = append(c.Tombstones, Tombstone{Meta: m, Deleted: true})
	c.Items = append(c.Items[:i], c.Items[i+1:]...)
	return true
}

var errNoHost = errors.New("host not found")

// Host returns the host with id, in trash or not, or nil.
func (d *Data) Host(id string) *Host {
	if i := find(d.Hosts.Items, id); i >= 0 {
		return &d.Hosts.Items[i]
	}
	return nil
}

// PutHost saves h as a local change and returns it with its new Meta.
func (d *Data) PutHost(h Host) Host { return put(d, &d.Hosts, h) }

// AddHost adds h, keeping any Meta it carries.
func (d *Data) AddHost(h Host) Host { return add(d, &d.Hosts, h) }

// EditHost changes the host with id through fn and records the change.
func (d *Data) EditHost(id string, fn func(h *Host)) error {
	h := d.Host(id)
	if h == nil {
		return errNoHost
	}
	fn(h)
	d.st.Touch(&h.Meta)
	return nil
}

// SetLastConnected records when this computer last connected to the host
// (Unix milliseconds, or "" to clear it). It is bookkeeping, not an edit, so
// it leaves Meta alone: a connect neither opens a fresh connection (the key
// follows the revision) nor wins a sync merge over another computer's edit.
func (d *Data) SetLastConnected(id, at string) error {
	h := d.Host(id)
	if h == nil {
		return errNoHost
	}
	h.LastConnected = at
	return nil
}

// TrashHost moves a host to trash, from where it can be restored.
func (d *Data) TrashHost(id string) error {
	return d.EditHost(id, func(h *Host) { h.DeletedAt = time.Now().UTC().Format(time.RFC3339) })
}

// RestoreHost takes a host out of trash.
func (d *Data) RestoreHost(id string) error {
	return d.EditHost(id, func(h *Host) { h.DeletedAt = "" })
}

// PurgeHost removes a host for good, leaving a tombstone.
func (d *Data) PurgeHost(id string) error {
	if !purge(d, &d.Hosts, id) {
		return errNoHost
	}
	return nil
}

// CloneHost adds a copy of the host with id as "<alias> copy". The copy has
// a new ID, so the original's sealed password would not open on it; it is
// left out, and the caller re-seals it under the copy's SecretAAD.
func (d *Data) CloneHost(id string) (Host, error) {
	h := d.Host(id)
	if h == nil {
		return Host{}, errNoHost
	}
	c := *h
	c.Meta = Meta{}
	c.Alias += " copy"
	c.Password = ""
	c.Tags = append([]string(nil), h.Tags...)
	return d.AddHost(c), nil
}
