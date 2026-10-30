package netd

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRestartSweepsJournaledRoutes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.json")
	added := []SysRoute{sr("10.1.0.0/16", "utun3"), sr("10.2.0.0/16", "utun3"), {Dst: sr("203.0.113.7/32", "").Dst, Iface: "en0", Gateway: lanGateway}}
	j, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range added {
		if err := j.record(Added, r); err != nil {
			t.Fatal(err)
		}
	}

	// The helper died with the three routes in place; a new one starts.
	table := newFakeTable(append([]SysRoute{defaultRoute, lanRoute}, added...)...)
	j, err = OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Sweep(table, j); err != nil {
		t.Fatal(err)
	}
	if got := table.snapshot(); !slices.Equal(got, []SysRoute{defaultRoute, lanRoute}) {
		t.Errorf("routes after the restart sweep = %v", got)
	}
	if j, _ = OpenJournal(path); len(j.entries) != 0 {
		t.Errorf("journal on disk keeps %v", j.entries)
	}
}

func TestSweepToleratesRoutesAlreadyGone(t *testing.T) {
	j, _ := OpenJournal(filepath.Join(t.TempDir(), "journal.json"))
	j.record(Added, sr("10.1.0.0/16", "utun3"))
	j.record(Deleted, sr("10.0.0.0/8", "utun5"))
	table := newFakeTable(sr("10.0.0.0/8", "utun5")) // vendor already re-added it
	if err := Sweep(table, j); err != nil {
		t.Fatal(err)
	}
	if len(j.entries) != 0 || !slices.Equal(table.snapshot(), []SysRoute{sr("10.0.0.0/8", "utun5")}) {
		t.Errorf("entries %v, table %v", j.entries, table.snapshot())
	}
}

// The journal holds a route before the system does, so a crash between the
// two still leaves the sweep something to undo.
func TestJournalWrittenBeforeTheRouteLands(t *testing.T) {
	table := newFakeTable(defaultRoute)
	s := newServer(t, table)
	path := s.Journal.path
	var onDisk string
	table.onAdd = func(SysRoute) {
		b, _ := os.ReadFile(path)
		onDisk = string(b)
	}
	conn, done := s.pipe()
	c, err := newClient(conn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { c.Close(); <-done }()
	if err := c.Add(t.Context(), Route{Dst: "10.20.0.0/16", Iface: "utun3"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(onDisk, "10.20.0.0/16") {
		t.Errorf("journal before the add = %q", onDisk)
	}
}

// Hostbay deletes a vendor route and then adds the very same route itself:
// undoing newest first deletes Hostbay's and then puts the vendor's back.
func TestSweepUndoesNewestFirst(t *testing.T) {
	vendor := sr("10.0.0.0/8", "utun5")
	j, _ := OpenJournal(filepath.Join(t.TempDir(), "journal.json"))
	j.record(Deleted, vendor)
	j.record(Added, vendor)
	table := newFakeTable(vendor)
	if err := Sweep(table, j); err != nil {
		t.Fatal(err)
	}
	if !table.has(vendor) {
		t.Errorf("the vendor route is gone: %v", table.snapshot())
	}
}
