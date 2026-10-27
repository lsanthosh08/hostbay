package sync

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"

	"hostbay/internal/store"
)

// collection moves one store document in and out of a merge Set.
type collection struct {
	name string
	get  func(d *store.Data) (Set, bool) // bool: Items was nil (never saved)
	set  func(d *store.Data, s Set, wasNil bool) error
	// secret is the JSON field holding a sealed secret, and aad its binding.
	// d is the merged data, so a binding may follow another collection
	// merged before this one.
	secret string
	aad    func(d *store.Data, raw json.RawMessage) []byte
	// known are the JSON keys this build stores. Only other keys are kept
	// from the target's copy: a known key absent here was cleared.
	known map[string]bool
}

func coll[T any](name string, c func(d *store.Data) *store.Collection[T]) collection {
	return collection{
		name:  name,
		known: jsonKeys(reflect.TypeFor[T]()),
		get: func(d *store.Data) (Set, bool) {
			s, _ := toSet(*c(d))
			return s, c(d).Items == nil
		},
		set: func(d *store.Data, s Set, wasNil bool) error {
			out, err := fromSet[T](s)
			if err != nil {
				return err
			}
			if wasNil && len(out.Items) == 0 && len(out.Tombstones) == 0 {
				out.Items = nil // scripts and bookmarks show their defaults until first saved
			}
			*c(d) = out
			return nil
		},
	}
}

// collections lists every synced document (R35).
var collections = func() []collection {
	hosts := coll("hosts", func(d *store.Data) *store.Collection[store.Host] { return &d.Hosts })
	hosts.secret = "password"
	hosts.aad = func(_ *store.Data, raw json.RawMessage) []byte {
		var h store.Host
		_ = json.Unmarshal(raw, &h)
		return h.SecretAAD()
	}
	keys := coll("keys", func(d *store.Data) *store.Collection[store.Key] { return &d.Keys })
	keys.secret = "data"
	keys.aad = func(_ *store.Data, raw json.RawMessage) []byte {
		var m store.Meta
		_ = json.Unmarshal(raw, &m)
		return store.KeyAAD(m.ID)
	}
	proxies := coll("proxies", func(d *store.Data) *store.Collection[store.Proxy] { return &d.Proxies })
	proxies.secret = "password"
	proxies.aad = func(_ *store.Data, raw json.RawMessage) []byte {
		var p store.Proxy
		_ = json.Unmarshal(raw, &p)
		return p.SecretAAD()
	}
	// A rule's answer is bound to the merged host, so it merges after hosts
	// (KTD39). A rule whose host is gone gets a binding nothing was sealed
	// under, and its answer needs entering again.
	postlogin := coll("postlogin", func(d *store.Data) *store.Collection[store.PostLoginRule] { return &d.PostLogin })
	postlogin.secret = "answer"
	postlogin.aad = func(d *store.Data, raw json.RawMessage) []byte {
		var r store.PostLoginRule
		_ = json.Unmarshal(raw, &r)
		h := d.Host(r.HostID)
		if h == nil {
			return []byte("hostbay postlogin\x00" + r.ID + "\x00no host")
		}
		return store.PostLoginAAD(r.ID, h)
	}
	aiProviders := coll("aiproviders", func(d *store.Data) *store.Collection[store.AIProvider] { return &d.AIProviders })
	aiProviders.secret = "apiKey"
	aiProviders.aad = func(_ *store.Data, raw json.RawMessage) []byte {
		var p store.AIProvider
		_ = json.Unmarshal(raw, &p)
		return p.SecretAAD()
	}
	localOnly(&aiProviders)
	return []collection{
		hosts,
		coll("folders", func(d *store.Data) *store.Collection[store.Folder] { return &d.Folders }),
		keys,
		coll("scripts", func(d *store.Data) *store.Collection[store.Script] { return &d.Scripts }),
		coll("bookmarks", func(d *store.Data) *store.Collection[store.Bookmark] { return &d.Bookmarks }),
		coll("tunnels", func(d *store.Data) *store.Collection[store.Tunnel] { return &d.Tunnels }),
		proxies,
		coll("settings", func(d *store.Data) *store.Collection[store.SettingsGroup] { return &d.Settings }),
		postlogin,
		coll("workspaces", func(d *store.Data) *store.Collection[store.Workspace] { return &d.Workspaces }),
		aiProviders,
		coll("skills", func(d *store.Data) *store.Collection[store.Skill] { return &d.Skills }),
	}
}()

