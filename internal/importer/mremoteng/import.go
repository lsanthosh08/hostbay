// Package mremoteng imports an mRemoteNG connection file (confCons.xml).
//
// The XML model, inheritance and password decryption are adapted from
// uniterm's backend/importer/mremoteng.go (github.com/ys-ll/uniterm),
// Copyright 2025 uniTerm, licensed under the Apache License, Version 2.0.
// Hostbay adds the Protected check that tells a wrong master password from
// a damaged file, a cap on key-derivation work, and its own host kinds.
package mremoteng

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/sha1"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"

	"hostbay/internal/importer"
	"hostbay/internal/store"
)

// defaultPassword is RootNodeInfo.DefaultPassword in mRemoteNG: what it
// encrypts with when no master password is set.
const defaultPassword = "mR3m"

// kdfBudget caps the PBKDF2 rounds one import may spend, so a hostile file
// of many passwords at the maximum iteration count cannot hang the app.
const kdfBudget = 50_000_000

type document struct {
	XMLName            xml.Name `xml:"Connections"`
	EncryptionEngine   string   `xml:"EncryptionEngine,attr"`
	BlockCipherMode    string   `xml:"BlockCipherMode,attr"`
	KdfIterations      string   `xml:"KdfIterations,attr"`
	FullFileEncryption string   `xml:"FullFileEncryption,attr"`
	Protected          string   `xml:"Protected,attr"`
	Nodes              []node   `xml:"Node"`
}

type values struct {
	Username             string `xml:"Username,attr"`
	Domain               string `xml:"Domain,attr"`
	Password             string `xml:"Password,attr"`
	Port                 string `xml:"Port,attr"`
	Protocol             string `xml:"Protocol,attr"`
	UseCredSsp           string `xml:"UseCredSsp,attr"`
	PuttySession         string `xml:"PuttySession,attr"`
	RDGatewayUsageMethod string `xml:"RDGatewayUsageMethod,attr"`
}

type node struct {
	values
	Name                       string `xml:"Name,attr"`
	Type                       string `xml:"Type,attr"`
	Hostname                   string `xml:"Hostname,attr"`
	InheritUsername            string `xml:"InheritUsername,attr"`
	InheritDomain              string `xml:"InheritDomain,attr"`
	InheritPassword            string `xml:"InheritPassword,attr"`
	InheritPort                string `xml:"InheritPort,attr"`
	InheritProtocol            string `xml:"InheritProtocol,attr"`
	InheritUseCredSsp          string `xml:"InheritUseCredSsp,attr"`
	InheritPuttySession        string `xml:"InheritPuttySession,attr"`
	InheritRDGatewayUsageMethd string `xml:"InheritRDGatewayUsageMethod,attr"`
	Nodes                      []node `xml:"Node"`
}

