package store

// Bookmark is a saved quick path in the file browser.
type Bookmark struct {
	Meta
	Path string `json:"path"`
}

// defaultBookmarks are shown until the user first saves bookmarks.
var defaultBookmarks = []string{"/etc/nginx", "/etc/ssl", "/var/log", "/var/www"}

// BookmarkPaths returns the saved paths, or the defaults when none were
// ever saved.
func (d *Data) BookmarkPaths() []string {
	if d.Bookmarks.Items == nil {
		return append([]string(nil), defaultBookmarks...)
	}
	out := []string{}
	for _, b := range d.Bookmarks.Items {
		out = append(out, b.Path)
	}
	return out
}

// SetBookmarks makes paths the saved list, in its order. A path already
// saved keeps its record; dropped ones leave tombstones.
func (d *Data) SetBookmarks(paths []string) {
	old := d.Bookmarks.Items
	d.Bookmarks.Items = []Bookmark{}
	for _, p := range paths {
		i := -1
		for j := range old {
			if old[j].Path == p {
				i = j
				break
			}
		}
		if i < 0 {
			add(d, &d.Bookmarks, Bookmark{Path: p})
			continue
		}
		d.Bookmarks.Items = append(d.Bookmarks.Items, old[i])
		old = append(old[:i], old[i+1:]...)
	}
	for _, b := range old {
		d.Bookmarks.Items = append(d.Bookmarks.Items, b)
		purge(d, &d.Bookmarks, b.ID)
	}
}
