// Package importer brings connections from other tools into Hostbay's store
// (R54). Each tool's package parses its export into a backup bundle plus the
// entries it could not map, and Import restores that bundle the way
// backup.Restore does, so importing the same file twice adds nothing.
package importer

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"hostbay/internal/apperr"
	"hostbay/internal/backup"
	"hostbay/internal/secrets"
	"hostbay/internal/store"
)

// Skipped names a source entry (a file, or a connection by its folder path
// and name) that could not be imported, and why.
type Skipped struct {
	File   string `json:"file"`
	Reason string `json:"reason"`
}

// Report says what an import added. Ran is false when there was nothing to
// import from, in which case there is nothing to show (AE5).
type Report struct {
	Ran     bool      `json:"ran"`
	Hosts   int       `json:"hosts"`
	Folders int       `json:"folders"`
	Scripts int       `json:"scripts"`
	Keys    int       `json:"keys"`
	Skipped []Skipped `json:"skipped"`
	// PasswordsNeeded counts passwords left behind because they could not
	// be sealed; those hosts ask for them at connect (AE7).
	PasswordsNeeded int `json:"passwordsNeeded"`
	// Notes name what imported but needs a look: a password that stayed
	// behind, and every plaintext or weakening option imported as stated.
	Notes []string `json:"notes"`
	// VPNs are the VPN profiles added, each with what the file held that
	// was dropped (R43).
	VPNs []VPNImport `json:"vpns"`
}

// VPNImport is one VPN profile an import read, by its ID in the bundle.
type VPNImport struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Ignored lists each directive or setting that was dropped, such as a
	// script hook, as the file stated it (R43).
	Ignored []string `json:"ignored"`
	// AskPassword: it signs in with a password the file did not hold, so
	// Hostbay asks for it on first connect (R44).
	AskPassword bool `json:"askPassword"`
}

// Result is one parsed export: a bundle to restore with plaintext passwords
// that Restore seals, and what the parser could not map.
type Result struct {
	Bundle   backup.Bundle
	Skipped  []Skipped
	Notes    []string
	VPNs    []VPNImport // one per Bundle.VPNs entry
}

// Import restores res into st and reports what it added.
func Import(st *store.Store, v *secrets.Vault, res Result) (Report, error) {
	rep := Report{Ran: true, Skipped: append([]Skipped{}, res.Skipped...), Notes: append([]string{}, res.Notes...), VPNs: []VPNImport{}}
	if err := Restore(st, v, res.Bundle, WithoutSecrets(res.Bundle), &rep); err != nil {
		return Report{}, err
	}
	if len(res.VPNs) > 0 {
		d, err := st.Data()
		if err != nil {
			return Report{}, err
		}
		// A profile matching a saved one was not added; parsers give each a
		// fresh ID, so the added ones are those saved under it.
		for _, x := range res.VPNs {
			if p := d.VPN(x.ID); p != nil {
				x.AskPassword = x.AskPassword || p.Auth == "password" && p.Secret == ""
				rep.VPNs = append(rep.VPNs, x)
			}
		}
	}
	return rep, nil
}

// Restore merges b into st in one edit, so a failure saves nothing, and
// fills rep's counts. If v cannot seal (a refusing keychain, or a declined
// verification), it restores fallback instead, which must carry no secrets,
// and counts b's passwords as needed at connect (AE7). Nothing is ever
// stored unsealed.
func Restore(st *store.Store, v *secrets.Vault, b, fallback backup.Bundle, rep *Report) error {
	err := restore(st, v, b, rep)
	if c := apperr.CodeOf(err); c == apperr.SecretNeeded || c == apperr.VerifyFailed {
		rep.PasswordsNeeded = 0
		for _, h := range b.Hosts {
			if h.Password != "" {
				rep.PasswordsNeeded++
			}
		}
		for _, p := range b.VPNs {
			if p.Secret != "" {
				rep.PasswordsNeeded++
			}
		}
		err = restore(st, v, fallback, rep)
	}
	return err
}

