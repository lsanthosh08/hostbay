package store

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// KeyRefPrefix marks a host IdentityFile that names a key in the key store,
// as SSH Manager's backups already do.
const KeyRefPrefix = "keystore:"

// Key is a private key copied into Hostbay's key store. Data is the key file
// sealed under KeyAAD, so no key sits on disk in readable form (KTD6).
type Key struct {
	Meta
	Name string `json:"name"`
	Data string `json:"data"`
}

// KeyAAD binds a sealed key file to its record ID, which never changes, so
// the key opens whatever host uses it and a ciphertext moved to another
// record does not.
func KeyAAD(id string) []byte { return []byte("hostbay identity key\x00" + id) }

// KeyByName returns the key named name, or nil.
func (d *Data) KeyByName(name string) *Key {
	for i := range d.Keys.Items {
		if d.Keys.Items[i].Name == name {
			return &d.Keys.Items[i]
		}
	}
	return nil
}

// KeyNames lists the key store, sorted.
func (d *Data) KeyNames() []string {
	names := []string{}
	for _, k := range d.Keys.Items {
		names = append(names, k.Name)
	}
	slices.Sort(names)
	return names
}

// AddKey stores k under a name not yet taken (see UniqueKeyName) and returns
// it. k.ID must already be the ID its Data is sealed to.
func (d *Data) AddKey(k Key) (Key, error) {
	if k.ID == "" {
		return Key{}, errors.New("key must be sealed to its ID before it is added")
	}
	k.Name = d.UniqueKeyName(k.Name)
	return add(d, &d.Keys, k), nil
}

// DeleteKey removes the key named name. Hosts that use it keep the reference.
func (d *Data) DeleteKey(name string) {
	if k := d.KeyByName(name); k != nil {
		purge(d, &d.Keys, k.ID)
	}
}

// UniqueKeyName returns base, or base with a number before its extension
// when taken (foo -> foo-1, a.pem -> a-1.pem), as SSH Manager numbers them.
func (d *Data) UniqueKeyName(base string) string {
	if base == "" {
		base = "id_key"
	}
	if d.KeyByName(base) == nil {
		return base
	}
	stem, ext := base, ""
	if i := strings.LastIndex(base, "."); i >= 0 {
		stem, ext = base[:i], base[i:]
	}
	for i := 1; ; i++ {
		if c := fmt.Sprintf("%s-%d%s", stem, i, ext); d.KeyByName(c) == nil {
			return c
		}
	}
}
