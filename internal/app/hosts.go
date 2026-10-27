package app

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"hostbay/internal/apperr"
	"hostbay/internal/backup"
	"hostbay/internal/importer"
	"hostbay/internal/importer/dbeaver"
	"hostbay/internal/importer/kubeconfig"
	"hostbay/internal/importer/mobaxterm"
	"hostbay/internal/importer/mremoteng"
	"hostbay/internal/importer/navicat"
	"hostbay/internal/importer/securecrt"
	"hostbay/internal/importer/sshmanager"
	"hostbay/internal/importer/uniterm"
	"hostbay/internal/importer/virtviewer"
	"hostbay/internal/importer/windterm"
	"hostbay/internal/importer/xshell"
	"hostbay/internal/secrets"
	"hostbay/internal/sshx"
	"hostbay/internal/store"
)

// HostService manages hosts, folders, trash, scripts, bookmarks, keys and
// backups.
type HostService struct {
	st    *store.Store
	vault *secrets.Vault
	ver   secrets.Verifier // gates exports (R15)
}

func NewHostService(st *store.Store, vault *secrets.Vault, ver secrets.Verifier) *HostService {
	return &HostService{st: st, vault: vault, ver: ver}
}

// SSHConfigImport reports an ~/.ssh/config import: how many hosts were
// added, and notes such as a ProxyJump alias that matched no host.
type SSHConfigImport struct {
	Imported int      `json:"imported"`
	Notes    []string `json:"notes"`
}

// StoreImport reports a backup restore: how many hosts were added, and
// notes such as a host whose jump host or proxy the backup did not carry.
type StoreImport struct {
	Imported int      `json:"imported"`
	Notes    []string `json:"notes"`
}

// toUI shows a stored host to the UI: never the sealed password, only that
// one is saved, and a key-store key by its bare name as the key picker
// expects.
func toUI(h store.Host) Host {
	u := Host{
		ID: h.ID, Alias: h.Alias, Hostname: h.Hostname, Port: h.Port, User: h.User,
		IdentityFile: strings.TrimPrefix(h.IdentityFile, store.KeyRefPrefix),
		Tags:         h.Tags, LastConnected: h.LastConnected, FolderID: h.FolderID, Order: h.Order,
		AuthType: h.AuthType, Kind: h.Kind, RdpPort: h.RdpPort, RdpSecurity: h.RdpSecurity, Mac: h.Mac,
		JumpHostID: h.JumpHostID,
		TLS:        h.TLS, URL: h.URL, Domain: h.Domain, Bucket: h.Bucket, Region: h.Region, PathStyle: h.PathStyle,
		ProxyID: h.ProxyID, ForwardAgent: h.ForwardAgent, Algorithms: h.Algorithms, Keepalive: h.Keepalive, LocalEcho: h.LocalEcho,
		X11: h.X11, X11Desktop: h.X11Desktop, Database: h.Database, File: h.File,
		Namespace: h.Namespace,
	}
	if u.Tags == nil {
		u.Tags = []string{}
	}
	if h.Password != "" {
		u.Password, u.HasPassword = new(string), true
	}
	return u
}

func (s *HostService) listHosts(trash bool) ([]Host, error) {
	d, err := s.st.Data()
	if err != nil {
		return nil, err
	}
	out := []Host{}
	for _, h := range d.Hosts.Items {
		if (h.DeletedAt != "") == trash {
			out = append(out, toUI(h))
		}
	}
	return out, nil
}

func (s *HostService) ListHosts() ([]Host, error) { return s.listHosts(false) }
func (s *HostService) ListTrash() ([]Host, error) { return s.listHosts(true) }

