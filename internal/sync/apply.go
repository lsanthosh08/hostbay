package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"hostbay/internal/apperr"
	"hostbay/internal/store"
)

// tombstoneKeep is the least time a tombstone is kept, on top of waiting
// until every known computer has synced past it.
const tombstoneKeep = 30 * 24 * time.Hour

func (e *Engine) once(ctx context.Context, choice string, rep *Report) error {
	remotes, version, seen, mine, err := e.pull(ctx)
	if err != nil {
		return err
	}
	now, me := e.now(), e.Store.DeviceID()
	devices := maps.Clone(e.State.Devices)
	if devices == nil {
		devices = map[string]DeviceMark{}
	}
	for _, r := range remotes {
		for d, mark := range r.m.Devices {
			if cur, ok := devices[d]; !ok || mark.Generation > cur.Generation {
				devices[d] = mark
			}
		}
	}

	var up map[string]Set
	var conflicts []Conflict
	var holds []Hold
	err = e.Store.Edit(func(d *store.Data) error {
		up, conflicts, holds = map[string]Set{}, nil, nil
		base := map[string]map[string]store.Meta{}
		for _, c := range collections {
			base[c.name] = maps.Clone(e.State.Base[c.name])
			if base[c.name] == nil {
				base[c.name] = map[string]store.Meta{}
			}
		}
		// Before the first-sync merge and the holds lift: this computer's own
		// settings, so hosts a "merge" restores from the target are held too.
		was := confirmed(d)
		replace := false
		if !e.State.Synced && remoteHasData(remotes) && !emptyData(d) {
			switch choice {
			case "":
				return ErrChoiceNeeded
			case "remote":
				replace = true
			case "merge":
				if err := e.firstMerge(d, remotes, base, now); err != nil {
					return err
				}
			default:
				return fmt.Errorf("unknown first-sync choice %q", choice)
			}
		}
		e.liftHolds(d)

		rep.Received = 0
		for _, c := range collections {
			local, wasNil := c.get(d)
			pre := map[string]Record{}
			for _, r := range local.Items {
				pre[r.Meta.ID] = r
			}
			if replace {
				local = Set{}
			}
			merged := local
			for _, r := range remotes {
				var cs []Conflict
				merged, cs = Merge(c.name, base[c.name], merged, r.sets[c.name], now)
				conflicts = append(conflicts, cs...)
			}
			merged.Tombstones = pruneTombstones(merged.Tombstones, devices, now)
			for _, it := range merged.Items {
				if p, ok := pre[it.Meta.ID]; !ok || !sameMeta(p.Meta, it.Meta) {
					rep.Received++
				}
			}
			if c.secret != "" {
				if merged, err = e.rewrapIn(d, c, merged, pre, remotes); err != nil {
					return err
				}
			}
			if err := c.set(d, merged, wasNil); err != nil {
				return err
			}
		}
		rep.Notes = Repair(d)
		for _, c := range collections {
			up[c.name], _ = c.get(d)
		}
		holds = holdNew(d, was)
		return nil
	})
	if err != nil {
		return err
	}
	e.State.Holds = append(holds, unknownHolds(e.State.Holds)...)
	e.addConflicts(conflicts)
	rep.Conflicts = len(e.State.Conflicts)

	// Bindings on upload follow the hosts as uploaded, not this computer's
	// stand-ins for held settings.
	hosts, err := fromSet[store.Host](up["hosts"])
	if err != nil {
		return err
	}
	final := &store.Data{Hosts: hosts}
	payload := map[string]json.RawMessage{}
	if len(remotes) > 0 {
		payload = maps.Clone(remotes[0].payload) // keeps parts from a newer Hostbay
	}
	for _, c := range collections {
		s, err := e.wrapOut(final, c, up[c.name], remotes)
		if err != nil {
			return err
		}
		w := wireSet{Items: []json.RawMessage{}, Tombstones: s.Tombstones}
		for _, r := range s.Items {
			w.Items = append(w.Items, r.Raw)
		}
		if payload[c.name], err = json.Marshal(w); err != nil {
			return err
		}
	}
	plain, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	gen := max(e.State.Generation, seen[me], mine) + 1
	devices[me] = DeviceMark{Generation: gen, SyncedAt: now}
	m := Manifest{KeyID: e.KeyID, KDF: e.KDF, DeviceID: me, Generation: gen, Devices: devices, CreatedAt: now}
	if len(remotes) > 0 {
		m.extra = remotes[0].m.extra
	}
	blob, err := Seal(m, plain, e.Key)
	if err != nil {
		return err
	}
	name := SharedName
	if e.Backend.PerDevice() {
		name = deviceFileName(me, gen)
	}
	if err := e.Backend.Store(ctx, name, blob, version); err != nil {
		return err
	}
	rep.Uploaded = true

	e.State.Base = map[string]map[string]store.Meta{}
	for _, c := range collections {
		e.State.Base[c.name] = metas(up[c.name])
	}
	seen[me] = gen
	e.State.Seen, e.State.Generation, e.State.Devices, e.State.Synced = seen, gen, devices, true
	if err := e.Backend.Prune(ctx); err != nil {
		rep.Notes = append(rep.Notes, "Old sync files were not removed: "+err.Error())
	}
	e.State.Version, _ = e.Backend.Version(ctx)
	return nil
}