// localOnly makes the aiproviders collection leave out a Claude Code
// provider (KTD7): its program path means nothing on another computer, and a
// path from a sync target would let that target choose what Hostbay runs.
// get leaves such a record out of what is merged and uploaded; set puts the
// ones already here back after storing the merged set, so a pull replacing
// the collection does not delete them. Whatever arrives is stripped of the
// two fields that only mean something here and of any Claude Code record.
func localOnly(c *collection) {
	get, set := c.get, c.set
	c.get = func(d *store.Data) (Set, bool) {
		s, wasNil := get(d)
		s.Items = slices.DeleteFunc(s.Items, func(r Record) bool { return isClaudeCode(r.Raw) })
		return s, wasNil
	}
	c.set = func(d *store.Data, s Set, wasNil bool) error {
		var keep []store.AIProvider
		keptID := map[string]bool{}
		for _, p := range d.AIProviders.Items {
			if p.LocalOnly() {
				keep = append(keep, p)
				keptID[p.ID] = true
			}
		}
		items := make([]Record, 0, len(s.Items))
		for _, r := range s.Items {
			if keptID[r.Meta.ID] { // this computer's own provider keeps its ID
				continue
			}
			if isClaudeCode(r.Raw) {
				// A record that makes itself a Claude Code provider cannot turn
				// one of this computer's providers into one: it keeps its own.
				if p := d.AIProvider(r.Meta.ID); p != nil && !p.LocalOnly() {
					raw, _ := json.Marshal(p)
					items = append(items, Record{Meta: p.Meta, Raw: raw})
				}
				continue
			}
			r.Raw = dropKeys(r.Raw, "execPath", "consented")
			items = append(items, r)
		}
		s.Items = items
		if err := set(d, s, wasNil && len(keep) == 0); err != nil {
			return err
		}
		d.AIProviders.Items = append(d.AIProviders.Items, keep...)
		return nil
	}
}

// isClaudeCode reads a record's protocol the way fromSet will: encoding/json
// matches keys without regard to case, so an exact-case lookup could miss a
// record that still decodes as a Claude Code provider.
func isClaudeCode(raw json.RawMessage) bool {
	var p struct {
		Protocol string `json:"protocol"`
	}
	return json.Unmarshal(raw, &p) == nil && p.Protocol == store.AIClaudeCode
}

// dropKeys returns raw without the named keys, in any spelling of their case.
func dropKeys(raw json.RawMessage, names ...string) json.RawMessage {
	var f map[string]json.RawMessage
	if json.Unmarshal(raw, &f) != nil {
		return raw
	}
	for k := range f {
		for _, n := range names {
			if strings.EqualFold(k, n) {
				delete(f, k)
			}
		}
	}
	out, _ := json.Marshal(f)
	return out
}

func toSet[T any](c store.Collection[T]) (Set, error) {
	s := Set{Tombstones: slices.Clone(c.Tombstones)}
	for _, it := range c.Items {
		raw, err := json.Marshal(it)
		if err != nil {
			return s, err
		}
		r, err := record(raw)
		if err != nil {
			return s, err
		}
		s.Items = append(s.Items, r)
	}
	return s, nil
}

func fromSet[T any](s Set) (store.Collection[T], error) {
	c := store.Collection[T]{Items: []T{}, Tombstones: s.Tombstones}
	for _, r := range s.Items {
		var v T
		if err := json.Unmarshal(r.Raw, &v); err != nil {
			return c, err
		}
		c.Items = append(c.Items, v)
	}
	return c, nil
}

func record(raw json.RawMessage) (Record, error) {
	var m store.Meta
	err := json.Unmarshal(raw, &m)
	return Record{Meta: m, Raw: raw}, err
}

// field reads a string field of an item's JSON.
func field(raw json.RawMessage, key string) string {
	var f map[string]json.RawMessage
	_ = json.Unmarshal(raw, &f)
	var s string
	_ = json.Unmarshal(f[key], &s)
	return s
}

// setField returns raw with key set to value, or removed when value is "".
func setField(raw json.RawMessage, key, value string) json.RawMessage {
	var f map[string]json.RawMessage
	if json.Unmarshal(raw, &f) != nil {
		return raw
	}
	if value == "" {
		delete(f, key)
	} else {
		f[key], _ = json.Marshal(value)
	}
	out, _ := json.Marshal(f)
	return out
}

// jsonKeys lists the keys encoding/json writes for struct type t.
func jsonKeys(t reflect.Type) map[string]bool {
	keys := map[string]bool{}
	for f := range t.Fields() {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		switch {
		case name == "-" || !f.IsExported() && !f.Anonymous:
		case name == "" && f.Anonymous && f.Type.Kind() == reflect.Struct:
			for k := range jsonKeys(f.Type) {
				keys[k] = true
			}
		case name == "":
			keys[f.Name] = true
		default:
			keys[name] = true
		}
	}
	return keys
}

// overlay returns local with every key of remote that local lacks and this
// build does not know, so fields from a newer Hostbay survive this
// computer's upload while a field cleared here stays cleared.
func overlay(remote, local json.RawMessage, known map[string]bool) json.RawMessage {
	if remote == nil {
		return local
	}
	var r, l map[string]json.RawMessage
	if json.Unmarshal(remote, &r) != nil || json.Unmarshal(local, &l) != nil {
		return local
	}
	for k, v := range r {
		if _, ok := l[k]; !ok && !known[k] {
			l[k] = v
		}
	}
	out, _ := json.Marshal(l)
	return out
}

// wireSet is a collection inside the snapshot payload.
type wireSet struct {
	Items      []json.RawMessage `json:"items"`
	Tombstones []store.Tombstone `json:"tombstones,omitempty"`
}

func (w wireSet) set() (Set, error) {
	s := Set{Tombstones: w.Tombstones}
	for _, raw := range w.Items {
		r, err := record(raw)
		if err != nil {
			return s, err
		}
		s.Items = append(s.Items, r)
	}
	return s, nil
}
