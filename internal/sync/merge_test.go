package sync

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"hostbay/internal/store"
)

var t0 = time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)

func meta(id string, rev int64, dev string, at time.Time) store.Meta {
	return store.Meta{ID: id, Revision: rev, DeviceID: dev, ModifiedAt: at}
}

// host is a hosts-collection record with alias as its only field.
func host(m store.Meta, alias string) Record {
	raw, _ := json.Marshal(store.Host{Meta: m, Alias: alias, Hostname: "10.0.0.1", Port: 22, User: "root"})
	return Record{Meta: m, Raw: raw}
}

func alias(t *testing.T, r Record) string {
	t.Helper()
	var h store.Host
	if err := json.Unmarshal(r.Raw, &h); err != nil {
		t.Fatal(err)
	}
	return h.Alias
}

func baseOf(ms ...store.Meta) map[string]store.Meta {
	b := map[string]store.Meta{}
	for _, m := range ms {
		b[m.ID] = m
	}
	return b
}

func only(t *testing.T, s Set) Record {
	t.Helper()
	if len(s.Items) != 1 {
		t.Fatalf("got %d items, want 1: %+v", len(s.Items), s.Items)
	}
	return s.Items[0]
}

// Covers AE14: both computers renamed one host; the newer name wins and the
// other is listed in conflicts, whichever side merges.
func TestMergeConcurrentEditsNewerWinsOlderIsAConflict(t *testing.T) {
	b := meta("h", 1, "A", t0)
	onA := host(meta("h", 2, "A", t0.Add(time.Minute)), "web-a")
	onB := host(meta("h", 2, "B", t0.Add(2*time.Minute)), "web-b")
	for _, tc := range []struct {
		name          string
		local, remote Record
	}{{"B merges", onB, onA}, {"A merges", onA, onB}} {
		t.Run(tc.name, func(t *testing.T) {
			out, conflicts := Merge("hosts", baseOf(b), Set{Items: []Record{tc.local}}, Set{Items: []Record{tc.remote}}, t0.Add(time.Hour))
			if got := alias(t, only(t, out)); got != "web-b" {
				t.Fatalf("winner %q, want web-b", got)
			}
			if len(conflicts) != 1 || conflicts[0].Kind != "edit" || conflicts[0].Label != "web-a" || conflicts[0].ItemID != "h" {
				t.Fatalf("conflicts = %+v, want the web-a edit", conflicts)
			}
		})
	}
}

func TestMergeEditOnOneSideWinsWhateverTheClock(t *testing.T) {
	now := t0.Add(2 * time.Hour)
	b := meta("h", 1, "A", t0.Add(time.Hour))
	// B's clock is an hour slow, so its edit carries a time before the base.
	edited := host(meta("h", 2, "B", t0), "renamed-on-b")
	unchanged := host(b, "old")
	for _, tc := range []struct {
		name          string
		local, remote Record
	}{{"edit is local", edited, unchanged}, {"edit is remote", unchanged, edited}} {
		t.Run(tc.name, func(t *testing.T) {
			out, conflicts := Merge("hosts", baseOf(b), Set{Items: []Record{tc.local}}, Set{Items: []Record{tc.remote}}, now)
			if got := alias(t, only(t, out)); got != "renamed-on-b" || len(conflicts) != 0 {
				t.Fatalf("got %q with %d conflicts, want renamed-on-b and none", got, len(conflicts))
			}
		})
	}
}

func TestMergePurgeOnOneSideStaysGone(t *testing.T) {
	b := meta("h", 1, "B", t0)
	tomb := store.Tombstone{Meta: meta("h", 2, "A", t0.Add(time.Minute)), Deleted: true}
	out, conflicts := Merge("hosts", baseOf(b), Set{Items: []Record{host(b, "web")}}, Set{Tombstones: []store.Tombstone{tomb}}, t0.Add(time.Hour))
	if len(out.Items) != 0 || len(out.Tombstones) != 1 || len(conflicts) != 0 {
		t.Fatalf("got %+v, %+v; want only the tombstone", out, conflicts)
	}
}