func metas(s Set) map[string]store.Meta {
	m := map[string]store.Meta{}
	for _, t := range s.Tombstones {
		m[t.ID] = t.Meta
	}
	for _, r := range s.Items {
		m[r.Meta.ID] = r.Meta
	}
	return m
}

// Settings alone do not count as data for the first-sync choice: each
// group has the same ID everywhere, so the three-way merge joins them.
func remoteHasData(remotes []remote) bool {
	for _, r := range remotes {
		for name, s := range r.sets {
			if len(s.Items) > 0 && name != "settings" {
				return true
			}
		}
	}
	return false
}

func emptyData(d *store.Data) bool {
	for _, c := range collections {
		if s, _ := c.get(d); len(s.Items) > 0 && c.name != "settings" {
			return false
		}
	}
	return true
}

// pruneTombstones drops a tombstone once it is old and every known
// computer has synced since it was written.
func pruneTombstones(ts []store.Tombstone, devices map[string]DeviceMark, now time.Time) []store.Tombstone {
	var out []store.Tombstone
	for _, t := range ts {
		keep := len(devices) == 0 || now.Sub(t.ModifiedAt) < tombstoneKeep
		for _, mark := range devices {
			keep = keep || mark.SyncedAt.Before(t.ModifiedAt)
		}
		if keep {
			out = append(out, t)
		}
	}
	return out
}

// open decrypts a locally sealed value. ok is false for a value that is
// damaged or bound elsewhere; err is set only when the master key itself
// cannot be had, which stops the sync rather than dropping secrets.
func (e *Engine) open(sealed string, aad []byte) (plain []byte, ok bool, err error) {
	plain, err = e.Vault.Open(sealed, aad)
	if err == nil {
		return plain, true, nil
	}
	var ae *apperr.Error
	if errors.As(err, &ae) && ae.Code == apperr.SecretNeeded && ae.Details["reason"] == "decrypt" {
		return nil, false, nil
	}
	return nil, false, err
}

