package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspacesPersistWithMetaAndLeaveTombstones(t *testing.T) {
	s := openTestStore(t)
	layout := WorkspaceNode{Dir: "row", Sizes: []float64{0.5, 0.5}, Children: []WorkspaceNode{{HostID: "h1"}, {HostID: "h2"}}}
	var id string
	d := edit(t, s, func(d *Data) error {
		id = d.PutWorkspace(Workspace{Name: "web", Layout: layout}).ID
		return nil
	})
	got := d.Workspace(id)
	if got == nil || got.Revision != 1 || got.Name != "web" || len(got.Layout.Children) != 2 || got.Layout.Children[1].HostID != "h2" {
		t.Fatalf("saved workspace = %+v", got)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "workspaces.json")); err != nil {
		t.Fatalf("workspaces.json not written: %v", err)
	}
	d = edit(t, s, func(d *Data) error { return d.PurgeWorkspace(id) })
	if d.Workspace(id) != nil {
		t.Fatal("purged workspace still listed")
	}
	if ts := d.Workspaces.Tombstones; len(ts) != 1 || ts[0].ID != id || !ts[0].Deleted {
		t.Fatalf("tombstones = %+v", ts)
	}
}

// A workspace holds host IDs and the layout only (KTD40): anything else the
// page sends, such as a hostname or a password, is dropped on decode.
func TestWorkspaceKeepsHostIDsAndLayoutOnly(t *testing.T) {
	in := `{"id":"w1","name":"web","password":"hunter2","layout":{"dir":"col","sizes":[0.3,0.7],"children":[
		{"hostId":"h1","hostname":"db.internal"},{"hostId":"h2","user":"root"}]}}`
	var w Workspace
	if err := json.Unmarshal([]byte(in), &w); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(w)
	for _, leak := range []string{"hunter2", "db.internal", "root", "password", "hostname"} {
		if strings.Contains(string(out), leak) {
			t.Fatalf("workspace JSON kept %q: %s", leak, out)
		}
	}
	if !strings.Contains(string(out), `"hostId":"h1"`) || !strings.Contains(string(out), `"hostId":"h2"`) {
		t.Fatalf("workspace JSON lost its hosts: %s", out)
	}
}
