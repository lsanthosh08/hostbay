package netd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
)

// Journal operations.
const (
	Added   = "added"   // the helper added this route; the sweep deletes it
	Deleted = "deleted" // the helper deleted this vendor route; the sweep restores it
)

type journalEntry struct {
	Op    string   `json:"op"`
	Route SysRoute `json:"route"`
}

// Journal records every route change the helper makes, on disk and before
// making it, so the sweep can undo them after a disconnect or a crash.
type Journal struct {
	path    string
	entries []journalEntry
}

// OpenJournal reads the journal at path; a missing file is an empty journal.
func OpenJournal(path string) (*Journal, error) {
	j := &Journal{path: path}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return j, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &j.entries); err != nil {
		return nil, fmt.Errorf("reading the route journal %s: %w", path, err)
	}
	return j, nil
}

func (j *Journal) record(op string, r SysRoute) error {
	j.entries = append(j.entries, journalEntry{op, r})
	if err := j.save(); err != nil {
		j.entries = j.entries[:len(j.entries)-1]
		return err
	}
	return nil
}

// forget drops the newest entry for r, once the change it records is undone
// or never happened.
func (j *Journal) forget(op string, r SysRoute) error {
	for i := len(j.entries) - 1; i >= 0; i-- {
		if j.entries[i] == (journalEntry{op, r}) {
			j.entries = slices.Delete(j.entries, i, i+1)
			return j.save()
		}
	}
	return nil
}

// added finds a route the helper added to req.Dst, on req.Iface or, with
// req.Gateway, through the default gateway.
func (j *Journal) added(req Route) (SysRoute, bool) {
	for i := len(j.entries) - 1; i >= 0; i-- {
		e := j.entries[i]
		via := e.Route.Iface == req.Iface
		if req.Gateway {
			via = e.Route.Gateway.IsValid()
		}
		if e.Op == Added && e.Route.Dst.String() == req.Dst && via {
			return e.Route, true
		}
	}
	return SysRoute{}, false
}

// save replaces the file whole, synced before the rename, so a crash leaves
// either the old journal or the new one.
func (j *Journal) save() error {
	b, err := json.Marshal(j.entries)
	if err != nil {
		return err
	}
	return writeFile(j.path, b, 0o600)
}

// Sweep undoes the journal newest first, so a narrowed vendor route loses
// its narrow replacement before it comes back: it deletes each route the
// helper added and restores each vendor route it deleted. A route already
// gone, or already back, counts as done. Entries that fail stay for the next
// sweep.
func Sweep(t Table, j *Journal) error {
	if len(j.entries) == 0 {
		return nil
	}
	routes, err := t.Routes()
	if err != nil {
		return err
	}
	var kept []journalEntry
	var errs []error
	for i := len(j.entries) - 1; i >= 0; i-- {
		e := j.entries[i]
		present := slices.Contains(routes, e.Route)
		var err error
		switch {
		case e.Op == Added && present:
			if err = t.Delete(e.Route); err == nil {
				routes = slices.DeleteFunc(routes, func(r SysRoute) bool { return r == e.Route })
			}
		case e.Op == Deleted && !present:
			if err = t.Add(e.Route); err == nil {
				routes = append(routes, e.Route)
			}
		}
		if err != nil {
			kept = append([]journalEntry{e}, kept...)
			errs = append(errs, fmt.Errorf("undoing %s %s: %w", e.Op, e.Route, err))
		}
	}
	j.entries = kept
	if err := j.save(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
