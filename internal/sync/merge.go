package sync

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"hostbay/internal/store"
)

// Record is one stored item as merge sees it: its sync Meta and its whole
// JSON, which may carry fields this build does not know.
type Record struct {
	Meta store.Meta
	Raw  json.RawMessage
}

// Set is one collection: live items plus tombstones of purged ones.
type Set struct {
	Items      []Record
	Tombstones []store.Tombstone
}

// Conflict is the losing side of an item both computers changed (R37). For
// Kind "edit" Raw is the losing version, which RestoreConflict brings back;
// for "purge" the losing side purged the item and restoring purges it.
type Conflict struct {
	ID         string          `json:"id"`
	Collection string          `json:"collection"`
	ItemID     string          `json:"itemId"`
	Label      string          `json:"label"`
	Kind       string          `json:"kind"`
	Raw        json.RawMessage `json:"raw,omitempty"`
	DeviceID   string          `json:"deviceId"`
	ModifiedAt time.Time       `json:"modifiedAt"`
	At         time.Time       `json:"at"`
}

// side is one computer's state of an item.
type side struct {
	present bool
	dead    bool // a tombstone
	meta    store.Meta
	raw     json.RawMessage
}

// changedSince reports whether s holds a change the base has not seen. A
// revision below the base is a stale copy (a per-device file written before
// this computer's last sync), not a change.
func (s side) changedSince(b store.Meta, inBase bool) bool {
	switch {
	case !s.present:
		return false
	case !inBase:
		return true
	case s.meta.Revision != b.Revision:
		return s.meta.Revision > b.Revision
	default:
		return s.meta.DeviceID != b.DeviceID
	}
}

// Merge combines local and remote against base, the Meta of every item as
// of the last sync (KTD13). A side that alone changed an item wins whatever
// the clocks say. When both did, the newer time wins (future times clamped
// to now, ties to the higher device ID) at the higher revision, so it
// outranks both on every computer, and the other side is returned as a
// conflict. An edit always beats a purge.
func Merge(collection string, base map[string]store.Meta, local, remote Set, now time.Time) (Set, []Conflict) {
	ls, rs := sides(local), sides(remote)
	var order []string
	seen := map[string]bool{}
	for _, s := range []Set{local, remote} {
		for _, r := range s.Items {
			if !seen[r.Meta.ID] {
				seen[r.Meta.ID] = true
				order = append(order, r.Meta.ID)
			}
		}
		for _, t := range s.Tombstones {
			if !seen[t.ID] {
				seen[t.ID] = true
				order = append(order, t.ID)
			}
		}
	}

	var out Set
	var conflicts []Conflict
	for _, id := range order {
		l, r := ls[id], rs[id]
		b, inBase := base[id]
		win, lose := l, side{}
		switch lc, rc := l.changedSince(b, inBase), r.changedSince(b, inBase); {
		case !lc && !rc:
			if !l.present {
				win = r
			}
		case lc && !rc:
		case rc && !lc:
			win = r
		case sameMeta(l.meta, r.meta):
		default:
			win, lose = resolve(l, r, now)
		}
		if lose.present && !(lose.dead && win.dead) && !sameContent(win.raw, lose.raw) {
			conflicts = append(conflicts, conflictFor(collection, id, win, lose, now))
		}
		if win.dead {
			out.Tombstones = append(out.Tombstones, store.Tombstone{Meta: win.meta, Deleted: true})
		} else {
			out.Items = append(out.Items, Record{Meta: win.meta, Raw: withMeta(win.raw, win.meta)})
		}
	}
	return out, conflicts
}

// resolve picks the winner of a change made on both sides.
func resolve(l, r side, now time.Time) (win, lose side) {
	switch {
	case l.dead && !r.dead:
		win, lose = r, l
	case r.dead && !l.dead:
		win, lose = l, r
	default:
		lt, rt := clamp(l.meta.ModifiedAt, now), clamp(r.meta.ModifiedAt, now)
		if rt.After(lt) || (rt.Equal(lt) && r.meta.DeviceID > l.meta.DeviceID) {
			win, lose = r, l
		} else {
			win, lose = l, r
		}
	}
	win.meta.Revision = max(l.meta.Revision, r.meta.Revision)
	return win, lose
}

func sameMeta(a, b store.Meta) bool {
	return a.ID == b.ID && a.Revision == b.Revision && a.DeviceID == b.DeviceID && a.ModifiedAt.Equal(b.ModifiedAt)
}