func restore(st *store.Store, v *secrets.Vault, b backup.Bundle, rep *Report) error {
	return st.Edit(func(d *store.Data) error {
		// A saved list replaces Hostbay's untouched defaults rather than
		// merging with them, so a removed default stays removed.
		if d.Bookmarks.Items == nil && len(b.Bookmarks) > 0 {
			d.SetBookmarks(b.Bookmarks)
		}
		folders, scripts, keys := len(d.Folders.Items), len(d.Scripts.Items), len(d.Keys.Items)
		b, err := refreshClusters(d, v, b, rep)
		if err != nil {
			return err
		}
		keys = len(d.Keys.Items)
		n, _, err := backup.Restore(d, v, b)
		rep.Hosts = n
		rep.Folders = len(d.Folders.Items) - folders
		rep.Scripts = len(d.Scripts.Items) - scripts
		rep.Keys = len(d.Keys.Items) - keys
		return err
	})
}

// refreshClusters updates each saved Kubernetes host that b brings again,
// matched by context name and server URL, with b's token, client
// certificate and key, CA data and TLS server name, and returns b without
// it, so re-importing a kubeconfig renews credentials instead of adding a
// second host (KTD28). A token or key b lacks (the fallback pass carries
// none) leaves the saved one, unless b changes the CA data or TLS server
// name: then it is dropped and asked for again. A replaced key-store key is
// removed unless another host uses it.
func refreshClusters(d *store.Data, v *secrets.Vault, b backup.Bundle, rep *Report) (backup.Bundle, error) {
	keys := maps.Clone(b.Keys)
	var keep []store.Host
	for _, h := range b.Hosts {
		var local *store.Host
		for i := range d.Hosts.Items {
			l := &d.Hosts.Items[i]
			if h.KindName() == "kubernetes" && l.KindName() == "kubernetes" && l.DeletedAt == "" && l.Alias == h.Alias && store.Deref(l.URL) == store.Deref(h.URL) {
				local = l
			}
		}
		if local == nil {
			keep = append(keep, h)
			continue
		}
		// A saved credential never follows the cluster to a new trust anchor
		// or checked name: whoever the new CA vouches for could take it.
		retrust := store.Deref(local.CAData) != store.Deref(h.CAData) || store.Deref(local.TLSServerName) != store.Deref(h.TLSServerName)
		password, identity, old := local.Password, local.IdentityFile, ""
		had := password != "" || identity != ""
		if retrust {
			password, identity = "", ""
			old, _ = strings.CutPrefix(local.IdentityFile, store.KeyRefPrefix)
		}
		if h.Password != "" {
			sealed, err := v.Seal([]byte(h.Password), local.SecretAAD())
			if err != nil {
				return b, err
			}
			password = sealed
		}
		if name, ok := strings.CutPrefix(h.IdentityFile, store.KeyRefPrefix); ok && keys[name] != "" {
			raw, err := base64.StdEncoding.DecodeString(keys[name])
			if err != nil {
				return b, err
			}
			delete(keys, name)
			id := store.NewID()
			sealed, err := v.Seal(raw, store.KeyAAD(id))
			if err != nil {
				return b, err
			}
			k, err := d.AddKey(store.Key{Meta: store.Meta{ID: id}, Name: name, Data: sealed})
			if err != nil {
				return b, err
			}
			old, _ = strings.CutPrefix(local.IdentityFile, store.KeyRefPrefix)
			identity = store.KeyRefPrefix + k.Name
		}
		if err := d.EditHost(local.ID, func(l *store.Host) {
			l.CAData, l.TLSServerName, l.Password, l.IdentityFile = h.CAData, h.TLSServerName, password, identity
		}); err != nil {
			return b, err
		}
		if old != "" && !slices.ContainsFunc(d.Hosts.Items, func(o store.Host) bool { return o.IdentityFile == store.KeyRefPrefix+old }) {
			d.DeleteKey(old)
		}
		rep.Notes = append(rep.Notes, h.Alias+": refreshed the saved cluster's credentials and certificate authority")
		if retrust && had && password == "" && identity == "" {
			rep.Notes = append(rep.Notes, h.Alias+": its certificate authority or server name changed, so a saved token or client key the file does not bring again was dropped; enter it when you next connect")
		}
	}
	b.Hosts, b.Keys = keep, keys
	return b, nil
}

