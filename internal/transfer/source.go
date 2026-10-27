// Package transfer moves files between any two Sources: the local disk, or
// SFTP or SCP on a host's SSH lease. One engine gives every pair progress,
// cancel, a choice on name conflicts and no partial files (R38, R44, KTD17).
package transfer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"sort"
	"strings"

	"hostbay/internal/sftp"
)

// Entry is one file or folder, shaped like the UI's DirEntry. Mode is a raw
// st_mode, file type bits included.
type Entry = sftp.Entry

// Source is one browsable file tree. Paths are absolute and slash-separated
// on every source, the local disk included, so the engine can join them.
type Source interface {
	// List returns dir's entries, folders first, then by case-insensitive
	// name. Symlinks are listed as links, not as what they point at.
	List(ctx context.Context, dir string) ([]Entry, error)
	// Stat describes p, following symlinks; a missing p is fs.ErrNotExist.
	Stat(ctx context.Context, p string) (Entry, error)
	Open(ctx context.Context, p string) (io.ReadCloser, error)
	// Create starts writing rel, a slash-separated path inside the folder
	// dir, as size bytes. Nothing changes at the destination until Commit;
	// Abort leaves it as it was, except on SCP, which has no rename that
	// replaces a file and so overwrites in place. An existing destination
	// keeps its permission bits, and a symlink keeps pointing at its target.
	Create(ctx context.Context, dir, rel string, size int64) (Writer, error)
	// Mkdir makes the folder rel inside dir.
	Mkdir(ctx context.Context, dir, rel string) error
	Rename(ctx context.Context, from, to string) error
	// Remove deletes a file or an empty folder.
	Remove(ctx context.Context, p string, isDir bool) error
}

// Writer is a file being written. Exactly one of Commit and Abort ends it.
type Writer interface {
	io.Writer
	Commit() error
	Abort() error
}

// defaultLimit is how many files a source moves at once unless it says
// otherwise with a Limit method.
const defaultLimit = 4

func limit(s Source) int {
	if l, ok := s.(interface{ Limit() int }); ok {
		return l.Limit()
	}
	return defaultLimit
}

const (
	modeType    = 0o170000
	modeDir     = 0o040000
	modeSymlink = 0o120000
)

func isSymlink(e Entry) bool { return e.Mode&modeType == modeSymlink }

// sortEntries orders a listing as internal/sftp does: folders first, then
// by case-insensitive name.
func sortEntries(es []Entry) {
	sort.Slice(es, func(i, j int) bool {
		if es[i].IsDir != es[j].IsDir {
			return es[i].IsDir
		}
		return strings.ToLower(es[i].Name) < strings.ToLower(es[j].Name)
	})
}

// validName reports whether a name a server sent is one path element that
// stays where it is put. A backslash is refused too: SMB and a Windows disk
// read it as a separator, so ..\x would climb out of the folder.
func validName(n string) bool {
	return n != "" && n != "." && n != ".." && !strings.ContainsAny(n, "/\\\x00\n")
}

// tempName is a fresh name for a file being written beside its destination.
func tempName() string {
	var b [8]byte
	rand.Read(b[:])
	return ".hostbay-" + hex.EncodeToString(b[:]) + ".part"
}
