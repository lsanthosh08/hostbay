package app

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"hostbay/internal/store"
)

// Workspace is a saved split-pane layout (R65); see store.Workspace. Opening
// one is the page's: it connects the members one at a time (KTD40).
type Workspace struct {
	ID     string              `json:"id"`
	Name   string              `json:"name"`
	Layout store.WorkspaceNode `json:"layout"`
}

// WorkspaceService saves workspaces. It never connects anything.
type WorkspaceService struct {
	st *store.Store
}

func NewWorkspaceService(st *store.Store) *WorkspaceService { return &WorkspaceService{st: st} }

// Limits on a saved layout, well past what a window can show.
const (
	maxWorkspacePanes = 16
	maxWorkspaceDepth = 8
)

func (s *WorkspaceService) ListWorkspaces() ([]Workspace, error) {
	d, err := s.st.Data()
	if err != nil {
		return nil, err
	}
	out := []Workspace{}
	for _, w := range d.Workspaces.Items {
		out = append(out, Workspace{ID: w.ID, Name: w.Name, Layout: w.Layout})
	}
	return out, nil
}

// SaveWorkspace adds or updates a workspace. Every pane must be a saved
// host, each in one pane only.
func (s *WorkspaceService) SaveWorkspace(w Workspace) (Workspace, error) {
	w.Name = strings.TrimSpace(w.Name)
	if w.Name == "" || len(w.Name) > 100 {
		return Workspace{}, errors.New("a workspace needs a name of up to 100 characters")
	}
	var hosts []string
	if err := checkPane(w.Layout, 0, &hosts); err != nil {
		return Workspace{}, err
	}
	rec := store.Workspace{Meta: store.Meta{ID: w.ID}, Name: w.Name, Layout: w.Layout}
	err := s.st.Edit(func(d *store.Data) error {
		seen := map[string]bool{}
		for _, id := range hosts {
			if d.Host(id) == nil {
				return errors.New("host not found")
			}
			if seen[id] {
				return errors.New("a host can fill one pane only")
			}
			seen[id] = true
		}
		if old := d.Workspace(w.ID); old != nil {
			rec.Meta = old.Meta
		}
		rec = d.PutWorkspace(rec)
		return nil
	})
	w.ID = rec.ID
	return w, err
}

func (s *WorkspaceService) DeleteWorkspace(id string) error {
	return s.st.Edit(func(d *store.Data) error { return d.PurgeWorkspace(id) })
}

// checkPane checks n's shape and collects its hosts in pane order.
func checkPane(n store.WorkspaceNode, depth int, hosts *[]string) error {
	if depth > maxWorkspaceDepth {
		return errors.New("the layout is nested too deeply")
	}
	if n.Dir == "" && len(n.Children) == 0 && len(n.Sizes) == 0 {
		if n.HostID == "" {
			return errors.New("a pane has no host")
		}
		if *hosts = append(*hosts, n.HostID); len(*hosts) > maxWorkspacePanes {
			return fmt.Errorf("a workspace holds at most %d panes", maxWorkspacePanes)
		}
		return nil
	}
	if n.HostID != "" || (n.Dir != "row" && n.Dir != "col") || len(n.Children) < 2 || len(n.Sizes) != len(n.Children) {
		return errors.New("the layout is not valid")
	}
	for i, c := range n.Children {
		if !(n.Sizes[i] > 0) || math.IsInf(n.Sizes[i], 0) {
			return errors.New("the layout is not valid")
		}
		if err := checkPane(c, depth+1, hosts); err != nil {
			return err
		}
	}
	return nil
}