func TestMergePurgeAgainstEditKeepsTheEditAndListsThePurge(t *testing.T) {
	b := meta("h", 1, "A", t0)
	tomb := store.Tombstone{Meta: meta("h", 2, "A", t0.Add(2*time.Minute)), Deleted: true}
	edit := host(meta("h", 2, "B", t0.Add(time.Minute)), "edited")
	for _, tc := range []struct {
		name          string
		local, remote Set
	}{
		{"purge is remote", Set{Items: []Record{edit}}, Set{Tombstones: []store.Tombstone{tomb}}},
		{"purge is local", Set{Tombstones: []store.Tombstone{tomb}}, Set{Items: []Record{edit}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, conflicts := Merge("hosts", baseOf(b), tc.local, tc.remote, t0.Add(time.Hour))
			got := only(t, out)
			if alias(t, got) != "edited" || len(out.Tombstones) != 0 {
				t.Fatalf("got %+v, want the edit and no tombstone", out)
			}
			// The kept edit must outrank the tombstone on every computer.
			if got.Meta.Revision < tomb.Revision {
				t.Fatalf("kept edit revision %d is below the purge's %d", got.Meta.Revision, tomb.Revision)
			}
			if len(conflicts) != 1 || conflicts[0].Kind != "purge" {
				t.Fatalf("conflicts = %+v, want the purge", conflicts)
			}
		})
	}
}

func TestMergeDifferentRecordsKeepsBothChanges(t *testing.T) {
	ba, bb := meta("a", 1, "A", t0), meta("b", 1, "A", t0)
	local := Set{Items: []Record{host(meta("a", 2, "A", t0.Add(time.Minute)), "a2"), host(bb, "b1")}}
	remote := Set{Items: []Record{host(ba, "a1"), host(meta("b", 2, "B", t0.Add(time.Minute)), "b2"), host(meta("c", 1, "B", t0), "c1")}}
	out, conflicts := Merge("hosts", baseOf(ba, bb), local, remote, t0.Add(time.Hour))
	got := map[string]string{}
	for _, r := range out.Items {
		got[r.Meta.ID] = alias(t, r)
	}
	if got["a"] != "a2" || got["b"] != "b2" || got["c"] != "c1" || len(got) != 3 || len(conflicts) != 0 {
		t.Fatalf("got %v with %d conflicts", got, len(conflicts))
	}
}

func TestMergeClampsFutureTimesAndBreaksTiesByDevice(t *testing.T) {
	now := t0.Add(time.Hour)
	b := meta("h", 1, "A", t0)
	local := host(meta("h", 2, "z-device", now), "local")
	remote := host(meta("h", 2, "a-device", now.Add(24*time.Hour)), "from-the-future")
	out, _ := Merge("hosts", baseOf(b), Set{Items: []Record{local}}, Set{Items: []Record{remote}}, now)
	if got := alias(t, only(t, out)); got != "local" {
		t.Fatalf("winner %q: a future timestamp must clamp to now, then the higher device ID wins", got)
	}
}

func TestMergeStaleSideCountsAsUnchanged(t *testing.T) {
	b := meta("h", 4, "A", t0.Add(time.Hour))
	out, conflicts := Merge("hosts", baseOf(b), Set{Items: []Record{host(b, "current")}}, Set{Items: []Record{host(meta("h", 3, "B", t0), "stale")}}, t0.Add(2*time.Hour))
	if alias(t, only(t, out)) != "current" || len(conflicts) != 0 {
		t.Fatal("an older revision than the base must not win")
	}
}

func TestMergeWinnerTakesTheHighestRevision(t *testing.T) {
	b := meta("h", 1, "A", t0)
	local := host(meta("h", 6, "A", t0.Add(time.Minute)), "many-edits")
	remote := host(meta("h", 2, "B", t0.Add(2*time.Minute)), "one-newer-edit")
	out, _ := Merge("hosts", baseOf(b), Set{Items: []Record{local}}, Set{Items: []Record{remote}}, t0.Add(time.Hour))
	got := only(t, out)
	if alias(t, got) != "one-newer-edit" || got.Meta.Revision != 6 || got.Meta.DeviceID != "B" {
		t.Fatalf("got %s %+v, want B's edit at revision 6", alias(t, got), got.Meta)
	}
	var h store.Host
	json.Unmarshal(got.Raw, &h)
	if h.Revision != 6 {
		t.Fatalf("raw revision %d, want 6 to match Meta", h.Revision)
	}
}

