// Package navicat imports a Navicat connection export (.ncx) as database
// hosts (R54, R56).
//
// The connection types and password decryption (AES-128-CBC with Navicat's
// fixed key for version 12 and later, Blowfish for version 11) are adapted
// from uniterm's backend/importer/navicat.go (github.com/ys-ll/uniterm),
// Copyright 2025 uniTerm, licensed under the Apache License, Version 2.0.
// Hostbay's version 11 decryptor feeds the ciphertext back into the
// chaining value as Navicat does, so passwords longer than eight bytes
// decrypt; it adds a nesting cap, Hostbay's host kinds, and the TLS and
// tunnel rules of KTD25 and KTD26.
package navicat

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"golang.org/x/crypto/blowfish"

	"hostbay/internal/importer"
)

// maxDepth caps XML nesting; an export is three levels deep.
const maxDepth = 32

var (
	v12Key = []byte("libcckeylibcckey")
	v12IV  = []byte("libcciv libcciv ")
)

var kinds = map[string]string{
	"MYSQL": "mysql", "MARIADB": "mysql", "POSTGRESQL": "postgres", "MSSQL": "mssql", "SQLSERVER": "mssql",
	"ORACLE": "oracle", "SQLITE": "sqlite", "MONGODB": "mongodb", "REDIS": "redis",
}

type connection struct {
	Name         string `xml:"ConnectionName,attr"`
	Type         string `xml:"ConnType,attr"`
	Host         string `xml:"Host,attr"`
	Port         string `xml:"Port,attr"`
	Database     string `xml:"Database,attr"`
	File         string `xml:"DatabaseFileName,attr"`
	User         string `xml:"UserName,attr"`
	Password     string `xml:"Password,attr"`
	Folder       string `xml:"Folder,attr"`
	SSL          string `xml:"SSL,attr"`
	PGSSLMode    string `xml:"SSL_PGSSLMode,attr"`
	WeakCert     string `xml:"SSL_WeakCertValidation,attr"`
	InvalidHost  string `xml:"SSL_AllowInvalidHostName,attr"`
	SSH          string `xml:"SSH,attr"`
	SSHHost      string `xml:"SSH_Host,attr"`
	HTTP         string `xml:"HTTP,attr"`
	HTTPProxy    string `xml:"HTTP_Proxy,attr"`
	HTTPProxyURL string `xml:"HTTP_URL,attr"`
}

// Parse reads the .ncx file at path. password is unused: Navicat encrypts
// passwords with a fixed key.
func Parse(path, _ string) (importer.Result, error) {
	raw, err := importer.ReadFile(path)
	if err != nil {
		return importer.Result{}, err
	}
	text := importer.Text(raw)
	// The text is UTF-8 now, whatever the declaration says.
	decoder := func() *xml.Decoder {
		d := xml.NewDecoder(strings.NewReader(text))
		d.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
		return d
	}
	for d, depth := decoder(), 0; ; {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			// XML errors can quote input, which may hold credential material.
			return importer.Result{}, errors.New("this is not a Navicat connection export (.ncx)")
		}
		switch tok.(type) {
		case xml.StartElement:
			if depth++; depth > maxDepth {
				return importer.Result{}, errors.New("this file is nested too deeply to be a Navicat connection export")
			}
		case xml.EndElement:
			depth--
		}
	}
	var doc struct {
		XMLName xml.Name     `xml:"Connections"`
		Conns   []connection `xml:"Connection"`
	}
	if err := decoder().Decode(&doc); err != nil {
		return importer.Result{}, errors.New("this is not a Navicat connection export (.ncx)")
	}

	b := importer.NewBuilder("Navicat")
	for _, c := range doc.Conns {
		var folder []string
		if c.Folder != "" {
			folder = []string{c.Folder}
		}
		name := strings.Join(append(append([]string{}, folder...), c.Name), "/")
		kind, ok := kinds[strings.ToUpper(strings.TrimSpace(c.Type))]
		if !ok {
			b.Skip(name, fmt.Sprintf("Navicat connection type %q has no Hostbay equivalent", c.Type))
			continue
		}
		pw := ""
		if c.Password != "" {
			if pw, err = decrypt(c.Password); err != nil {
				b.Note(name, "its password could not be decrypted and was not imported")
			}
		}
		port, _ := strconv.Atoi(strings.TrimSpace(c.Port))
		opts := map[string]string{}
		if c.SSL == "true" {
			opts = map[string]string{"SSL": c.SSL, "SSL_WeakCertValidation": c.WeakCert, "SSL_AllowInvalidHostName": c.InvalidHost}
			if kind == "postgres" {
				opts["SSL_PGSSLMode"] = c.PGSSLMode
			}
		}
		tunnel := ""
		switch {
		case c.SSH == "true":
			tunnel = fmt.Sprintf("the SSH tunnel %q", c.SSHHost)
		case c.HTTP == "true":
			tunnel = "an HTTP tunnel, which Hostbay does not support"
		case c.HTTPProxy == "true":
			tunnel = "a proxy"
		}
		b.AddDatabase(importer.Database{
			Name: name, Alias: c.Name, Kind: kind, Host: c.Host, Port: port, User: c.User, Database: c.Database,
			Path: c.File, Options: opts, Tunnel: tunnel, FolderID: b.Folder(folder), Password: pw,
		})
	}
	return b.Result(), nil
}

// decrypt opens a Navicat password: version 12's AES-128-CBC first, then
// version 11's Blowfish, both upper-case hex.
func decrypt(s string) (string, error) {
	data, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil || len(data) == 0 {
		return "", errors.New("not a Navicat password")
	}
	if p, err := decryptV12(data); err == nil {
		return p, nil
	}
	return decryptV11(data), nil
}

func decryptV12(data []byte) (string, error) {
	block, _ := aes.NewCipher(v12Key)
	if len(data)%aes.BlockSize != 0 {
		return "", errors.New("not a version 12 password")
	}
	plain := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, v12IV).CryptBlocks(plain, data)
	n := int(plain[len(plain)-1])
	if n == 0 || n > aes.BlockSize {
		return "", errors.New("not a version 12 password")
	}
	for _, c := range plain[len(plain)-n:] {
		if int(c) != n {
			return "", errors.New("not a version 12 password")
		}
	}
	return string(plain[:len(plain)-n]), nil
}

// decryptV11 reverses Navicat 11: Blowfish-ECB keyed by SHA-1("3DC5CA39"),
// each block XORed with a chaining value that starts as the encryption of
// eight 0xFF bytes and takes each ciphertext block in by XOR; a short tail
// is XORed with the encrypted chaining value.
func decryptV11(data []byte) string {
	key := sha1.Sum([]byte("3DC5CA39"))
	bf, _ := blowfish.NewCipher(key[:])
	cv := make([]byte, blowfish.BlockSize)
	for i := range cv {
		cv[i] = 0xFF
	}
	bf.Encrypt(cv, cv)
	plain := make([]byte, 0, len(data))
	full := len(data) / blowfish.BlockSize * blowfish.BlockSize
	for off := 0; off < full; off += blowfish.BlockSize {
		ct := data[off : off+blowfish.BlockSize]
		p := make([]byte, blowfish.BlockSize)
		bf.Decrypt(p, ct)
		for i := range p {
			p[i] ^= cv[i]
			cv[i] ^= ct[i]
		}
		plain = append(plain, p...)
	}
	if full < len(data) {
		bf.Encrypt(cv, cv)
		for i, c := range data[full:] {
			plain = append(plain, c^cv[i])
		}
	}
	return string(plain)
}
