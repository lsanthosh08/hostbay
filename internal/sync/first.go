package sync

import (
	"encoding/base64"
	"encoding/json"
	"slices"
	"time"

	"hostbay/internal/backup"
	"hostbay/internal/store"
)

// matched collections are the ones a backup restore matches under KTD12.
// Post-login rules follow their host to the one it matched.
var matched = []string{"hosts", "folders", "keys", "scripts", "bookmarks", "postlogin"}

// firstMerge handles the first sync of a computer that already has data
// when the user picks "merge": the target's data is restored into the
// store under KTD12's matching rules, so two computers that each imported
// SSH Manager do not end up with every host twice. A target item
// matched to a local one under another ID is purged, by a tombstone the
// three-way merge then carries to the target; base is set so the purge
// counts as this computer's change.
func (e *Engine) firstMerge(d *store.Data, remotes []remote, base map[string]map[string]store.Meta, now time.Time) error {
	for _, r := range remotes {
		b, err := e.bundle(r)
		if err != nil {
			return err
		}
		before := map[string]bool{}
		for _, h := range d.Hosts.Items {
			before[h.ID] = true
		}
		for _, f := range d.Folders.Items {
			before[f.ID] = true
		}
		for _, p := range d.PostLogin.Items {
			before[p.ID] = true
		}
		for _, w := range d.Workspaces.Items {
			before[w.ID] = true
		}
		if _, _, err := backup.Restore(d, e.Vault, b); err != nil {
			return err
		}
		// Restore may point an added item at a local folder or host; that is
		// a change the other computers must see.
		for _, rec := range r.sets["hosts"].Items {
			if h := d.Host(rec.Meta.ID); h != nil && !before[h.ID] && !sameJSON(rec.Raw, h) {
				e.Store.Touch(&h.Meta)
			}
		}
		for _, rec := range r.sets["folders"].Items {
			if f := d.Folder(rec.Meta.ID); f != nil && !before[f.ID] && !sameJSON(rec.Raw, f) {
				e.Store.Touch(&f.Meta)
			}
		}
		for _, rec := range r.sets["postlogin"].Items {
			if p := d.PostLoginRule(rec.Meta.ID); p != nil && !before[p.ID] && field(rec.Raw, "hostId") != p.HostID {
				e.Store.Touch(&p.Meta)
			}
		}
		// Restore moves a new workspace's panes to the matched hosts. One
		// held here under the same ID is kept as it is; the target's copy
		// names hosts the merge purges, so it must not win.
		for _, rec := range r.sets["workspaces"].Items {
			w := d.Workspace(rec.Meta.ID)
			if w != nil && (!before[w.ID] && !sameJSON(rec.Raw, w) || before[w.ID] && namesGoneHost(d, rec.Raw)) {
				e.Store.Touch(&w.Meta)
			}
		}
		for _, c := range collections {
			if !slices.Contains(matched, c.name) {
				continue
			}
			s, wasNil := c.get(d)
			here := metas(s)
			for _, rec := range r.sets[c.name].Items {
				if _, ok := here[rec.Meta.ID]; ok {
					continue
				}
				m := rec.Meta
				base[c.name][m.ID] = m
				m.Revision++
				m.DeviceID, m.ModifiedAt = e.Store.DeviceID(), now
				s.Tombstones = append(s.Tombstones, store.Tombstone{Meta: m, Deleted: true})
			}
			if err := c.set(d, s, wasNil); err != nil {
				return err
			}
		}
	}
	return nil
}

// bundle turns a snapshot into a backup bundle with plaintext secrets, the
// form backup.Restore takes.
func (e *Engine) bundle(r remote) (backup.Bundle, error) {
	b := backup.Bundle{SchemaVersion: 1, Keys: map[string]string{}}
	hosts, err := fromSet[store.Host](r.sets["hosts"])
	if err != nil {
		return b, err
	}
	for _, h := range hosts.Items {
		pw, _ := openField(e.Key, h.Password, h.SecretAAD())
		h.Password = string(pw)
		b.Hosts = append(b.Hosts, h)
	}
	rules, err := fromSet[store.PostLoginRule](r.sets["postlogin"])
	if err != nil {
		return b, err
	}
	for _, p := range rules.Items {
		answer := ""
		for _, h := range hosts.Items {
			if h.ID == p.HostID {
				a, _ := openField(e.Key, p.Answer, store.PostLoginAAD(p.ID, &h))
				answer = string(a)
			}
		}
		p.Answer = answer
		b.PostLogin = append(b.PostLogin, p)
	}
	folders, err := fromSet[store.Folder](r.sets["folders"])
	if err != nil {
		return b, err
	}
	b.Folders = folders.Items
	scripts, err := fromSet[store.Script](r.sets["scripts"])
	if err != nil {
		return b, err
	}
	b.Scripts = scripts.Items
	marks, err := fromSet[store.Bookmark](r.sets["bookmarks"])
	if err != nil {
		return b, err
	}
	for _, m := range marks.Items {
		b.Bookmarks = append(b.Bookmarks, m.Path)
	}
	workspaces, err := fromSet[store.Workspace](r.sets["workspaces"])
	if err != nil {
		return b, err
	}
	b.Workspaces = workspaces.Items
	keys, err := fromSet[store.Key](r.sets["keys"])
	if err != nil {
		return b, err
	}
	for _, k := range keys.Items {
		if raw, ok := openField(e.Key, k.Data, store.KeyAAD(k.ID)); ok {
			b.Keys[k.Name] = base64.StdEncoding.EncodeToString(raw)
		}
	}
	return b, nil
}

// namesGoneHost reports whether a workspace record has a pane whose host
// is not in d.
func namesGoneHost(d *store.Data, raw json.RawMessage) bool {
	var w store.Workspace
	if json.Unmarshal(raw, &w) != nil {
		return false
	}
	var gone func(n store.WorkspaceNode) bool
	gone = func(n store.WorkspaceNode) bool {
		return n.HostID != "" && d.Host(n.HostID) == nil || slices.ContainsFunc(n.Children, gone)
	}
	return gone(w.Layout)
}

func sameJSON(raw json.RawMessage, v any) bool {
	b, _ := json.Marshal(v)
	return sameContent(raw, b)
}