// SaveHost adds or updates a host. An empty or absent password keeps the
// saved one; a new one is sealed to the host's address (KTD6), and for a
// file host to how it connects too (KTD22). If either changes, the saved
// password stays sealed to the old one and must be entered again (R29), so
// a password saved for FTPS is never sent over plain FTP. A SQLite host's
// path is never taken from the page, only from the FilePick token of a file
// chosen in DatabasePickFile's dialog.
func (s *HostService) SaveHost(host Host) (Host, error) {
	if err := checkFileHost(host); err != nil {
		return Host{}, err
	}
	if err := checkDatabaseHost(host); err != nil {
		return Host{}, err
	}
	if err := checkClusterHost(host); err != nil {
		return Host{}, err
	}
	var out store.Host
	err := s.st.Edit(func(d *store.Data) error {
		h := store.Host{Meta: store.Meta{ID: host.ID}}
		if e := d.Host(host.ID); host.ID != "" && e != nil {
			h = *e
		} else if h.ID == "" {
			h.ID = store.NewID()
		}
		if err := checkExtras(d, host, h.ProxyID); err != nil {
			return err
		}
		fromForm(d, &h, host)
		if host.FilePick != nil && *host.FilePick != "" {
			path, ok := takeFilePick(*host.FilePick)
			if !ok {
				return errors.New("the chosen file has expired; choose it again")
			}
			h.File = &path
		}
		if h.KindName() == "sqlite" && (h.File == nil || *h.File == "") {
			return errors.New("choose the SQLite database file")
		}
		if host.Password != nil && *host.Password != "" {
			sealed, err := s.vault.Seal([]byte(*host.Password), h.SecretAAD())
			if err != nil {
				return err
			}
			h.Password = sealed
		}
		out = d.PutHost(h)
		return nil
	})
	return toUI(out), err
}

// fromForm copies the form's fields onto h, leaving its ID, Meta and
// sealed password alone.
func fromForm(d *store.Data, h *store.Host, host Host) {
	h.Alias, h.Hostname, h.Port, h.User = host.Alias, host.Hostname, host.Port, host.User
	h.IdentityFile = host.IdentityFile
	// The key picker sends a key-store key by its bare name.
	if !strings.ContainsAny(h.IdentityFile, `/\~`) && d.KeyByName(h.IdentityFile) != nil {
		h.IdentityFile = store.KeyRefPrefix + h.IdentityFile
	}
	h.Tags, h.FolderID, h.Order, h.AuthType = host.Tags, host.FolderID, host.Order, host.AuthType
	h.Kind, h.RdpPort, h.RdpSecurity, h.Mac, h.JumpHostID = host.Kind, host.RdpPort, host.RdpSecurity, host.Mac, host.JumpHostID
	h.TLS, h.URL, h.Domain, h.Bucket, h.Region, h.PathStyle = host.TLS, host.URL, host.Domain, host.Bucket, host.Region, host.PathStyle
	h.ProxyID, h.ForwardAgent, h.Algorithms, h.Keepalive, h.LocalEcho = host.ProxyID, host.ForwardAgent, host.Algorithms, host.Keepalive, host.LocalEcho
	h.X11, h.X11Desktop = host.X11, host.X11Desktop
	h.Database = host.Database // File is never the page's to set
	// A cluster's CA and TLS server name are never the page's to set, and
	// it dials the host and port of its URL, which its token is bound to.
	h.Namespace = host.Namespace
	if h.KindName() == "kubernetes" {
		h.Hostname, h.Port = clusterAddr(store.Deref(h.URL))
	}
}

// clusterAddr is the host and port an https:// URL is dialled at.
func clusterAddr(raw string) (string, int) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", 0
	}
	port, _ := strconv.Atoi(u.Port())
	if port == 0 {
		port = 443
	}
	return u.Hostname(), port
}

// checkClusterHost refuses Kubernetes settings that could not connect
// (R57): the server must be https:// and the namespace a valid name.
func checkClusterHost(h Host) error {
	if h.Kind == nil || *h.Kind != "kubernetes" {
		return nil
	}
	if u, err := url.Parse(store.Deref(h.URL)); err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" {
		return errors.New("enter the cluster's API server URL, starting https://")
	}
	if ns := store.Deref(h.Namespace); ns != "" && !clusterNamespace.MatchString(ns) {
		return fmt.Errorf("%q is not a valid namespace name", ns)
	}
	if store.Deref(h.TLS) != "" {
		return errors.New("a Kubernetes host always verifies the server's certificate")
	}
	return nil
}

