package sync

import (
	"errors"
	"slices"

	"hostbay/internal/store"
)

func (e *Engine) conflict(id string) (int, Conflict, error) {
	for i, c := range e.State.Conflicts {
		if c.ID == id {
			return i, c, nil
		}
	}
	return -1, Conflict{}, errors.New("conflict not found")
}

// RestoreConflict brings back the listed losing side as a new local change,
// so it wins the next sync: the losing version for an edit, the purge for a
// purge.
func (e *Engine) RestoreConflict(id string) error {
	i, c, err := e.conflict(id)
	if err != nil {
		return err
	}
	if c.Collection == "settings" && c.ItemID == store.GroupAI {
		return errors.New("the AI execution mode cannot be restored here") // only its own verified path sets it (KTD37)
	}
	col := slices.IndexFunc(collections, func(x collection) bool { return x.name == c.Collection })
	if col < 0 {
		return errors.New("unknown collection " + c.Collection)
	}
	err = e.Store.Edit(func(d *store.Data) error {
		cl := collections[col]
		s, wasNil := cl.get(d)
		cur := store.Meta{ID: c.ItemID}
		at := slices.IndexFunc(s.Items, func(r Record) bool { return r.Meta.ID == c.ItemID })
		if at >= 0 {
			cur = s.Items[at].Meta
		}
		if t := slices.IndexFunc(s.Tombstones, func(t store.Tombstone) bool { return t.ID == c.ItemID }); t >= 0 {
			cur.Revision = max(cur.Revision, s.Tombstones[t].Revision)
			s.Tombstones = slices.Delete(s.Tombstones, t, t+1)
		}
		e.Store.Touch(&cur) // one past what is here now, so it outranks it
		if c.Kind == "purge" {
			if at >= 0 {
				s.Items = slices.Delete(s.Items, at, at+1)
			}
			s.Tombstones = append(s.Tombstones, store.Tombstone{Meta: cur, Deleted: true})
		} else {
			r := Record{Meta: cur, Raw: withMeta(c.Raw, cur)}
			if at >= 0 {
				s.Items[at] = r
			} else {
				s.Items = append(s.Items, r)
			}
		}
		return cl.set(d, s, wasNil)
	})
	if err != nil {
		return err
	}
	e.State.Conflicts = slices.Delete(e.State.Conflicts, i, i+1)
	return SaveState(e.Store, e.State)
}

// DismissConflict removes a conflict from the list and keeps what won.
func (e *Engine) DismissConflict(id string) error {
	i, _, err := e.conflict(id)
	if err != nil {
		return err
	}
	e.State.Conflicts = slices.Delete(e.State.Conflicts, i, i+1)
	return SaveState(e.Store, e.State)
}