// WithoutSecrets copies b with no passwords, VPN secrets or key-store keys;
// a host that used one of b's keys is left with no key file.
func WithoutSecrets(b backup.Bundle) backup.Bundle {
	keys := b.Keys
	b.Keys = map[string]string{}
	b.Hosts = append([]store.Host(nil), b.Hosts...)
	for i := range b.Hosts {
		b.Hosts[i].Password = ""
		if name, ok := strings.CutPrefix(b.Hosts[i].IdentityFile, store.KeyRefPrefix); ok && keys[name] != "" {
			b.Hosts[i].IdentityFile = ""
		}
	}
	b.Proxies = append([]store.Proxy(nil), b.Proxies...)
	for i := range b.Proxies {
		b.Proxies[i].Password = ""
	}
	b.VPNs = append([]store.VPN(nil), b.VPNs...)
	for i := range b.VPNs {
		b.VPNs[i].Secret = ""
	}
	return b
}

// MaxFileSize caps any export an importer reads; real ones are kilobytes.
const MaxFileSize = 16 << 20

// ReadFile reads path, refusing anything over MaxFileSize without reading it
// all.
func ReadFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, MaxFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxFileSize {
		return nil, fmt.Errorf("%s is larger than %d MiB, too large for a connection export", path, MaxFileSize>>20)
	}
	return raw, nil
}

// Text decodes an export's bytes: UTF-16 or UTF-8 by byte-order mark, else
// UTF-8.
// shortcut: other legacy code pages (GBK, Windows-1252) decode with U+FFFD in
// place of their non-ASCII bytes; add golang.org/x/text decoding if users
// report garbled names.
func Text(raw []byte) string {
	switch {
	case len(raw) >= 2 && raw[0] == 0xFF && raw[1] == 0xFE:
		return utf16Text(raw[2:], func(b []byte) uint16 { return uint16(b[0]) | uint16(b[1])<<8 })
	case len(raw) >= 2 && raw[0] == 0xFE && raw[1] == 0xFF:
		return utf16Text(raw[2:], func(b []byte) uint16 { return uint16(b[1]) | uint16(b[0])<<8 })
	case len(raw) >= 3 && raw[0] == 0xEF && raw[1] == 0xBB && raw[2] == 0xBF:
		raw = raw[3:]
	}
	if utf8.Valid(raw) {
		return string(raw)
	}
	return strings.ToValidUTF8(string(raw), "�")
}

func utf16Text(b []byte, word func([]byte) uint16) string {
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = word(b[2*i:])
	}
	return string(utf16.Decode(u))
}

// INI is a minimal Windows INI reader: [Section] headers and Key=Value
// lines, with sections and keys matched case-insensitively. Later
// duplicates win.
type INI map[string]map[string]string

// ParseINI reads text into sections.
func ParseINI(text string) INI {
	out := INI{}
	section := ""
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(strings.TrimSpace(line[1 : len(line)-1]))
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, ";") {
			continue
		}
		if out[section] == nil {
			out[section] = map[string]string{}
		}
		out[section][strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	return out
}

// Get returns section's key, or "".
func (i INI) Get(section, key string) string {
	return i[strings.ToLower(section)][strings.ToLower(key)]
}