var clusterNamespace = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// checkDatabaseHost refuses database settings that could not connect, or
// that would connect another way than the form showed (R56).
func checkDatabaseHost(h Host) error {
	kind := store.Deref(h.Kind)
	switch kind {
	case "elasticsearch":
		u, err := url.Parse(store.Deref(h.URL))
		if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
			return errors.New("enter the node's URL, starting https:// (or http:// to connect unencrypted)")
		}
		if store.Deref(h.TLS) != "" {
			return errors.New("an Elasticsearch host connects as its URL says; it has no TLS mode")
		}
		if strings.IndexFunc(h.User, unicode.IsControl) >= 0 {
			return errors.New("the user name cannot contain control characters")
		}
	case "rqlite":
		if h.URL == nil || !(strings.HasPrefix(*h.URL, "https://") || strings.HasPrefix(*h.URL, "http://")) {
			return errors.New("enter the rqlite node's URL, starting https:// (or http:// to connect unencrypted)")
		}
		if u, err := url.Parse(*h.URL); err != nil || u.Host == "" || u.User != nil {
			return errors.New("the node's URL is not valid")
		}
		if store.Deref(h.TLS) != "" {
			return errors.New("an rqlite host's URL says how it connects; it has no TLS mode")
		}
	case "postgres", "redis", "mongodb", "mysql", "mssql", "oracle":
		if db := store.Deref(h.Database); kind == "redis" && db != "" {
			if n, err := strconv.Atoi(db); err != nil || n < 0 {
				return errors.New("the Redis database is a number, 0 or more")
			}
		}
		if h.TLS != nil && !slices.Contains([]string{"", "verify", "none"}, *h.TLS) {
			return fmt.Errorf("unknown TLS mode %q", *h.TLS)
		}
		if strings.TrimSpace(h.Hostname) == "" {
			return errors.New("enter the server's host name")
		}
		if h.Port < 0 || h.Port > 65535 {
			return errors.New("the port must be between 1 and 65535")
		}
		if strings.IndexFunc(h.User, unicode.IsControl) >= 0 || (h.Database != nil && strings.IndexFunc(*h.Database, unicode.IsControl) >= 0) {
			return errors.New("the user and database names cannot contain control characters")
		}
	case "sqlite":
		if (h.JumpHostID != nil && *h.JumpHostID != "") || (h.ProxyID != nil && *h.ProxyID != "") {
			return errors.New("a SQLite host is a file on this computer; it cannot use a jump host or proxy")
		}
		if store.Deref(h.TLS) != "" {
			return errors.New("a SQLite host is a file on this computer; it has no TLS mode")
		}
	}
	return nil
}

// checkFileHost refuses file-host, VNC and SPICE settings that could not connect,
// or that would connect some other way than the form showed.
func checkFileHost(h Host) error {
	kind := store.Deref(h.Kind)
	switch kind {
	case "ftp":
		if h.TLS != nil && !slices.Contains([]string{"", "explicit", "implicit", "none"}, *h.TLS) {
			return fmt.Errorf("unknown FTP TLS mode %q", *h.TLS)
		}
	case "vnc":
		if h.TLS != nil && !slices.Contains([]string{"", "vencrypt", "none"}, *h.TLS) {
			return fmt.Errorf("unknown VNC security mode %q", *h.TLS)
		}
	case "spice":
		if h.TLS != nil && !slices.Contains([]string{"", "tls", "none"}, *h.TLS) {
			return fmt.Errorf("unknown SPICE security mode %q", *h.TLS)
		}
	case "webdav", "s3":
		if h.URL == nil || !(strings.HasPrefix(*h.URL, "https://") || strings.HasPrefix(*h.URL, "http://")) {
			return errors.New("enter the server's URL, starting https:// (or http:// to connect unencrypted)")
		}
		if u, err := url.Parse(*h.URL); err != nil || u.Host == "" {
			return errors.New("the server's URL is not valid")
		}
		if kind == "s3" && (h.Bucket == nil || *h.Bucket == "" || h.User == "") {
			return errors.New("an S3 host needs a bucket and an access key ID")
		}
	}
	return nil
}

