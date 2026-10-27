// Package dbeaver imports DBeaver's database connections (R54, R56) from
// the data-sources.json the user picks, or the .dbeaver folder holding it,
// with the user names and passwords in credentials-config.json beside it.
// Hostbay never looks for DBeaver's folder itself.
//
// The file layout, provider names and credentials decryption (AES-128-CBC
// under DBeaver's built-in key, the file's IV as its first block) are
// adapted from uniterm's backend/importer/dbeaver.go
// (github.com/ys-ll/uniterm), Copyright 2025 uniTerm, licensed under the
// Apache License, Version 2.0. Hostbay adds the TLS, tunnel and option
// rules of KTD25 and KTD26.
package dbeaver

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"hostbay/internal/importer"
)

var credentialsKey, _ = hex.DecodeString("babb4a9f774ab853c96c2d653dfe544a")

var kinds = map[string]string{
	"mysql": "mysql", "mariadb": "mysql", "postgresql": "postgres", "postgres": "postgres", "mssql": "mssql", "sqlserver": "mssql",
	"oracle": "oracle", "sqlite": "sqlite", "redis": "redis", "mongodb": "mongodb",
}

type source struct {
	Name          string `json:"name"`
	Provider      string `json:"provider"`
	Folder        string `json:"folder"`
	Configuration struct {
		Host        string                     `json:"host"`
		Port        json.RawMessage            `json:"port"`
		Database    string                     `json:"database"`
		User        string                     `json:"user"`
		URL         string                     `json:"url"`
		LocalClient string                     `json:"local-client"`
		Properties  map[string]json.RawMessage `json:"properties"`
		Bootstrap   struct {
			InitQueries []string `json:"initQueries"`
		} `json:"bootstrap"`
		Handlers map[string]struct {
			Type       string                     `json:"type"`
			Enabled    bool                       `json:"enabled"`
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"handlers"`
	} `json:"configuration"`
}

type credentials map[string]struct {
	Connection struct {
		User     string `json:"user"`
		Password string `json:"password"`
	} `json:"#connection"`
}

// Parse reads data-sources.json at path, or in the folder path names.
// password is unused: a DBeaver master password is not supported.
func Parse(path, _ string) (importer.Result, error) {
	if path == "" {
		return importer.Result{}, errors.New("choose DBeaver's data-sources.json, in the .dbeaver folder of its workspace")
	}
	dir, file := filepath.Dir(path), path
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		dir, file = path, filepath.Join(path, "data-sources.json")
	}
	raw, err := importer.ReadFile(file)
	if err != nil {
		return importer.Result{}, err
	}
	var doc struct {
		Connections map[string]source `json:"connections"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || doc.Connections == nil {
		return importer.Result{}, errors.New("this is not DBeaver's data-sources.json")
	}

	b := importer.NewBuilder("DBeaver")
	creds := credentials{}
	if raw, err := importer.ReadFile(filepath.Join(dir, "credentials-config.json")); err == nil {
		if creds, err = open(raw); err != nil {
			creds = credentials{}
			b.Note("credentials-config.json", "DBeaver's saved passwords could not be read, as when a DBeaver master password protects them; the connections were imported without them")
		}
	}
	for _, id := range slices.Sorted(maps.Keys(doc.Connections)) {
		s := doc.Connections[id]
		alias := s.Name
		if alias == "" {
			alias = id
		}
		folder, _, _ := strings.Cut(s.Folder, "/")
		name := alias
		if folder != "" {
			name = folder + "/" + alias
		}
		kind, ok := kinds[strings.ToLower(strings.TrimSpace(s.Provider))]
		if !ok {
			b.Skip(name, fmt.Sprintf("DBeaver provider %q has no Hostbay equivalent", s.Provider))
			continue
		}
		c := s.Configuration
		if kind != "sqlite" && c.Host == "" && c.URL != "" {
			// KTD26: Hostbay never reads a connection string.
			b.Skip(name, "it is set up by JDBC URL, which Hostbay does not read; add it with its host name instead")
			continue
		}
		opts := text(c.Properties)
		tunnel := ""
		for _, hid := range slices.Sorted(maps.Keys(c.Handlers)) {
			hd := c.Handlers[hid]
			switch t := strings.ToUpper(hd.Type); {
			case !hd.Enabled:
			case t == "TUNNEL" || t == "PROXY":
				tunnel = "DBeaver's " + hid
				if host := text(hd.Properties)["host"]; host != "" {
					tunnel = fmt.Sprintf("the %s %q", strings.ReplaceAll(hid, "_", " "), host)
				}
			case t == "CONFIG" && strings.Contains(strings.ToLower(hid), "ssl"):
				maps.Copy(opts, text(hd.Properties))
			default:
				b.Skip(name+": "+hid, "this DBeaver connection setting has no Hostbay equivalent")
			}
		}
		if len(c.Bootstrap.InitQueries) > 0 {
			b.Skip(name+": bootstrap queries", "Hostbay runs no SQL when it connects")
		}
		if c.LocalClient != "" {
			b.Skip(name+": local client", "Hostbay runs no program named in another tool's settings")
		}
		cr := creds[id].Connection
		user := c.User
		if user == "" {
			user = cr.User
		}
		e := importer.Database{
			Name: name, Alias: alias, Kind: kind, Host: c.Host, Port: port(c.Port), User: user, Database: c.Database,
			Options: opts, Tunnel: tunnel, Password: cr.Password,
		}
		if folder != "" {
			e.FolderID = b.Folder([]string{folder})
		}
		if kind == "sqlite" {
			e.Path = c.Database
		}
		b.AddDatabase(e)
	}
	return b.Result(), nil
}

// open decrypts credentials-config.json: AES-128-CBC under DBeaver's key,
// whose first plaintext block is the file's IV when decrypted with a zero
// IV, then PKCS#7-padded JSON.
func open(raw []byte) (credentials, error) {
	block, _ := aes.NewCipher(credentialsKey)
	if len(raw) < 2*aes.BlockSize || len(raw)%aes.BlockSize != 0 {
		return nil, errors.New("damaged")
	}
	plain := make([]byte, len(raw))
	cipher.NewCBCDecrypter(block, make([]byte, aes.BlockSize)).CryptBlocks(plain, raw)
	plain = plain[aes.BlockSize:]
	n := int(plain[len(plain)-1])
	if n == 0 || n > aes.BlockSize {
		return nil, errors.New("damaged")
	}
	var c credentials
	if err := json.Unmarshal(plain[:len(plain)-n], &c); err != nil {
		return nil, err
	}
	return c, nil
}

// text turns option values, strings or numbers, into strings.
func text(m map[string]json.RawMessage) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		var s string
		if json.Unmarshal(v, &s) != nil {
			s = string(v)
		}
		out[k] = s
	}
	return out
}

// port reads DBeaver's port, a string or a number; 0 when unset.
func port(raw json.RawMessage) int {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	n, _ := strconv.Atoi(s)
	return n
}