func clamp(t, now time.Time) time.Time {
	if t.After(now) {
		return now
	}
	return t
}

func sides(s Set) map[string]side {
	m := map[string]side{}
	for _, t := range s.Tombstones {
		m[t.ID] = side{present: true, dead: true, meta: t.Meta}
	}
	for _, r := range s.Items {
		m[r.Meta.ID] = side{present: true, meta: r.Meta, raw: r.Raw}
	}
	return m
}

func conflictFor(collection, id string, win, lose side, now time.Time) Conflict {
	c := Conflict{ID: rand.Text(), Collection: collection, ItemID: id, Kind: "edit", DeviceID: lose.meta.DeviceID, ModifiedAt: lose.meta.ModifiedAt, At: now}
	if lose.dead {
		c.Kind, c.Label = "purge", label(win.raw)
	} else {
		c.Raw, c.Label = lose.raw, label(lose.raw)
	}
	return c
}

// label names an item for the conflicts list.
func label(raw json.RawMessage) string {
	var f map[string]any
	_ = json.Unmarshal(raw, &f)
	for _, k := range []string{"alias", "name", "label", "path"} {
		if s, ok := f[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// metaKeys and secretKeys are left out when comparing two versions: Meta
// always differs, a sealed secret differs per seal, and lastConnected is a
// computer's own bookkeeping, stamped without touching Meta.
var metaKeys = []string{"id", "revision", "deviceId", "modifiedAt", "lastConnected"}
var secretKeys = []string{"password", "data"}

// sameContent reports whether two versions differ only in Meta or in how a
// secret was sealed.
func sameContent(a, b json.RawMessage) bool {
	strip := func(raw json.RawMessage) []byte {
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) != nil {
			return raw
		}
		for _, k := range slices.Concat(metaKeys, secretKeys) {
			delete(m, k)
		}
		out, _ := json.Marshal(m)
		return out
	}
	return bytes.Equal(strip(a), strip(b))
}

// withMeta returns raw with its Meta fields set to m.
func withMeta(raw json.RawMessage, m store.Meta) json.RawMessage {
	var f map[string]json.RawMessage
	if json.Unmarshal(raw, &f) != nil {
		return raw
	}
	mb, _ := json.Marshal(m)
	var mf map[string]json.RawMessage
	_ = json.Unmarshal(mb, &mf)
	for k, v := range mf {
		f[k] = v
	}
	out, _ := json.Marshal(f)
	return out
}

// Repair fixes references a merge can break and returns a note for each
// (KTD13). A host or folder whose folder is gone moves to the top level, a
// folder cycle is cut at its member with the lowest ID (so every computer
// cuts the same one), and a missing jump host is cleared. Repairs do not
// touch Meta: every computer makes the same ones.
func Repair(d *store.Data) []string {
	var notes []string
	for i := range d.Folders.Items {
		f := &d.Folders.Items[i]
		if f.ParentID != nil && d.Folder(*f.ParentID) == nil {
			f.ParentID = nil
			notes = append(notes, fmt.Sprintf("Folder %q moved to the top level: its parent is gone.", f.Name))
		}
	}
	for i := range d.Folders.Items {
		var cycle []string
		for id, steps := d.Folders.Items[i].ID, 0; steps <= len(d.Folders.Items); steps++ {
			f := d.Folder(id)
			if f == nil || f.ParentID == nil {
				break
			}
			if slices.Contains(cycle, id) {
				cycle = cycle[slices.Index(cycle, id):]
				cut := d.Folder(slices.Min(cycle))
				cut.ParentID = nil
				notes = append(notes, fmt.Sprintf("Folder %q moved to the top level to break a loop.", cut.Name))
				break
			}
			cycle = append(cycle, id)
			id = *f.ParentID
		}
	}
	for i := range d.Hosts.Items {
		h := &d.Hosts.Items[i]
		if h.FolderID != nil && d.Folder(*h.FolderID) == nil {
			h.FolderID = nil
			notes = append(notes, fmt.Sprintf("Host %q moved to the top level: its folder is gone.", h.Alias))
		}
		if h.JumpHostID != nil && d.Host(*h.JumpHostID) == nil {
			h.JumpHostID = nil
			notes = append(notes, fmt.Sprintf("Host %q lost its jump host, which is gone.", h.Alias))
		}
	}
	return notes
}