// checkExtras refuses connection extras that could not work, or that would
// connect another way than the form showed (R48–R52). saved is the host's
// stored proxy: keeping it unchanged passes even when sync left it pointing
// at a proxy this computer lacks, which then fails at connect.
func checkExtras(d *store.Data, h Host, saved *string) error {
	kind := ""
	if h.Kind != nil {
		kind = *h.Kind
	}
	if h.AuthType != nil && !slices.Contains([]string{"", "key", "password", "agent", "kerberos"}, *h.AuthType) {
		return fmt.Errorf("unknown sign-in method %q", *h.AuthType)
	}
	if kind == "tcp" && (h.Port < 1 || h.Port > 65535) {
		return errors.New("a raw TCP host needs a port")
	}
	if h.Keepalive != nil && (*h.Keepalive < 0 || *h.Keepalive > 86400) {
		return errors.New("the keepalive interval must be between 0 and 86400 seconds")
	}
	if h.Algorithms != nil {
		if err := sshx.Algorithms(*h.Algorithms).Validate(); err != nil {
			return err
		}
	}
	if slices.Contains(store.FileKinds, kind) && h.JumpHostID != nil && *h.JumpHostID != "" {
		return errors.New("a file host cannot be reached through a jump host; remove its jump host or connect it through a proxy")
	}
	if h.ProxyID == nil || *h.ProxyID == "" {
		return nil
	}
	switch {
	case h.JumpHostID != nil && *h.JumpHostID != "":
		return errors.New("a host reached through a jump host cannot also use a proxy; set the proxy on the jump host")
	case kind == "mosh":
		return errors.New("mosh cannot use a proxy, because its UDP traffic would bypass it")
	case d.Proxy(*h.ProxyID) == nil && (saved == nil || *saved != *h.ProxyID):
		return errors.New("the chosen proxy no longer exists")
	}
	return nil
}

// ClearRecent forgets when every host was last connected to, emptying the
// recent list. Like the stamp, it is not an edit to sync.
func (s *HostService) ClearRecent() error {
	return s.st.Edit(func(d *store.Data) error {
		for _, h := range d.Hosts.Items {
			if err := d.SetLastConnected(h.ID, ""); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteHost moves a host to trash.
func (s *HostService) DeleteHost(id string) error {
	return s.st.Edit(func(d *store.Data) error { return d.TrashHost(id) })
}

func (s *HostService) RestoreHost(id string) error {
	return s.st.Edit(func(d *store.Data) error { return d.RestoreHost(id) })
}

// PurgeHost removes a host for good, leaving a tombstone for sync.
func (s *HostService) PurgeHost(id string) error {
	return s.st.Edit(func(d *store.Data) error { return d.PurgeHost(id) })
}

// CloneHost copies a host as "<alias> copy", re-sealing its password to the
// copy. A password that no longer opens is left off the copy; a locked
// vault fails the clone so the UI can unlock it and try again.
func (s *HostService) CloneHost(id string) (Host, error) {
	var out store.Host
	err := s.st.Edit(func(d *store.Data) error {
		c, err := d.CloneHost(id)
		if err != nil {
			return err
		}
		if orig := d.Host(id); orig.Password != "" {
			pw, err := s.vault.Open(orig.Password, orig.SecretAAD())
			var ae *apperr.Error
			if errors.As(err, &ae) && ae.Details["reason"] == "locked" {
				return err
			}
			if err == nil {
				sealed, err := s.vault.Seal(pw, c.SecretAAD())
				if err != nil {
					return err
				}
				d.Host(c.ID).Password = sealed // same edit as the add, so no extra revision
			}
		}
		out = *d.Host(c.ID)
		return nil
	})
	return toUI(out), err
}

// ImportSshConfig imports a chosen ssh_config file, or a folder holding a
// "config" file; with no path, ~/.ssh/config.
func (s *HostService) ImportSshConfig(path *string) (SSHConfigImport, error) {
	var p string
	if path != nil && *path != "" {
		p = *path
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			p = filepath.Join(p, "config")
		}
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return SSHConfigImport{}, err
		}
		p = filepath.Join(home, ".ssh", "config")
	}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return SSHConfigImport{}, fmt.Errorf("no config file at %s", p)
	}
	if err != nil {
		return SSHConfigImport{}, err
	}
	defer f.Close()
	out := SSHConfigImport{Notes: []string{}}
	err = s.st.Edit(func(d *store.Data) error {
		n, notes, err := d.ImportSSHConfig(f)
		out.Imported, out.Notes = n, append(out.Notes, notes...)
		return err
	})
	return out, err
}

// ExportStore writes a backup of everything: encrypted under passphrase, or
// plain without secrets when it is empty. Either needs OS verification.
func (s *HostService) ExportStore(ctx context.Context, path, passphrase string) error {
	return s.export(ctx, "", path, passphrase)
}

// ExportHost writes a backup of one host, as ExportStore does.
func (s *HostService) ExportHost(ctx context.Context, id, path, passphrase string) error {
	return s.export(ctx, id, path, passphrase)
}

func (s *HostService) export(ctx context.Context, id, path, passphrase string) error {
	if err := secrets.Gate(ctx, s.ver, "export saved hosts"); err != nil {
		return err
	}
	d, err := s.st.Data()
	if err != nil {
		return err
	}
	out, err := backup.Export(d, s.vault, id, passphrase)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o600)
}