func TestMergeSameContentOnBothSidesIsNoConflict(t *testing.T) {
	out, conflicts := Merge("hosts", nil, Set{Items: []Record{host(meta("h", 1, "A", t0), "same")}}, Set{Items: []Record{host(meta("h", 1, "B", t0.Add(time.Second)), "same")}}, t0.Add(time.Hour))
	if alias(t, only(t, out)) != "same" || len(conflicts) != 0 {
		t.Fatalf("conflicts = %+v, want none for identical content", conflicts)
	}
}

// A computer that only connected to a host has not edited it: the stamp
// leaves Meta alone, so another computer's edit wins, even with an older
// clock, and is no conflict.
func TestMergeConnectOnlyDoesNotRevertAnEdit(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "Hostbay"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	var h store.Host
	if err := st.Edit(func(d *store.Data) error {
		h = d.PutHost(store.Host{Alias: "web", Hostname: "10.0.0.1", Port: 22, User: "root"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Edit(func(d *store.Data) error { return d.SetLastConnected(h.ID, "1761213600000") }); err != nil {
		t.Fatal(err)
	}
	d, err := st.Data()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(d.Host(h.ID))
	onA := Record{Meta: d.Host(h.ID).Meta, Raw: raw}
	onB := host(meta(h.ID, h.Revision+1, "B", h.ModifiedAt.Add(-time.Hour)), "renamed-on-b")
	out, conflicts := Merge("hosts", baseOf(h.Meta), Set{Items: []Record{onA}}, Set{Items: []Record{onB}}, time.Now())
	if got := alias(t, only(t, out)); got != "renamed-on-b" || len(conflicts) != 0 {
		t.Fatalf("winner %q, conflicts %+v; want B's edit and no conflict", got, conflicts)
	}
}

// Two versions that differ only in when each computer last connected are
// not a conflict.
func TestMergeLastConnectedAloneIsNoConflict(t *testing.T) {
	a := host(meta("h", 2, "A", t0.Add(time.Minute)), "same")
	var h store.Host
	_ = json.Unmarshal(a.Raw, &h)
	h.LastConnected = "1761213600000"
	a.Raw, _ = json.Marshal(h)
	b := host(meta("h", 2, "B", t0.Add(2*time.Minute)), "same")
	if _, conflicts := Merge("hosts", baseOf(meta("h", 1, "A", t0)), Set{Items: []Record{a}}, Set{Items: []Record{b}}, t0.Add(time.Hour)); len(conflicts) != 0 {
		t.Fatalf("conflicts = %+v, want none", conflicts)
	}
}

func newData(t *testing.T) *store.Data {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "Hostbay"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	d, err := st.Data()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestRepairBreaksFolderCyclesAndClearsDanglingLinks(t *testing.T) {
	d := newData(t)
	// X under Y on one computer, Y under X on the other, merged.
	d.Folders.Items = []store.Folder{
		{Meta: store.Meta{ID: "x"}, Name: "X", ParentID: new("y")},
		{Meta: store.Meta{ID: "y"}, Name: "Y", ParentID: new("x")},
		{Meta: store.Meta{ID: "z"}, Name: "Z", ParentID: new("gone")},
	}
	d.Hosts.Items = []store.Host{
		{Meta: store.Meta{ID: "h1"}, Alias: "in-purged-folder", FolderID: new("purged")},
		{Meta: store.Meta{ID: "h2"}, Alias: "jump-gone", JumpHostID: new("nope"), FolderID: new("x")},
	}
	notes := Repair(d)
	if len(notes) == 0 {
		t.Fatal("repair reported nothing")
	}
	roots := 0
	for _, f := range d.Folders.Items {
		if f.ParentID == nil {
			roots++
		}
	}
	if roots != 2 { // one of X/Y, and Z
		t.Fatalf("got %d top-level folders, want 2: %+v", roots, d.Folders.Items)
	}
	if d.Folder("x").ParentID == nil && d.Folder("y").ParentID == nil {
		t.Fatal("both cycle members were cut loose; only one needs to be")
	}
	if d.Host("h1").FolderID != nil || d.Host("h2").JumpHostID != nil || d.Host("h2").FolderID == nil {
		t.Fatalf("hosts = %+v", d.Hosts.Items)
	}
}
