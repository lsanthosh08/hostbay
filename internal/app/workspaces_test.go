package app

import (
	"path/filepath"
	"testing"

	"hostbay/internal/store"
)

func newWorkspaceService(t *testing.T) (*WorkspaceService, []string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "Hostbay"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	var ids []string
	if err := st.Edit(func(d *store.Data) error {
		for _, a := range []string{"web", "db"} {
			ids = append(ids, d.PutHost(store.Host{Meta: store.Meta{ID: store.NewID()}, Alias: a, Hostname: a + ".lan"}).ID)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return NewWorkspaceService(st), ids
}

func leaf(id string) store.WorkspaceNode { return store.WorkspaceNode{HostID: id} }

func TestWorkspaceSavesListsAndDeletes(t *testing.T) {
	s, h := newWorkspaceService(t)
	w, err := s.SaveWorkspace(Workspace{Name: " prod ", Layout: store.WorkspaceNode{Dir: "row", Sizes: []float64{0.4, 0.6}, Children: []store.WorkspaceNode{leaf(h[0]), leaf(h[1])}}})
	if err != nil || w.ID == "" || w.Name != "prod" {
		t.Fatalf("saved = %+v, %v", w, err)
	}
	w.Name = "prod 2"
	if _, err := s.SaveWorkspace(w); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListWorkspaces()
	if err != nil || len(list) != 1 || list[0].Name != "prod 2" || list[0].Layout.Children[1].HostID != h[1] {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if err := s.DeleteWorkspace(w.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListWorkspaces(); len(list) != 0 {
		t.Fatalf("after delete = %+v", list)
	}
}

// No empty or malformed layout is saved, and a session sits in one pane only.
func TestWorkspaceRefusesBadLayouts(t *testing.T) {
	s, h := newWorkspaceService(t)
	row := func(sizes []float64, kids ...store.WorkspaceNode) store.WorkspaceNode {
		return store.WorkspaceNode{Dir: "row", Sizes: sizes, Children: kids}
	}
	for name, l := range map[string]store.WorkspaceNode{
		"empty":         {},
		"one-child":     row([]float64{1}, leaf(h[0])),
		"sizes":         row([]float64{1}, leaf(h[0]), leaf(h[1])),
		"zero size":     row([]float64{0, 1}, leaf(h[0]), leaf(h[1])),
		"dir":           {Dir: "diagonal", Sizes: []float64{0.5, 0.5}, Children: []store.WorkspaceNode{leaf(h[0]), leaf(h[1])}},
		"leaf and kids": {HostID: h[0], Dir: "row", Sizes: []float64{0.5, 0.5}, Children: []store.WorkspaceNode{leaf(h[0]), leaf(h[1])}},
		"twice":         row([]float64{0.5, 0.5}, leaf(h[0]), leaf(h[0])),
		"unknown host":  row([]float64{0.5, 0.5}, leaf(h[0]), leaf("gone")),
	} {
		if _, err := s.SaveWorkspace(Workspace{Name: "w", Layout: l}); err == nil {
			t.Errorf("%s: saved %+v", name, l)
		}
	}
	if _, err := s.SaveWorkspace(Workspace{Name: "  ", Layout: leaf(h[0])}); err == nil {
		t.Error("saved a workspace with no name")
	}
	if list, _ := s.ListWorkspaces(); len(list) != 0 {
		t.Fatalf("saved = %+v", list)
	}
}