// ImportStore restores a Hostbay backup or an SSH Manager .sshmbak or JSON
// export, adding what is not saved yet. It reports how many hosts it added
// and which need a jump host or proxy chosen.
func (s *HostService) ImportStore(path string, passphrase *string) (StoreImport, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return StoreImport{}, err
	}
	pass := ""
	if passphrase != nil {
		pass = *passphrase
	}
	b, err := backup.Read(raw, pass)
	if err != nil {
		return StoreImport{}, err
	}
	out := StoreImport{Notes: []string{}}
	err = s.st.Edit(func(d *store.Data) error {
		n, notes, err := backup.Restore(d, s.vault, b)
		out.Imported, out.Notes = n, append(out.Notes, notes...)
		return err
	})
	return out, err
}

// ImportFromSshManager runs the SSH Manager import again on request, so a
// file the first launch skipped can be retried (R31). It merges as the first
// launch did, keeping SSH Manager's IDs, so hosts already imported are not
// added twice, and a refusing keychain still brings hosts across without
// their passwords (AE7). No SSH Manager folder is an error, not a report.
func (s *HostService) ImportFromSshManager() (sshmanager.Report, error) {
	src, err := sshmanager.DefaultSourceDir()
	if err != nil {
		return sshmanager.Report{}, err
	}
	rep, err := sshmanager.Import(s.st, s.vault, src)
	if err != nil {
		return sshmanager.Report{}, err
	}
	if !rep.Ran {
		return rep, fmt.Errorf("no SSH Manager data at %s", src)
	}
	// Recorded so the automatic first-launch import never runs after this.
	return rep, s.st.MarkSSHManagerImported(time.Now().UTC())
}

// importers parse each tool's export (R54); the UI names them by these keys.
var importers = map[string]func(path, password string) (importer.Result, error){
	"mobaxterm":  mobaxterm.Parse,
	"mremoteng":  mremoteng.Parse,
	"securecrt":  securecrt.Parse,
	"xshell":     xshell.Parse,
	"windterm":   windterm.Parse,
	"virtviewer": virtviewer.Parse,
	"uniterm":    uniterm.Parse,
	"kubeconfig": kubeconfig.Parse,
	"navicat":    navicat.Parse,
	"dbeaver":    dbeaver.Parse,
}

// ImportFrom imports another tool's connection export at path, adding what
// is not saved yet, so importing the same file twice adds nothing.
// password is the tool's master or export password where it encrypts saved
// passwords, and is used only to read them. Passwords are sealed as they
// are stored; entries that cannot be mapped are reported as skipped, with
// why, and plaintext or weakening options imported as stated are noted.
// Imported hosts trust no host key: known_hosts stays the only source.
func (s *HostService) ImportFrom(tool, path, password string) (importer.Report, error) {
	parse, ok := importers[tool]
	if !ok {
		return importer.Report{}, fmt.Errorf("unknown import source %q", tool)
	}
	res, err := parse(path, password)
	if err != nil {
		return importer.Report{}, err
	}
	return importer.Import(s.st, s.vault, res)
}

// WriteTextFile writes text to a local path the user chose (log dumps).
func (s *HostService) WriteTextFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

// ReadTextFile reads a local text file the user chose (.rdp import).
func (s *HostService) ReadTextFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	return string(b), err
}