// rewrapIn re-seals the merged collection's secrets under this computer's
// master key (KTD13). A secret from another computer opens only with the
// binding of the merged item, so a host pointed elsewhere asks for its
// password again (R29). A local secret that no longer opens is repaired
// from the target when the target's copy has the same binding.
func (e *Engine) rewrapIn(d *store.Data, c collection, s Set, pre map[string]Record, remotes []remote) (Set, error) {
	for i, r := range s.Items {
		v, aad := field(r.Raw, c.secret), c.aad(d, r.Raw)
		switch {
		case v == "":
		case strings.HasPrefix(v, fieldPrefix) || v == unavailable:
			if plain, ok := openField(e.Key, v, aad); ok {
				sealed, err := e.Vault.Seal(plain, aad)
				if err != nil {
					return s, err
				}
				v = sealed
				break
			}
			v = ""
			if p, ok := pre[r.Meta.ID]; ok {
				if lv := field(p.Raw, c.secret); lv != "" && !strings.HasPrefix(lv, fieldPrefix) {
					if _, ok, err := e.open(lv, aad); err != nil {
						return s, err
					} else if ok {
						v = lv
					}
				}
			}
		default:
			_, ok, err := e.open(v, aad)
			if err != nil {
				return s, err
			}
			if ok {
				break
			}
			for _, rem := range remotes {
				if plain, ok := openField(e.Key, remoteField(rem, c, r.Meta.ID), aad); ok {
					if v, err = e.Vault.Seal(plain, aad); err != nil {
						return s, err
					}
					break
				}
			}
		}
		s.Items[i].Raw = setField(r.Raw, c.secret, v)
	}
	return s, nil
}

func remoteRaw(rem remote, c collection, id string) json.RawMessage {
	for _, r := range rem.sets[c.name].Items {
		if r.Meta.ID == id {
			return r.Raw
		}
	}
	return nil
}

func remoteField(rem remote, c collection, id string) string {
	return field(remoteRaw(rem, c, id), c.secret)
}

// wrapOut prepares a collection for upload: secrets move from the master
// key to the sync key with their binding, and fields only a newer Hostbay
// knows are kept from the target's copy. A secret this computer cannot
// open is never uploaded as a change: the target's copy is carried on, or
// it is marked unavailable so other computers keep theirs.
func (e *Engine) wrapOut(d *store.Data, c collection, s Set, remotes []remote) (Set, error) {
	out := Set{Tombstones: s.Tombstones}
	for _, r := range s.Items {
		var theirs json.RawMessage
		for _, rem := range remotes {
			if theirs = remoteRaw(rem, c, r.Meta.ID); theirs != nil {
				break
			}
		}
		raw := overlay(theirs, r.Raw, c.known)
		if c.secret != "" {
			v, aad := field(r.Raw, c.secret), c.aad(d, r.Raw)
			if v != "" {
				plain, ok, err := e.open(v, aad)
				if err != nil {
					return out, err
				}
				switch {
				case ok:
					if v, err = sealField(e.Key, plain, aad); err != nil {
						return out, err
					}
				case theirs != nil && validField(e.Key, field(theirs, c.secret), aad):
					v = field(theirs, c.secret)
				default:
					v = unavailable
				}
			}
			raw = setField(raw, c.secret, v)
		}
		out.Items = append(out.Items, Record{Meta: r.Meta, Raw: raw})
	}
	return out, nil
}

func validField(key []byte, v string, aad []byte) bool {
	_, ok := openField(key, v, aad)
	return ok
}

// addConflicts records new conflicts once each. Secrets sealed under the
// sync key are dropped from them: a restored version asks for its
// password again.
func (e *Engine) addConflicts(cs []Conflict) {
	for _, c := range cs {
		for _, col := range collections {
			if col.name == c.Collection && col.secret != "" && c.Raw != nil {
				if v := field(c.Raw, col.secret); v != "" && !strings.HasPrefix(v, "enc:") {
					c.Raw = setField(c.Raw, col.secret, "")
				}
			}
		}
		dup := false
		for _, old := range e.State.Conflicts {
			dup = dup || (old.Collection == c.Collection && old.ItemID == c.ItemID && old.Kind == c.Kind &&
				old.DeviceID == c.DeviceID && old.ModifiedAt.Equal(c.ModifiedAt))
		}
		if !dup {
			e.State.Conflicts = append(e.State.Conflicts, c)
		}
	}
}