// Builder collects one export's hosts, folders and skipped entries.
type Builder struct {
	tool    string
	res     Result
	folders map[string]string // "a\x00b" path -> folder ID
	ids     map[string]string // the tool's connection ID -> host ID
}

// NewBuilder starts a result for tool, named in notes.
func NewBuilder(tool string) *Builder {
	return &Builder{tool: tool, folders: map[string]string{}, ids: map[string]string{}, res: Result{Bundle: backup.Bundle{Keys: map[string]string{}}}}
}

// Folder finds or adds the nested folders for path, ancestor first, and
// returns the leaf's ID, or nil for an empty path. Blank segments are
// dropped and names are trimmed to one line.
func (b *Builder) Folder(path []string) *string {
	var parent *string
	key := ""
	for _, seg := range path {
		seg = strings.TrimSpace(strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, seg))
		if seg == "" {
			continue
		}
		key += "\x00" + seg
		id, ok := b.folders[key]
		if !ok {
			id = store.NewID()
			b.folders[key] = id
			b.res.Bundle.Folders = append(b.res.Bundle.Folders, store.Folder{Meta: store.Meta{ID: id}, Name: seg, ParentID: parent})
		}
		parent = &id
	}
	return parent
}

// Entry is one connection as a tool stores it, before checks.
type Entry struct {
	Name     string // folder path and name, for the report
	Host     store.Host
	Password string // plaintext; Restore seals it
	SourceID string // the tool's ID, for Jump links
	JumpID   string // the tool's ID of the host this one is reached through
}

// Add checks e and adds it, or skips it with the reason, and reports
// whether it added e. A host name, user or alias that could break a
// known_hosts line, a command line or the host list is refused rather than
// cleaned.
func (b *Builder) Add(e Entry) bool {
	h := e.Host
	h.Alias = strings.TrimSpace(h.Alias)
	if h.Alias == "" {
		h.Alias = h.Hostname
	}
	if err := check(h); err != nil {
		b.Skip(e.Name, err.Error())
		return false
	}
	h.ID = store.NewID()
	if h.Tags == nil {
		h.Tags = []string{}
	}
	h.Password = e.Password
	if e.SourceID != "" {
		b.ids[e.SourceID] = h.ID
	}
	if e.JumpID != "" {
		h.JumpHostID = &e.JumpID // resolved in Result
	}
	b.res.Bundle.Hosts = append(b.res.Bundle.Hosts, h)
	b.noteWeak(e.Name, h)
	return true
}

// AddKey carries a private key for the key store and returns the
// IdentityFile that refers to it.
func (b *Builder) AddKey(name string, pem []byte) string {
	name = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("._-", r) {
			return r
		}
		return '_'
	}, name)
	for base, i := name, 2; b.res.Bundle.Keys[name] != ""; i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	b.res.Bundle.Keys[name] = base64.StdEncoding.EncodeToString(pem)
	return store.KeyRefPrefix + name
}

// Skip reports an entry that was not imported.
func (b *Builder) Skip(name, reason string) {
	b.res.Skipped = append(b.res.Skipped, Skipped{File: name, Reason: reason})
}

// Note reports something about an imported entry.
func (b *Builder) Note(name, text string) {
	b.res.Notes = append(b.res.Notes, name+": "+text)
}

// Result returns what was collected, with Jump links pointed at the hosts
// added for them. A link to a host the export does not hold becomes the
// missing-proxy marker and is noted.
func (b *Builder) Result() Result {
	for i := range b.res.Bundle.Hosts {
		h := &b.res.Bundle.Hosts[i]
		if h.JumpHostID == nil {
			continue
		}
		if id, ok := b.ids[*h.JumpHostID]; ok && id != h.ID {
			h.JumpHostID = &id
		} else {
			// No direct fallback (KTD24): it cannot connect until its
			// settings are looked at.
			h.JumpHostID, h.ProxyID = nil, MissingProxy()
			b.Note(h.Alias, "its jump host is not in this export, so it will not connect until you choose its jump host or a proxy in its settings")
		}
	}
	return b.res
}