// RevealPassword returns a host's saved password after OS verification.
func (s *HostService) RevealPassword(ctx context.Context, id string) (string, error) {
	d, err := s.st.Data()
	if err != nil {
		return "", err
	}
	h := d.Host(id)
	if h == nil {
		return "", errors.New("host not found")
	}
	if h.Password == "" {
		return "", errors.New("no password saved for this host")
	}
	pw, err := s.vault.RevealGated(ctx, h.Password, h.SecretAAD(), "reveal the saved password")
	return string(pw), err
}

// ListSshKeys lists the private keys in ~/.ssh.
func (s *HostService) ListSshKeys() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, ".ssh")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	keys := []string{}
	for _, e := range entries {
		n := e.Name()
		if !e.Type().IsRegular() || strings.HasSuffix(n, ".pub") || strings.HasPrefix(n, ".") ||
			slices.Contains([]string{"config", "known_hosts", "authorized_keys"}, n) {
			continue
		}
		keys = append(keys, filepath.Join(dir, n))
	}
	slices.Sort(keys)
	return keys, nil
}

// SaveIdentityKey copies a private key file into the key store, sealed, and
// returns the name it was saved under.
func (s *HostService) SaveIdentityKey(sourcePath string) (string, error) {
	raw, err := os.ReadFile(sourcePath)
	if err != nil {
		return "", fmt.Errorf("read key: %w", err)
	}
	var name string
	err = s.st.Edit(func(d *store.Data) error {
		id := store.NewID()
		sealed, err := s.vault.Seal(raw, store.KeyAAD(id))
		if err != nil {
			return err
		}
		k, err := d.AddKey(store.Key{Meta: store.Meta{ID: id}, Name: filepath.Base(sourcePath), Data: sealed})
		name = k.Name
		return err
	})
	return name, err
}

func (s *HostService) ListIdentityKeys() ([]string, error) {
	d, err := s.st.Data()
	if err != nil {
		return nil, err
	}
	return d.KeyNames(), nil
}

func (s *HostService) DeleteIdentityKey(name string) error {
	return s.st.Edit(func(d *store.Data) error { d.DeleteKey(name); return nil })
}

func (s *HostService) ListFolders() ([]Folder, error) {
	d, err := s.st.Data()
	if err != nil {
		return nil, err
	}
	out := []Folder{}
	for _, f := range d.Folders.Items {
		out = append(out, Folder{ID: f.ID, Name: f.Name, ParentID: f.ParentID, Order: f.Order})
	}
	return out, nil
}

func (s *HostService) SaveFolder(folder Folder) (Folder, error) {
	var out store.Folder
	err := s.st.Edit(func(d *store.Data) error {
		f := store.Folder{Meta: store.Meta{ID: folder.ID}}
		if e := d.Folder(folder.ID); e != nil {
			f = *e
		}
		f.Name, f.ParentID, f.Order = folder.Name, folder.ParentID, folder.Order
		var err error
		out, err = d.SaveFolder(f)
		return err
	})
	return Folder{ID: out.ID, Name: out.Name, ParentID: out.ParentID, Order: out.Order}, err
}

// DeleteFolder removes a folder. mode "purge" trashes its hosts and removes
// its subfolders; anything else moves them up to its parent.
func (s *HostService) DeleteFolder(id, mode string) error {
	return s.st.Edit(func(d *store.Data) error { return d.DeleteFolder(id, mode == "purge") })
}

func (s *HostService) ListScripts() ([]Script, error) {
	d, err := s.st.Data()
	if err != nil {
		return nil, err
	}
	out := []Script{}
	for _, sc := range d.ScriptList() {
		out = append(out, Script{ID: sc.ID, Label: sc.Label, Script: sc.Script})
	}
	return out, nil
}

func (s *HostService) SaveScripts(scripts []Script) error {
	in := []store.Script{}
	for _, sc := range scripts {
		in = append(in, store.Script{Meta: store.Meta{ID: sc.ID}, Label: sc.Label, Script: sc.Script})
	}
	return s.st.Edit(func(d *store.Data) error { d.SetScripts(in); return nil })
}

func (s *HostService) ListBookmarks() ([]string, error) {
	d, err := s.st.Data()
	if err != nil {
		return nil, err
	}
	return d.BookmarkPaths(), nil
}

