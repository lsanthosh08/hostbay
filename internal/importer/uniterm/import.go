// Package uniterm imports a uniterm connection export (.utm).
//
// The file layout and its password protection (PBKDF2-HMAC-SHA256 with
// 100000 rounds over the file's salt, AES-256-GCM "enc:v1:" fields for
// passwords and key text) follow uniterm's backend/importer/uniterm.go,
// crypto.go and backend/credentials (github.com/ys-ll/uniterm), Copyright
// 2025 uniTerm, licensed under the Apache License, Version 2.0.
package uniterm

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"hostbay/internal/importer"
	"hostbay/internal/sshx"
	"hostbay/internal/store"
)

type file struct {
	Format    string `json:"format"`
	Encrypted bool   `json:"encrypted"`
	KDF       *struct {
		Salt string `json:"salt"`
	} `json:"kdf"`
	Groups []struct {
		ID       string  `json:"id"`
		Name     string  `json:"name"`
		ParentID *string `json:"parentId"`
	} `json:"groups"`
	Connections []conn `json:"connections"`
}

type conn struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	Type            string  `json:"type"`
	Host            string  `json:"host"`
	Port            int     `json:"port"`
	User            string  `json:"user"`
	AuthType        string  `json:"authType"`
	Password        string  `json:"password"`
	KeyPath         string  `json:"keyPath"`
	KeyContent      string  `json:"keyContent"`
	GroupID         *string `json:"groupId"`
	RdpDomain       string  `json:"rdpDomain"`
	RdpEnableNLA    bool    `json:"rdpEnableNLA"`
	TunnelSSHConnID string  `json:"tunnelSSHConnId"`
	TunnelSSHUser   string  `json:"tunnelSSHUser"`
	ProxyID         string  `json:"proxyId"`
	FtpEncryption   string  `json:"ftpEncryption"`
	SmbDomain       string  `json:"smbDomain"`
	DBType          string  `json:"dbType"`
	DBName          string  `json:"dbName"`
	DBParams        string  `json:"dbParams"`
	EsUseSSL        bool    `json:"esUseSsl"`
	EsPathPrefix    string  `json:"esPathPrefix"`
	EsSkipVerify    bool    `json:"esSkipVerify"`
	RedisMode       string  `json:"redisMode"`
	S3Region        string  `json:"s3Region"`
	S3Bucket        string  `json:"s3Bucket"`
	S3URLStyle      string  `json:"s3UrlStyle"`
	X11Forwarding   bool    `json:"x11Forwarding"`
	AgentForwarding bool    `json:"agentForwarding"`
	SSHAlgorithms   *struct {
		Mode         string   `json:"mode"`
		KeyExchanges []string `json:"keyExchanges"`
		Ciphers      []string `json:"ciphers"`
		MACs         []string `json:"macs"`
		HostKeys     []string `json:"hostKeys"`
	} `json:"sshAlgorithms"`
}

