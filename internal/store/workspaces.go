package store

import "errors"

// Workspace is a saved split-pane layout (R65). It holds member host IDs
// and the layout only, never a secret or an address (KTD40), so it syncs
// as is.
type Workspace struct {
	Meta
	Name   string        `json:"name"`
	Layout WorkspaceNode `json:"layout"`
}

// WorkspaceNode is a pane (HostID set) or a split of Children in Dir
// ("row" or "col"), each with its share of the split in Sizes.
type WorkspaceNode struct {
	HostID   string          `json:"hostId,omitempty"`
	Dir      string          `json:"dir,omitempty"`
	Sizes    []float64       `json:"sizes,omitempty"`
	Children []WorkspaceNode `json:"children,omitempty"`
}

// Workspace returns the workspace with id, or nil.
func (d *Data) Workspace(id string) *Workspace {
	if i := find(d.Workspaces.Items, id); i >= 0 {
		return &d.Workspaces.Items[i]
	}
	return nil
}

// PutWorkspace saves w as a local change and returns it with its new Meta.
func (d *Data) PutWorkspace(w Workspace) Workspace { return put(d, &d.Workspaces, w) }

// AddWorkspace adds w, keeping any Meta it carries.
func (d *Data) AddWorkspace(w Workspace) Workspace { return add(d, &d.Workspaces, w) }

// PurgeWorkspace removes a workspace, leaving a tombstone for sync.
func (d *Data) PurgeWorkspace(id string) error {
	if !purge(d, &d.Workspaces, id) {
		return errors.New("workspace not found")
	}
	return nil
}
