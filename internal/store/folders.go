package store

import "errors"

// Folder is a node in the host tree; ParentID nil means the top level.
type Folder struct {
	Meta
	Name     string  `json:"name"`
	ParentID *string `json:"parentId,omitempty"`
	Order    int     `json:"order,omitempty"`
}

// Folder returns the folder with id, or nil.
func (d *Data) Folder(id string) *Folder {
	if i := find(d.Folders.Items, id); i >= 0 {
		return &d.Folders.Items[i]
	}
	return nil
}

// AddFolder adds f, keeping any Meta it carries.
func (d *Data) AddFolder(f Folder) Folder { return add(d, &d.Folders, f) }

// SaveFolder adds or replaces f. A folder cannot move into itself or one of
// its descendants.
func (d *Data) SaveFolder(f Folder) (Folder, error) {
	if f.ParentID != nil && f.ID != "" && d.isDescendant(*f.ParentID, f.ID) {
		return Folder{}, errors.New("cannot move folder into its own descendant")
	}
	return put(d, &d.Folders, f), nil
}

// isDescendant reports whether candidate is target or below it.
func (d *Data) isDescendant(candidate, target string) bool {
	for seen := 0; seen <= len(d.Folders.Items); seen++ { // bounded, in case of a cycle on disk
		if candidate == target {
			return true
		}
		f := d.Folder(candidate)
		if f == nil || f.ParentID == nil {
			return false
		}
		candidate = *f.ParentID
	}
	return true
}

// DeleteFolder removes folder id. Without purge its hosts and subfolders move
// up to its parent; with purge it and every folder below it go, and their
// hosts move to trash at the top level.
func (d *Data) DeleteFolder(id string, purgeAll bool) error {
	f := d.Folder(id)
	if f == nil {
		return errors.New("folder not found")
	}
	parent := f.ParentID
	affected := []string{id}
	if purgeAll {
		for i := 0; i < len(affected); i++ {
			for _, sub := range d.Folders.Items {
				if sub.ParentID != nil && *sub.ParentID == affected[i] {
					affected = append(affected, sub.ID)
				}
			}
		}
	}
	in := func(p *string) bool {
		for _, a := range affected {
			if p != nil && *p == a {
				return true
			}
		}
		return false
	}
	for i := range d.Hosts.Items {
		h := &d.Hosts.Items[i]
		if !in(h.FolderID) {
			continue
		}
		if purgeAll {
			h.FolderID = nil
			if err := d.TrashHost(h.ID); err != nil {
				return err
			}
		} else {
			h.FolderID = parent
			d.st.Touch(&h.Meta)
		}
	}
	if !purgeAll {
		for i := range d.Folders.Items {
			if sub := &d.Folders.Items[i]; sub.ParentID != nil && *sub.ParentID == id {
				sub.ParentID = parent
				d.st.Touch(&sub.Meta)
			}
		}
	}
	for _, a := range affected {
		purge(d, &d.Folders, a)
	}
	return nil
}