func (s *HostService) SaveBookmarks(bookmarks []string) error {
	return s.st.Edit(func(d *store.Data) error { d.SetBookmarks(append([]string{}, bookmarks...)); return nil })
}

// HostCredentials hands backend code (connect, sudo, RDP) a host with its
// secrets unsealed. It is deliberately not a Wails service: plaintext must
// never be reachable from the UI except through RevealPassword's gate.
type HostCredentials struct {
	st    *store.Store
	vault *secrets.Vault
}

func NewHostCredentials(st *store.Store, vault *secrets.Vault) *HostCredentials {
	return &HostCredentials{st: st, vault: vault}
}

// Credentials is what connecting to a host needs.
type Credentials struct {
	Host     store.Host // Host.Password is still sealed; use Password
	Password string     // unsealed; empty unless the host has one that opens
	Key      []byte     // the key-store key file when Host.IdentityFile is "keystore:<name>"
}

// Lookup returns host id with its secrets unsealed. A password-auth host
// whose password is missing or no longer opens, or a key-store key that is
// missing or will not open, is a SecretNeeded error, never an empty secret
// (R29). Any other IdentityFile is a path for the caller to read.
func (c *HostCredentials) Lookup(id string) (Credentials, error) {
	d, err := c.st.Data()
	if err != nil {
		return Credentials{}, err
	}
	return c.lookupIn(d, id)
}

// lookupIn is Lookup on already-loaded data.
func (c *HostCredentials) lookupIn(d *store.Data, id string) (Credentials, error) {
	cr, err := c.lookup(d, id)
	// Name the host, which may be a jump host, so the UI knows whose
	// password to ask for.
	return cr, withHostID(err, id)
}

func (c *HostCredentials) lookup(d *store.Data, id string) (Credentials, error) {
	h := d.Host(id)
	if h == nil {
		return Credentials{}, errors.New("host not found")
	}
	return c.unseal(d, *h, "")
}

// anonymousDatabase reports a Redis, MongoDB, Elasticsearch or rqlite host
// that signs in to nothing: one with no user name, and for Redis, whose
// password needs no user name (AUTH <password>), no saved password either.
// A server that then asks for a password is answered by SecretNeeded.
func anonymousDatabase(h store.Host) bool {
	switch h.KindName() {
	case "redis":
		return h.User == "" && h.Password == ""
	case "mongodb", "elasticsearch", "rqlite":
		return h.User == ""
	}
	return false
}

// unseal opens h's secrets; a non-empty typed password is used instead of
// the saved one.
func (c *HostCredentials) unseal(d *store.Data, h store.Host, typed string) (Credentials, error) {
	cr := Credentials{Host: h}
	// A file host signs in with a password unless it has no user name
	// (anonymous FTP, a guest share, a public WebDAV folder). A database
	// signs in with a password, except a SQLite file and a server open to
	// anyone (anonymousDatabase).
	passwordAuth := (h.AuthType != nil && *h.AuthType == "password") || h.KindName() == "rdp" || h.KindName() == "vnc" ||
		(slices.Contains(store.FileKinds, h.KindName()) && h.User != "") ||
		(h.IsDatabase() && h.KindName() != "sqlite" && !anonymousDatabase(h)) ||
		(h.KindName() == "kubernetes" && h.Password != "") // a saved token must open; a client certificate alone needs none
	if typed != "" {
		cr.Password = typed
	} else if h.Password != "" {
		pw, err := c.vault.Open(h.Password, h.SecretAAD())
		if err != nil && passwordAuth {
			return Credentials{}, err
		}
		cr.Password = string(pw)
	} else if passwordAuth {
		return Credentials{}, apperr.New(apperr.SecretNeeded, "No password is saved for this host; enter it.").With("reason", "missing")
	}
	if name, ok := strings.CutPrefix(h.IdentityFile, store.KeyRefPrefix); ok {
		k := d.KeyByName(name)
		if k == nil {
			return Credentials{}, apperr.New(apperr.SecretNeeded, fmt.Sprintf("The key %q is no longer in Hostbay's key store; import it again.", name)).With("reason", "missing")
		}
		var err error
		if cr.Key, err = c.vault.Open(k.Data, store.KeyAAD(k.ID)); err != nil {
			return Credentials{}, err
		}
	}
	return cr, nil
}