// Parse reads the file at path. password is the master password; empty
// means mRemoteNG's default, which a file without one uses.
func Parse(path, password string) (importer.Result, error) {
	raw, err := importer.ReadFile(path)
	if err != nil {
		return importer.Result{}, err
	}
	raw = bytes.TrimSpace(bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf}))
	// Before format 2.6, full-file encryption wrote bare base64, not XML.
	if len(raw) > 0 && raw[0] != '<' {
		return importer.Result{}, errors.New("this mRemoteNG file is fully encrypted, which Hostbay cannot read; turn off \"Encrypt complete connection file\" in mRemoteNG and export again")
	}
	var doc document
	if err := xml.Unmarshal(raw, &doc); err != nil {
		// XML errors can quote input, which may hold credential material.
		return importer.Result{}, errors.New("this is not an mRemoteNG connection file")
	}
	if strings.EqualFold(strings.TrimSpace(doc.FullFileEncryption), "true") {
		return importer.Result{}, errors.New("this mRemoteNG file is fully encrypted, which Hostbay cannot read; turn off \"Encrypt complete connection file\" in mRemoteNG and export again")
	}

	b := importer.NewBuilder("mRemoteNG")
	dec := newDecrypter(doc, password)
	if err := dec.checkProtected(password); err != nil {
		return importer.Result{}, err
	}
	var walk func(n node, path []string, parent values)
	walk = func(n node, path []string, parent values) {
		v := n.effective(parent)
		name := strings.Join(append(append([]string{}, path...), n.Name), "/")
		if strings.EqualFold(n.Type, "Container") {
			b.Folder(append(path, n.Name))
			for _, c := range n.Nodes {
				walk(c, append(append([]string{}, path...), n.Name), v)
			}
			return
		}
		if n.Type != "" && !strings.EqualFold(n.Type, "Connection") {
			b.Skip(name, fmt.Sprintf("mRemoteNG node type %q is not a connection", n.Type))
			return
		}
		h, reason := toHost(n, v)
		if reason != "" {
			b.Skip(name, reason)
			return
		}
		h.FolderID = b.Folder(path)
		pw, note := dec.password(v.Password)
		if note != "" {
			b.Note(name, note)
		}
		if puttySession := strings.TrimSpace(v.PuttySession); h.KindName() == "ssh" && puttySession != "" && puttySession != "Default Settings" {
			b.Note(name, fmt.Sprintf("uses PuTTY session %q for its settings, which was not imported", puttySession))
		}
		if h.KindName() == "rdp" && v.RDGatewayUsageMethod != "" && !strings.EqualFold(v.RDGatewayUsageMethod, "Never") {
			b.Note(name, "uses a Remote Desktop Gateway, which Hostbay does not support; it was imported to connect directly")
		}
		b.Add(importer.Entry{Name: name, Host: h, Password: pw})
	}
	for _, n := range doc.Nodes {
		walk(n, nil, values{})
	}
	return b.Result(), nil
}