// noteWeak lists every plaintext or weakening option imported as stated.
func (b *Builder) noteWeak(name string, h store.Host) {
	if k := h.KindName(); k == "telnet" || k == "tcp" || h.Plaintext() {
		b.Note(name, "connects unencrypted")
	}
	if h.Forwards() {
		b.Note(name, "forwards this computer's SSH agent to the host")
	}
	if h.ForwardsX11() {
		b.Note(name, "forwards X11 to this computer")
	}
	if n := h.Algorithms.Names(); len(n) > 0 {
		b.Note(name, "offers extra SSH algorithms: "+strings.Join(n, ", "))
	}
}

// VPN is one VPN profile as a configuration file states it, before checks.
type VPN struct {
	Profile store.VPN
	Secrets store.VPNSecrets // plaintext; only what the file held (R44)
	Ignored []string         // what the file held that was dropped (R43)
}

// AddVPN checks e and adds it, or skips it with the reason, and reports
// whether it added e. A field that could add lines to the engine
// configuration Hostbay writes from it is refused rather than cleaned.
func (b *Builder) AddVPN(e VPN) bool {
	p := e.Profile
	p.Name = strings.TrimSpace(p.Name)
	if err := checkVPN(p); err != nil {
		name := p.Name
		if name == "" {
			name = p.Gateway
		}
		b.Skip(name, err.Error())
		return false
	}
	p.ID, p.Secret = store.NewID(), ""
	if e.Secrets != (store.VPNSecrets{}) {
		raw, _ := json.Marshal(e.Secrets) // Restore seals it
		p.Secret = string(raw)
	}
	b.res.Bundle.VPNs = append(b.res.Bundle.VPNs, p)
	b.res.VPNs = append(b.res.VPNs, VPNImport{ID: p.ID, Name: p.Name, Ignored: append([]string{}, e.Ignored...), AskPassword: p.Auth == "password" && e.Secrets.Password == ""})
	return true
}

// checkVPN is store.CheckVPN with caps on what a file may hold.
func checkVPN(v store.VPN) error {
	switch {
	case v.Name == "" || len(v.Name) > 200 || strings.IndexFunc(v.Name, unicode.IsControl) >= 0:
		return errors.New("no usable name")
	case len(v.User) > 256:
		return errors.New("a field contains a line break or control character")
	}
	return store.CheckVPN(v)
}

// MissingProxy stands in for a proxy the export names but does not carry.
func MissingProxy() *string { return store.MissingProxy() }

// Database is one database connection as a tool stores it (R56).
type Database struct {
	Name     string // folder path and name, for the report
	Alias    string
	Kind     string // one of store.DatabaseKinds
	Host     string // a SQL Server "host\instance" is split
	Port     int    // 0 for the engine's default
	User     string
	Database string
	Path     string            // sqlite: the file; elasticsearch and rqlite: the URL's path
	Options  map[string]string // driver options by name, under KTD26's import rule
	Tunnel   string            // what the tool reached it through that the export does not carry, such as "the SSH tunnel bastion"; "" for none
	FolderID *string
	Password string // plaintext; Restore seals it
	SourceID string
	JumpID   string // the tool's ID of the SSH connection it is reached through
}

