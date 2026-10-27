// Package securecrt imports sessions from a SecureCRT XML export
// (File > Export Settings).
//
// The XML walk over nested <key> folders follows uniterm's
// backend/importer/securecrt.go (github.com/ys-ll/uniterm), Copyright 2025
// uniTerm, licensed under the Apache License, Version 2.0. Hostbay adds
// password decryption, key files, proxies and forwarding.
package securecrt

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/crypto/blowfish"

	"hostbay/internal/importer"
	"hostbay/internal/store"
)

type key struct {
	Name    string `xml:"name,attr"`
	Strings []val  `xml:"string"`
	Dwords  []val  `xml:"dword"`
	Keys    []key  `xml:"key"`
}

type val struct {
	Name  string `xml:"name,attr"`
	Value string `xml:",chardata"`
}

func (k key) str(name string) string {
	for _, s := range k.Strings {
		if s.Name == name {
			return strings.TrimSpace(s.Value)
		}
	}
	return ""
}

func (k key) dword(name string) int {
	for _, d := range k.Dwords {
		if d.Name == name {
			v, _ := strconv.Atoi(strings.TrimSpace(d.Value))
			return v
		}
	}
	return 0
}

// Parse reads the export at path. password is SecureCRT's configuration
// passphrase, empty when none is set.
func Parse(path, password string) (importer.Result, error) {
	raw, err := importer.ReadFile(path)
	if err != nil {
		return importer.Result{}, err
	}
	var root key
	if err := xml.Unmarshal(raw, &root); err != nil {
		// XML errors can quote input, which may hold credential material.
		return importer.Result{}, errors.New("this is not a SecureCRT XML export")
	}
	// The sessions are the root itself or, in a full export, a child key.
	sessions, ok := root, root.Name == "Sessions"
	for _, c := range root.Keys {
		if !ok && c.Name == "Sessions" {
			sessions, ok = c, true
		}
	}
	if !ok {
		return importer.Result{}, errors.New("this SecureCRT export holds no Sessions")
	}
	b := importer.NewBuilder("SecureCRT")
	var walk func(k key, folder []string)
	walk = func(k key, folder []string) {
		name := strings.Join(append(append([]string{}, folder...), k.Name), "/")
		if proto := k.str("Protocol Name"); proto != "" && !(len(folder) == 0 && k.Name == "Default") {
			h, reason := toHost(k, proto)
			if reason != "" {
				b.Skip(name, reason)
			} else {
				h.FolderID = b.Folder(folder)
				pw := sessionPassword(k, password, name, b)
				if id := k.str("Identity Filename V2"); id != "" && h.KindName() == "ssh" {
					id, _, _ = strings.Cut(id, "::") // a key file may carry options after "::"
					if p := importer.KeyPath(id); p != "" {
						h.IdentityFile, h.AuthType = p, new("key")
					} else {
						b.Note(name, fmt.Sprintf("its key file %q is not a path on this computer; choose the key in its settings", id))
					}
				}
				if fw := k.str("Firewall Name"); fw != "" && !strings.EqualFold(fw, "None") {
					h.ProxyID = importer.MissingProxy()
					b.Note(name, fmt.Sprintf("uses SecureCRT firewall %q, which was not imported; choose a proxy in its settings before connecting", fw))
				}
				b.Add(importer.Entry{Name: name, Host: h, Password: pw})
			}
		}
		for _, c := range k.Keys {
			walk(c, append(append([]string{}, folder...), k.Name))
		}
	}
	for _, k := range sessions.Keys {
		walk(k, nil)
	}
	return b.Result(), nil
}

func toHost(k key, proto string) (store.Host, string) {
	h := store.Host{Alias: k.Name, Hostname: k.str("Hostname"), User: k.str("Username")}
	port := k.dword("Port")
	switch proto {
	case "SSH2":
		h.Port, h.AuthType = k.dword("[SSH2] Port"), new("password")
		if h.Port == 0 {
			h.Port = 22
		}
		if k.dword("Enable Agent Forwarding") == 1 {
			h.ForwardAgent = new(true)
		}
		if k.dword("Forward X11") == 1 {
			h.X11 = new(true)
		}
	case "SSH1":
		return h, "SSH-1 is obsolete and not supported"
	case "Telnet":
		h.Kind, h.Port, h.User = new("telnet"), port, ""
		if h.Port == 0 {
			h.Port = 23
		}
	case "Raw":
		if port == 0 {
			return h, "a raw connection needs a port"
		}
		h.Kind, h.Port, h.User = new("tcp"), port, ""
	case "Serial":
		return h, "serial ports are opened from Hostbay's Serial port action, not saved as hosts"
	default:
		return h, fmt.Sprintf("SecureCRT protocol %q has no Hostbay equivalent", proto)
	}
	return h, ""
}