// toHost maps a connection's protocol to a Hostbay kind, or says why not.
func toHost(n node, v values) (store.Host, string) {
	port, _ := strconv.Atoi(strings.TrimSpace(v.Port))
	if port < 1 || port > 65535 {
		port = 0
	}
	h := store.Host{Alias: n.Name, Hostname: strings.TrimSpace(n.Hostname), User: v.Username}
	kind := func(k string) *string { return &k }
	switch strings.ToUpper(strings.TrimSpace(v.Protocol)) {
	case "SSH2":
		h.Port, h.AuthType = or(port, 22), kind("password")
	case "SSH1":
		return h, "SSH-1 is obsolete and not supported"
	case "TELNET":
		h.Kind, h.Port, h.User = kind("telnet"), or(port, 23), ""
	case "RDP":
		h.Kind, h.AuthType, h.Port, h.RdpPort = kind("rdp"), kind("password"), 22, new(or(port, 3389))
		if d := strings.TrimSpace(v.Domain); d != "" && h.User != "" {
			h.User = d + `\` + h.User
		}
		if strings.EqualFold(v.UseCredSsp, "true") {
			h.RdpSecurity = kind("nla")
		}
	case "VNC":
		h.Kind, h.Port = kind("vnc"), port
	case "RAW":
		if port == 0 {
			return h, "a raw connection needs a port"
		}
		h.Kind, h.Port, h.User = kind("tcp"), port, ""
	default:
		return h, fmt.Sprintf("mRemoteNG protocol %q has no Hostbay equivalent", v.Protocol)
	}
	return h, ""
}

func or(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func (n node) effective(parent values) values {
	pick := func(flag, own, inherited string) string {
		if strings.EqualFold(strings.TrimSpace(flag), "true") {
			return inherited
		}
		return own
	}
	return values{
		Username:             pick(n.InheritUsername, n.Username, parent.Username),
		Domain:               pick(n.InheritDomain, n.Domain, parent.Domain),
		Password:             pick(n.InheritPassword, n.Password, parent.Password),
		Port:                 pick(n.InheritPort, n.Port, parent.Port),
		Protocol:             pick(n.InheritProtocol, n.Protocol, parent.Protocol),
		UseCredSsp:           pick(n.InheritUseCredSsp, n.UseCredSsp, parent.UseCredSsp),
		PuttySession:         pick(n.InheritPuttySession, n.PuttySession, parent.PuttySession),
		RDGatewayUsageMethod: pick(n.InheritRDGatewayUsageMethd, n.RDGatewayUsageMethod, parent.RDGatewayUsageMethod),
	}
}

// decrypter opens the file's password attributes, each decrypted once.
type decrypter struct {
	usable     string // why passwords cannot be read, or ""
	master     string
	protected  string
	iterations int
	spent      int
	seen       map[string]string
}

func newDecrypter(doc document, password string) *decrypter {
	d := &decrypter{master: password, protected: doc.Protected, seen: map[string]string{}}
	if d.master == "" {
		d.master = defaultPassword
	}
	d.iterations, _ = strconv.Atoi(strings.TrimSpace(doc.KdfIterations))
	switch {
	case !strings.EqualFold(doc.EncryptionEngine, "AES") || !strings.EqualFold(doc.BlockCipherMode, "GCM"):
		d.usable = "its password was not imported: this file uses an older mRemoteNG encryption; save it in mRemoteNG 1.76 or later and export again"
	case d.iterations < 1000 || d.iterations > 1_000_000:
		// mRemoteNG requires at least 1000; the cap bounds work from the file.
		d.usable = "its password was not imported: the file's key-derivation settings are out of range"
	}
	return d
}

// checkProtected decrypts the file's Protected marker, which mRemoteNG
// writes so a wrong master password is told apart from damage.
func (d *decrypter) checkProtected(given string) error {
	if d.usable != "" || strings.TrimSpace(d.protected) == "" {
		return nil
	}
	if _, err := decrypt(d.protected, d.master, d.iterations); err != nil {
		if given == "" {
			return errors.New("this mRemoteNG file is protected by a master password; enter it to import")
		}
		return errors.New("the mRemoteNG master password is wrong")
	}
	return nil
}

func (d *decrypter) password(encoded string) (string, string) {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return "", ""
	}
	if d.usable != "" {
		return "", d.usable
	}
	if pw, ok := d.seen[encoded]; ok {
		return pw, ""
	}
	if d.spent += d.iterations; d.spent > kdfBudget {
		return "", "its password was not imported: the file holds too many passwords to decrypt"
	}
	pw, err := decrypt(encoded, d.master, d.iterations)
	if err != nil {
		return "", "its password could not be decrypted, so it was not imported"
	}
	d.seen[encoded] = pw
	return pw, ""
}

// decrypt follows mRemoteNG's AeadCryptographyProvider and
// Pkcs5S2KeyGenerator: the base64 payload is salt(16) | nonce(16) |
// ciphertext | tag(16), the key is PBKDF2-HMAC-SHA1 to 32 bytes, and the
// salt is the GCM additional data.
func decrypt(encoded, password string, iterations int) (string, error) {
	payload, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil || len(payload) < 48 {
		return "", errors.New("invalid mRemoteNG password payload")
	}
	// BouncyCastle's Pkcs5PasswordToBytes keeps the low byte of each UTF-16
	// code unit.
	chars := utf16.Encode([]rune(password))
	pass := make([]byte, len(chars))
	for i, c := range chars {
		pass[i] = byte(c)
	}
	key, err := pbkdf2.Key(sha1.New, string(pass), payload[:16], iterations, 32)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, 16)
	if err != nil {
		return "", err
	}
	plain, err := gcm.Open(nil, payload[16:32], payload[32:], payload[:16])
	if err != nil {
		return "", errors.New("mRemoteNG password authentication failed")
	}
	return string(plain), nil
}
