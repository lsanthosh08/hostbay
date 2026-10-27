// Package sourcetest is the conformance suite every transfer.Source passes:
// list, upload, download, rename, make folders and remove (KTD17).
package sourcetest

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"path"
	"strings"
	"testing"

	"hostbay/internal/transfer"
)

// Run exercises src inside dir, an empty folder on it given in the
// source's slash-separated form.
func Run(t *testing.T, src transfer.Source, dir string) {
	t.Helper()
	RunWithOddName(t, src, dir, `it's "quoted" & spaced.txt`)
}

// RunWithOddName is Run with another awkward file name, for a protocol
// that forbids some of Run's: SMB refuses '"' in names.
func RunWithOddName(t *testing.T, src transfer.Source, dir, odd string) {
	t.Helper()
	ctx := context.Background()

	put(t, src, dir, "a.txt", "hello")
	if got := get(t, src, path.Join(dir, "a.txt")); got != "hello" {
		t.Fatalf("download = %q", got)
	}
	if e, err := src.Stat(ctx, path.Join(dir, "a.txt")); err != nil || e.Size != 5 || e.IsDir || e.Name != "a.txt" {
		t.Fatalf("stat = %+v, %v", e, err)
	}
	if _, err := src.Stat(ctx, path.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stat of a missing file: %v", err)
	}

	if err := src.Mkdir(ctx, dir, "sub dir"); err != nil {
		t.Fatal(err)
	}
	put(t, src, dir, "sub dir/"+odd, "")
	if got := get(t, src, path.Join(dir, "sub dir", odd)); got != "" {
		t.Fatalf("empty file read back as %q", got)
	}
	if got := names(t, src, dir); got != "sub dir/,a.txt" {
		t.Fatalf("list = %s, want folders first", got)
	}
	if got := names(t, src, path.Join(dir, "sub dir")); got != odd {
		t.Fatalf("list sub = %s", got)
	}

	put(t, src, dir, "a.txt", "bye") // overwrite
	if got := get(t, src, path.Join(dir, "a.txt")); got != "bye" {
		t.Fatalf("after overwrite = %q", got)
	}

	w, err := src.Create(ctx, dir, "aborted.txt", 7)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, "partial")
	if err := w.Abort(); err != nil {
		t.Fatal(err)
	}
	if got := names(t, src, dir); got != "sub dir/,a.txt" {
		t.Fatalf("after abort list = %s", got)
	}

	if err := src.Rename(ctx, path.Join(dir, "a.txt"), path.Join(dir, "z.txt")); err != nil {
		t.Fatal(err)
	}
	if got := names(t, src, dir); got != "sub dir/,z.txt" {
		t.Fatalf("after rename list = %s", got)
	}

	for _, rm := range []struct {
		p     string
		isDir bool
	}{{path.Join(dir, "z.txt"), false}, {path.Join(dir, "sub dir", odd), false}, {path.Join(dir, "sub dir"), true}} {
		if err := src.Remove(ctx, rm.p, rm.isDir); err != nil {
			t.Fatalf("remove %s: %v", rm.p, err)
		}
	}
	if got := names(t, src, dir); got != "" {
		t.Fatalf("after remove list = %s", got)
	}
}

func put(t *testing.T, src transfer.Source, dir, rel, content string) {
	t.Helper()
	w, err := src.Create(context.Background(), dir, rel, int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, content); err != nil {
		w.Abort()
		t.Fatal(err)
	}
	if err := w.Commit(); err != nil {
		t.Fatal(err)
	}
}

func get(t *testing.T, src transfer.Source, p string) string {
	t.Helper()
	r, err := src.Open(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// names lists dir as "name,name", folders marked with a trailing slash.
func names(t *testing.T, src transfer.Source, dir string) string {
	t.Helper()
	es, err := src.List(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		if e.Path != path.Join(dir, e.Name) {
			t.Fatalf("entry %q has path %q", e.Name, e.Path)
		}
		if e.IsDir {
			e.Name += "/"
		}
		out = append(out, e.Name)
	}
	return strings.Join(out, ",")
}