// sessionPassword decrypts the session's saved password, noting why when it
// cannot.
func sessionPassword(k key, passphrase, name string, b *importer.Builder) string {
	if v2 := k.str("Password V2"); v2 != "" {
		prefix, hexed, _ := strings.Cut(v2, ":")
		switch prefix {
		case "02":
			if pw, err := decryptV2(hexed, passphrase); err == nil {
				return pw
			}
			b.Note(name, "its password could not be decrypted; check the configuration passphrase")
		default:
			// "03" derives its key with bcrypt_pbkdf, which Go's x/crypto
			// keeps internal.
			b.Note(name, fmt.Sprintf("its password uses SecureCRT's %q format, which Hostbay cannot read; enter it after import", prefix))
		}
		return ""
	}
	if legacy := k.str("Password"); strings.HasPrefix(legacy, "u") {
		if pw, err := decryptLegacy(legacy[1:]); err == nil {
			return pw
		}
		b.Note(name, "its password could not be decrypted")
	}
	return ""
}

// decryptV2 opens a "Password V2" value with prefix 02: AES-256-CBC with a
// zero IV and SHA-256(passphrase) as the key, over length(4, little-endian)
// | password | SHA-256(password) | padding.
func decryptV2(hexed, passphrase string) (string, error) {
	ct, err := hex.DecodeString(hexed)
	if err != nil || len(ct) == 0 || len(ct)%aes.BlockSize != 0 {
		return "", errors.New("invalid ciphertext")
	}
	k := sha256.Sum256([]byte(passphrase))
	block, _ := aes.NewCipher(k[:])
	plain := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, make([]byte, aes.BlockSize)).CryptBlocks(plain, ct)
	if len(plain) < 4 {
		return "", errors.New("invalid plaintext")
	}
	n := int(binary.LittleEndian.Uint32(plain))
	if n > len(plain)-4-sha256.Size {
		return "", errors.New("invalid length")
	}
	pw, sum := plain[4:4+n], plain[4+n:4+n+sha256.Size]
	if got := sha256.Sum256(pw); !bytes.Equal(got[:], sum) || !utf8.Valid(pw) {
		return "", errors.New("wrong passphrase")
	}
	return string(pw), nil
}

// The two fixed Blowfish keys of SecureCRT's original "Password" format.
var (
	legacyKey1 = []byte{0x24, 0xa6, 0x3d, 0xde, 0x5b, 0xd3, 0xb3, 0x82, 0x9c, 0x7e, 0x06, 0xf4, 0x08, 0x16, 0xaa, 0x07}
	legacyKey2 = []byte{0x5f, 0xb0, 0x45, 0xa2, 0x94, 0x17, 0xd9, 0x16, 0xc6, 0xc6, 0xa2, 0xff, 0x06, 0x41, 0x82, 0xb7}
)

// decryptLegacy opens SecureCRT's original "Password" value (after its "u"
// prefix): Blowfish-CBC with a zero IV under the first key, four bytes off
// each end, then again under the second, giving NUL-terminated UTF-16LE.
func decryptLegacy(hexed string) (string, error) {
	ct, err := hex.DecodeString(hexed)
	if err != nil || len(ct) <= 8 || len(ct)%blowfish.BlockSize != 0 {
		return "", errors.New("invalid ciphertext")
	}
	cbc := func(k, in []byte) []byte {
		c, _ := blowfish.NewCipher(k)
		out := make([]byte, len(in))
		cipher.NewCBCDecrypter(c, make([]byte, blowfish.BlockSize)).CryptBlocks(out, in)
		return out
	}
	plain := cbc(legacyKey2, cbc(legacyKey1, ct)[4:len(ct)-4])
	var u []uint16
	for i := 0; i+1 < len(plain); i += 2 {
		w := binary.LittleEndian.Uint16(plain[i:])
		if w == 0 {
			return string(utf16.Decode(u)), nil
		}
		u = append(u, w)
	}
	return "", errors.New("unterminated password")
}