// AddDatabase maps e to a database host and adds it, or skips it with the
// reason. It always imports with TLS verify (KTD25): options that skip or
// weaken verification are dropped with a note, and every other option is
// reported as not imported, by name only, since a value can hold a
// password. A connection reached through something the export does not
// carry gets MissingProxy, never a direct connection.
func (b *Builder) AddDatabase(e Database) {
	h := store.Host{Alias: e.Alias, Hostname: strings.TrimSpace(e.Host), Port: e.Port, User: e.User, Kind: new(e.Kind), FolderID: e.FolderID}
	if e.Database != "" {
		h.Database = new(e.Database)
	}
	var notes []string
	switch e.Kind {
	case "sqlite":
		p := KeyPath(e.Path)
		if p == "" {
			b.Skip(e.Name, fmt.Sprintf("its SQLite file %q is not a path on this computer", e.Path))
			return
		}
		h = store.Host{Alias: e.Alias, Kind: h.Kind, File: &p, FolderID: e.FolderID}
		if e.Tunnel != "" || e.JumpID != "" {
			b.Note(e.Name, "a SQLite file opens on this computer, so its tunnel was not imported")
		}
		b.Add(Entry{Name: e.Name, Host: h})
		return
	case "elasticsearch", "rqlite":
		port := e.Port
		if port == 0 {
			port = map[string]int{"elasticsearch": 9200, "rqlite": 4001}[e.Kind]
		}
		if e.Path != "" && (e.Path[0] != '/' || strings.ContainsAny(e.Path, "?#")) {
			b.Skip(e.Name, fmt.Sprintf("its path %q is not a URL path", e.Path))
			return
		}
		u := url.URL{Scheme: "https", Host: net.JoinHostPort(h.Hostname, strconv.Itoa(port)), Path: e.Path}
		h.URL, h.Port, h.Database = new(u.String()), 0, nil
	case "redis":
		if n, err := strconv.Atoi(e.Database); e.Database != "" && (err != nil || n < 0) {
			h.Database = nil
			notes = append(notes, fmt.Sprintf("its database %q is not a Redis database number, so it opens database 0", e.Database))
		}
	case "mssql":
		// Hostbay dials the instance's TCP port and never asks SQL Server
		// Browser for it over UDP (KTD24).
		if host, inst, ok := strings.Cut(h.Hostname, `\`); ok {
			h.Hostname = host
			if h.Port == 0 {
				notes = append(notes, fmt.Sprintf("its SQL Server instance %q was not imported, because Hostbay connects to a port, not an instance name; enter the instance's TCP port in its settings", inst))
			}
		}
	}
	jump := e.JumpID
	if e.Tunnel != "" && jump == "" {
		h.ProxyID = MissingProxy()
		notes = append(notes, "it connects through "+e.Tunnel+", which was not imported; choose its jump host or a proxy in its settings before connecting")
	}
	if !b.Add(Entry{Name: e.Name, Host: h, Password: e.Password, SourceID: e.SourceID, JumpID: jump}) {
		return
	}
	for _, text := range notes {
		b.Note(e.Name, text)
	}
	b.databaseOptions(e.Name, e.Kind, e.Options)
}

// databaseOptions applies KTD26's import rule to a database host's driver
// options.
func (b *Builder) databaseOptions(name, kind string, opts map[string]string) {
	// Elasticsearch and rqlite have no TLS setting: their URL says how.
	noTLS := "TLS set to none"
	if kind == "elasticsearch" || kind == "rqlite" {
		noTLS = "its URL changed to http://"
	}
	verified, weakened := false, false
	var other []string
	for _, k := range slices.Sorted(maps.Keys(opts)) {
		switch tlsOption(k, opts[k]) {
		case "verify":
			verified = true
		case "kept":
		case "plain":
			weakened = true
			b.Note(name, k+" was not imported, because it asks for no TLS or TLS without checks; Hostbay connects with TLS and checks the server's certificate. If the server has no TLS, it needs "+noTLS+" in its settings")
		case "trust":
			weakened = true
			b.Note(name, k+" was not imported, because it skips certificate checks; Hostbay checks the server's certificate, and a self-signed one is asked about at first connect")
		default:
			other = append(other, k)
		}
	}
	if !verified && !weakened {
		b.Note(name, "the export says nothing on TLS, so it was imported to connect with TLS and check the server's certificate; a self-signed one is asked about at first connect, and a server without TLS needs "+noTLS+" in its settings")
	}
	if len(other) > 0 {
		b.Skip(name+": driver options", strings.Join(other, ", ")+": no Hostbay setting matches")
	}
}

// tlsOption sorts one driver option: "verify" asks for verified TLS,
// "kept" asks for TLS or is a safe default, "plain" asks for no TLS or
// unverified TLS, "trust" skips certificate checks, and "" is any other.
func tlsOption(key, value string) string {
	v := strings.ToLower(strings.TrimSpace(value))
	on := v == "true" || v == "yes" || v == "1"
	switch strings.ToLower(key) {
	case "sslmode", "ssl_mode", "ssl_pgsslmode":
		if v == "verify-full" || v == "verify-ca" || v == "verify_identity" || v == "verify_ca" {
			return "verify"
		}
		return "plain"
	case "ssl", "usessl", "requiressl", "ssl.require", "encrypt", "esusessl":
		if on || v == "strict" || v == "mandatory" {
			return "kept"
		}
		return "plain"
	case "tls":
		switch v {
		case "true", "1":
			return "kept"
		case "skip-verify":
			return "trust"
		}
		return "plain"
	case "trustservercertificate", "ssl_weakcertvalidation", "ssl_allowinvalidhostname", "esskipverify":
		if on {
			return "trust"
		}
		return "kept"
	case "verifyservercertificate", "ssl.verify.server":
		if on {
			return "kept"
		}
		return "trust"
	case "sslfactory":
		if strings.Contains(v, "nonvalidating") {
			return "trust"
		}
	}
	return ""
}

// check refuses a host whose fields could not be saved safely.
func check(h store.Host) error {
	if err := oneLine("name", h.Alias, 200); err != nil {
		return err
	}
	if err := oneLine("user name", h.User, 256); err != nil {
		return err
	}
	if err := oneLine("database name", store.Deref(h.Database), 256); err != nil {
		return err
	}
	// A SQLite file and a WebDAV, S3 or other URL host are addressed by
	// their path or URL, not a host name.
	if h.Hostname == "" && store.Deref(h.File) == "" && store.Deref(h.URL) == "" {
		return errors.New("no host name")
	}
	if h.Hostname != "" && (len(h.Hostname) > 253 || h.Hostname[0] == '-' || strings.IndexFunc(h.Hostname, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r) || strings.ContainsRune(",*?!|#@[]\\\"/'`<>;", r)
	}) >= 0) {
		return fmt.Errorf("the host name %q is not a valid address", h.Hostname)
	}
	if h.Port < 0 || h.Port > 65535 {
		return fmt.Errorf("port %d is out of range", h.Port)
	}
	return nil
}

func oneLine(what, s string, max int) error {
	if len(s) > max {
		return fmt.Errorf("the %s is longer than %d characters", what, max)
	}
	if strings.IndexFunc(s, unicode.IsControl) >= 0 {
		return fmt.Errorf("the %s contains a line break or control character", what)
	}
	return nil
}

// KeyPath returns a key file reference from an export as a local absolute
// path, expanding the home-folder placeholders tools write, or "" when it
// cannot name a file on this computer: a relative or other-OS path, one
// that climbs with "..", or a key-store reference.
func KeyPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, ph := range []string{"$(HomeDir)", "${HomeDir}", "%USERPROFILE%", "_ProfileDir_"} {
			if rest, ok := strings.CutPrefix(p, ph); ok {
				p = home + rest
			}
		}
		if rest, ok := strings.CutPrefix(p, "~"); ok && (rest == "" || rest[0] == '/' || rest[0] == '\\') {
			p = home + rest
		}
	}
	if os.PathSeparator == '/' {
		p = strings.ReplaceAll(p, `\`, "/")
	}
	for _, seg := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." {
			return ""
		}
	}
	if !filepath.IsAbs(p) || strings.IndexFunc(p, unicode.IsControl) >= 0 {
		return ""
	}
	return p
}