// Parse reads the .utm file at path. password is the export password,
// needed when the file was saved with one.
func Parse(path, password string) (importer.Result, error) {
	raw, err := importer.ReadFile(path)
	if err != nil {
		return importer.Result{}, err
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil || f.Format != "uniterm" {
		return importer.Result{}, errors.New("this is not a uniterm export")
	}
	// An unprotected export holds plain values; one still sealed to the
	// exporting computer's credential store cannot be opened here.
	open := func(s string) (string, error) {
		if strings.HasPrefix(s, "enc:v1:") {
			return "", nil
		}
		return s, nil
	}
	if f.Encrypted {
		if password == "" {
			return importer.Result{}, errors.New("this uniterm export is password-protected; enter its password")
		}
		if f.KDF == nil {
			return importer.Result{}, errors.New("this uniterm export is damaged: it has no key-derivation settings")
		}
		salt, err := base64.StdEncoding.DecodeString(f.KDF.Salt)
		if err != nil {
			return importer.Result{}, errors.New("this uniterm export is damaged: its salt is not valid")
		}
		key, err := pbkdf2.Key(sha256.New, password, salt, 100_000, 32)
		if err != nil {
			return importer.Result{}, err
		}
		open = func(s string) (string, error) { return openField(s, key) }
	}

	groups := map[string]int{}
	for i, g := range f.Groups {
		groups[g.ID] = i
	}
	// folderPath follows parents to the root; a cycle or an unknown parent
	// ends the path.
	folderPath := func(id *string) []string {
		var out []string
		for seen := map[string]bool{}; id != nil && !seen[*id]; {
			seen[*id] = true
			i, ok := groups[*id]
			if !ok {
				break
			}
			out = append([]string{f.Groups[i].Name}, out...)
			id = f.Groups[i].ParentID
		}
		return out
	}

	b := importer.NewBuilder("uniterm")
	for _, g := range f.Groups {
		b.Folder(append(folderPath(g.ParentID), g.Name))
	}
	wrong := false
	for _, c := range f.Connections {
		folder := folderPath(c.GroupID)
		name := strings.Join(append(append([]string{}, folder...), c.Name), "/")
		pw, err := open(c.Password)
		keyText, kerr := open(c.KeyContent)
		if err != nil || kerr != nil {
			wrong = true
			break
		}
		if !f.Encrypted && (pw == "" && c.Password != "" || keyText == "" && c.KeyContent != "") {
			b.Note(name, "its password or key is still sealed to the computer that exported it and was not imported")
		}
		if databaseTypes[c.Type] {
			addDatabase(b, c, name, folder, pw)
			continue
		}
		h, reason := toHost(c, b, name)
		if reason != "" {
			b.Skip(name, reason)
			continue
		}
		if h.KindName() == "ssh" || h.KindName() == "mosh" {
			switch c.AuthType {
			case "keyText":
				if keyText != "" {
					h.IdentityFile = b.AddKey(c.Name, []byte(keyText))
				}
			case "key":
				if p := importer.KeyPath(c.KeyPath); p != "" {
					h.IdentityFile = p
				} else {
					h.AuthType = new("password")
					b.Note(name, fmt.Sprintf("its key file %q is not a path on this computer; choose the key in its settings", c.KeyPath))
				}
			}
		}
		h.FolderID = b.Folder(folder)
		jump := c.TunnelSSHConnID
		if slices.Contains(store.FileKinds, h.KindName()) {
			jump = "" // toHost noted it
		}
		b.Add(importer.Entry{Name: name, Host: h, Password: pw, SourceID: c.ID, JumpID: jump})
	}
	if wrong {
		return importer.Result{}, errors.New("the uniterm export password is wrong")
	}
	return b.Result(), nil
}

// toHost maps a uniterm connection to a Hostbay host, or says why not.
func toHost(c conn, b *importer.Builder, name string) (store.Host, string) {
	h := store.Host{Alias: c.Name, Hostname: c.Host, Port: c.Port, User: c.User}
	def := func(p int) {
		if h.Port == 0 {
			h.Port = p
		}
	}
	switch c.Type {
	case "ssh", "sftp", "scp", "mosh":
		def(22)
		if c.Type == "mosh" {
			h.Kind = new("mosh")
		}
		switch c.AuthType {
		case "agent", "kerberos":
			h.AuthType = new(c.AuthType)
		case "key", "keyText":
			h.AuthType = new("key")
		default:
			h.AuthType = new("password")
		}
		if c.AgentForwarding {
			h.ForwardAgent = new(true)
		}
		if c.X11Forwarding && c.Type != "mosh" {
			h.X11 = new(true)
		}
		if a := c.SSHAlgorithms; a != nil && a.Mode == "custom" {
			algs := store.Algorithms{KeyExchanges: a.KeyExchanges, Ciphers: a.Ciphers, MACs: a.MACs, HostKeys: a.HostKeys}
			if err := sshx.Algorithms(algs).Validate(); err != nil {
				b.Note(name, "its custom SSH algorithms were not imported: "+err.Error())
			} else {
				h.Algorithms = &algs
			}
		}
		if c.TunnelSSHUser != "" {
			b.Note(name, "its jump host user override was not imported; the jump host signs in as itself")
		}
	case "telnet":
		h.Kind, h.User = new("telnet"), ""
		def(23)
	case "tcp":
		if c.Port < 1 {
			return h, "a raw TCP connection needs a port"
		}
		h.Kind, h.User = new("tcp"), ""
	case "rdp":
		h.Kind, h.AuthType, h.Port, h.RdpPort = new("rdp"), new("password"), 22, new(c.Port)
		if c.Port == 0 {
			h.RdpPort = new(3389)
		}
		if c.RdpDomain != "" && h.User != "" {
			h.User = c.RdpDomain + `\` + h.User
		}
		if c.RdpEnableNLA {
			h.RdpSecurity = new("nla")
		}
	case "vnc":
		h.Kind = new("vnc")
	case "spice":
		h.Kind = new("spice")
	case "ftp":
		h.Kind = new("ftp")
		if c.FtpEncryption == "" || c.FtpEncryption == "none" {
			h.TLS = new("none")
		}
	case "smb":
		h.Kind = new("smb")
		if c.SmbDomain != "" {
			h.Domain = new(c.SmbDomain)
		}
	case "webdav", "s3":
		u := strings.TrimSuffix(c.Host, "/")
		if u == "" && c.Type == "s3" {
			region := c.S3Region
			if region == "" {
				region = "us-east-1"
			}
			u = "https://s3." + region + ".amazonaws.com"
		}
		pu, err := url.Parse(u)
		if err != nil || (pu.Scheme != "https" && pu.Scheme != "http") || pu.Host == "" {
			return h, "its URL is not an http:// or https:// address"
		}
		h.Kind, h.URL, h.Hostname, h.Port = new(c.Type), &u, pu.Hostname(), 0
		if c.Type == "s3" {
			if c.S3Bucket == "" || c.User == "" {
				return h, "an S3 connection needs a bucket and an access key ID"
			}
			h.Bucket = new(c.S3Bucket)
			if c.S3Region != "" {
				h.Region = new(c.S3Region)
			}
			if c.S3URLStyle == "path" {
				h.PathStyle = new(true)
			}
		}
	case "serial":
		return h, "serial ports are opened from Hostbay's Serial port action, not saved as hosts"
	default:
		return h, fmt.Sprintf("uniterm connection type %q has no Hostbay equivalent", c.Type)
	}
	if c.TunnelSSHConnID != "" && slices.Contains(store.FileKinds, h.KindName()) {
		// No direct fallback: it cannot connect until its settings are looked at.
		h.ProxyID = importer.MissingProxy()
		b.Note(name, "its jump host was not imported, because a file host cannot be reached through one; choose a proxy in its settings, or clear it to connect directly")
	} else if c.ProxyID != "" && c.TunnelSSHConnID != "" {
		b.Note(name, "its proxy was not imported, because it is reached through a jump host; set the proxy on the jump host")
	} else if c.ProxyID != "" {
		if h.KindName() == "mosh" {
			return h, "a Mosh connection cannot go through a proxy in Hostbay, because its UDP would bypass it"
		}
		h.ProxyID = importer.MissingProxy()
		b.Note(name, "uses a uniterm proxy, which the export does not carry; choose a proxy in its settings before connecting")
	}
	return h, ""
}

// databaseTypes are uniterm's database connection types; "database"
// names its engine in dbType.
var databaseTypes = map[string]bool{"database": true, "redis": true, "mongodb": true, "elasticsearch": true}

// addDatabase maps a uniterm database connection to a database host. Its
// SSH connection becomes the jump host, as for other non-file kinds.
func addDatabase(b *importer.Builder, c conn, name string, folder []string, pw string) {
	kind := map[string]string{"mysql": "mysql", "postgres": "postgres", "rqlite": "rqlite", "oracle": "oracle", "sqlserver": "mssql", "sqlite": "sqlite"}[c.DBType]
	if c.Type != "database" {
		kind = c.Type
	}
	if kind == "" {
		b.Skip(name, fmt.Sprintf("uniterm database type %q has no Hostbay equivalent", c.DBType))
		return
	}
	if kind == "redis" && c.RedisMode == "sentinel" {
		b.Skip(name, "Redis Sentinel is not supported yet; add the primary as a Redis host")
		return
	}
	opts := map[string]string{}
	if q, err := url.ParseQuery(c.DBParams); err == nil {
		for k := range q {
			opts[k] = q.Get(k)
		}
	} else {
		b.Skip(name+": driver options", "they are not in name=value form")
	}
	if kind == "elasticsearch" {
		opts["esUseSsl"], opts["esSkipVerify"] = strconv.FormatBool(c.EsUseSSL), strconv.FormatBool(c.EsSkipVerify)
	}
	e := importer.Database{
		Name: name, Alias: c.Name, Kind: kind, Host: c.Host, Port: c.Port, User: c.User, Database: c.DBName, Path: c.EsPathPrefix,
		Options: opts, FolderID: b.Folder(folder), Password: pw, SourceID: c.ID, JumpID: c.TunnelSSHConnID,
	}
	if kind == "sqlite" {
		e.Host, e.Path = "", c.Host
	}
	if c.ProxyID != "" && c.TunnelSSHConnID != "" {
		b.Note(name, "its proxy was not imported, because it is reached through a jump host; set the proxy on the jump host")
	} else if c.ProxyID != "" {
		e.Tunnel = "a uniterm proxy"
	}
	b.AddDatabase(e)
}

// openField decrypts an "enc:v1:<nonce>:<ciphertext>" field, AES-256-GCM in
// base64; any other value is plain and returned as it is.
func openField(s string, key []byte) (string, error) {
	body, ok := strings.CutPrefix(s, "enc:v1:")
	if !ok {
		return s, nil
	}
	n64, c64, ok := strings.Cut(body, ":")
	nonce, err1 := base64.StdEncoding.DecodeString(n64)
	ct, err2 := base64.StdEncoding.DecodeString(c64)
	if !ok || err1 != nil || err2 != nil {
		return "", errors.New("damaged field")
	}
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	if len(nonce) != gcm.NonceSize() {
		return "", errors.New("damaged field")
	}
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}
