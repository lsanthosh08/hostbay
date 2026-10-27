package store

import (
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// seedStore fills home the way a Release 4 install does: store files,
// sync-git/, backups/, pin files, the master-password file, logs/,
// backgrounds/ and the MCP endpoint file beside the lock.
func seedStore(t *testing.T, home string) {
	t.Helper()
	s := openT(t, home)
	if err := Save(s, "hosts.json", []testHost{{Meta: Meta{ID: "h1"}, Alias: "web", Hostname: "10.0.0.5"}}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	for name, body := range map[string]string{
		"masterpassword.json":                    `{"schemaVersion":3,"data":{"salt":"c2FsdA=="}}`,
		"tlspins.json":                           `{"schemaVersion":3,"data":[]}`,
		"sync-git/objects/ab/cdef":               "git object",
		"sync-git/HEAD":                          "ref: refs/heads/main\n",
		"backups/v2-20261025T000000Z/hosts.json": `{"schemaVersion":2,"data":[]}`,
		"logs/web.log":                           "$ uptime\n",
		"backgrounds/sky.png":                    "\x89PNG",
		EndpointFile:                             `{"port":1234}`,
	} {
		p := filepath.Join(home, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// tree maps every file and folder under dir to its contents ("<dir>" for a
// folder), leaving out names in skip at the top level.
func tree(t *testing.T, dir string, skip ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
		for _, s := range skip {
			if rel == s {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
		}
		if d.IsDir() {
			out[filepath.ToSlash(rel)] = "<dir>"
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// homeOnly are the files that live in DefaultDir whatever the data folder is.
var homeOnly = []string{lockName, pointerName, EndpointFile}

func emptyDir(t *testing.T) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), "moved")
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}

func requestMove(t *testing.T, home, to string) {
	t.Helper()
	s := openT(t, home)
	defer s.Close()
	if err := s.RequestMove(to); err != nil {
		t.Fatalf("RequestMove(%s): %v", to, err)
	}
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func withHook(t *testing.T, h func(step string, n int) error) {
	t.Helper()
	moveHook = h
	t.Cleanup(func() { moveHook = nil })
}

func TestMoveCopiesEverythingAndCleansUpTheOldFolderNextLaunch(t *testing.T) {
	home := filepath.Join(t.TempDir(), "Hostbay")
	seedStore(t, home)
	data := tree(t, home, homeOnly...)
	to := emptyDir(t)
	requestMove(t, home, to)

	s := openT(t, home) // the launch that copies
	if s.Dir() != to || s.Notice() != "" {
		t.Fatalf("after the move Dir() = %s, notice %q; want %s and none", s.Dir(), s.Notice(), to)
	}
	if got := tree(t, to); !maps.Equal(got, data) {
		t.Fatalf("new folder:\n%v\nwant\n%v", got, data)
	}
	if got := tree(t, home, homeOnly...); !maps.Equal(got, data) {
		t.Fatal("the copying launch removed something from the old folder")
	}
	hosts, err := Load[[]testHost](s, "hosts.json")
	if err != nil || len(hosts) != 1 || hosts[0].Alias != "web" {
		t.Fatalf("hosts after the move: %v, %v", hosts, err)
	}
	s.Close()

	s = openT(t, home) // the next launch cleans up
	defer s.Close()
	if s.Dir() != to {
		t.Fatalf("second launch Dir() = %s, want %s", s.Dir(), to)
	}
	// Listed, not read: Windows refuses reads of the locked lock file.
	entries, _ := os.ReadDir(home)
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if want := []string{pointerName, lockName, EndpointFile}; !slices.Equal(left, want) {
		t.Fatalf("old folder after cleanup holds %v; want only %v", left, want)
	}
	if got := tree(t, to); !maps.Equal(got, data) {
		t.Fatal("cleanup changed the new folder")
	}
}

func TestSecondInstanceIsRefusedDuringAndAfterAMove(t *testing.T) {
	home := filepath.Join(t.TempDir(), "Hostbay")
	seedStore(t, home)
	to := emptyDir(t)
	requestMove(t, home, to)
	var during string
	withHook(t, func(step string, n int) error {
		if step == "copy" && n == 1 {
			during = openInOtherProcess(t, home)
		}
		return nil
	})
	s := openT(t, home)
	defer s.Close()
	if during != "refused" {
		t.Fatalf("second process during the copy: %q, want refused", during)
	}
	if got := openInOtherProcess(t, home); got != "refused" {
		t.Fatalf("second process after the move: %q, want refused", got)
	}
	if exists(filepath.Join(to, lockName)) {
		t.Fatal("the lock moved with the data")
	}
}

func TestRequestMoveRefusesUnsafeFolders(t *testing.T) {
	home := filepath.Join(t.TempDir(), "Hostbay")
	seedStore(t, home)
	s := openT(t, home)
	defer s.Close()

	full := emptyDir(t)
	os.WriteFile(filepath.Join(full, "notes.txt"), []byte("mine"), 0o600)
	inside := filepath.Join(home, "inner")
	os.Mkdir(inside, 0o700)
	cases := map[string]struct{ dir, reason string }{
		"not empty": {full, "not empty"},
		"inside":    {inside, "inside"},
		"current":   {home, "already"},
		"missing":   {filepath.Join(t.TempDir(), "nope"), "no such"},
		"relative":  {"relative/dir", "full path"},
	}
	if runtime.GOOS != "windows" {
		shared := emptyDir(t)
		os.Chmod(shared, 0o770)
		cases["group-writable"] = struct{ dir, reason string }{shared, "other accounts"}
	}
	for name, c := range cases {
		err := s.RequestMove(c.dir)
		if err == nil || !strings.Contains(err.Error(), c.reason) {
			t.Errorf("%s: RequestMove = %v; want a refusal mentioning %q", name, err, c.reason)
		}
	}
	if runtime.GOOS != "windows" {
		real := getuid
		getuid = func() int { return real() + 1 }
		err := s.RequestMove(emptyDir(t))
		getuid = real
		if err == nil || !strings.Contains(err.Error(), "another account") {
			t.Errorf("other owner: RequestMove = %v; want a refusal", err)
		}
	}
	if exists(filepath.Join(home, pointerName)) {
		t.Fatal("a refused move wrote the pointer")
	}
}

func TestMoveAwayAndBackKeepsTheLockAndLeavesNoPointer(t *testing.T) {
	home := filepath.Join(t.TempDir(), "Hostbay")
	seedStore(t, home)
	data := tree(t, home, homeOnly...)
	away := emptyDir(t)
	requestMove(t, home, away)
	s := openT(t, home)
	if s.Dir() != away {
		t.Fatalf("Dir() = %s, want %s", s.Dir(), away)
	}
	// Back before the old folder was cleaned up: its copied entries do not
	// count against the emptiness check, and are removed first.
	if err := s.RequestMove(home); err != nil {
		t.Fatalf("RequestMove(home): %v", err)
	}
	s.Close()
	s = openT(t, home)
	if s.Dir() != home || s.Notice() != "" {
		t.Fatalf("after moving back Dir() = %s, notice %q", s.Dir(), s.Notice())
	}
	if got := tree(t, home, homeOnly...); !maps.Equal(got, data) {
		t.Fatalf("home after moving back:\n%v\nwant\n%v", got, data)
	}
	s.Close()
	s = openT(t, home) // cleans up the folder it came back from
	defer s.Close()
	if exists(filepath.Join(home, pointerName)) {
		t.Fatal("pointer left behind after moving back to the default folder")
	}
	if !exists(filepath.Join(home, lockName)) {
		t.Fatal("lock gone")
	}
	if got := tree(t, away); len(got) != 0 {
		t.Fatalf("folder moved away from still holds %v", got)
	}
	if got := openInOtherProcess(t, home); got != "refused" {
		t.Fatalf("second process: %q, want refused", got)
	}
}

func TestCopyFailureRemovesThePartialCopyAndKeepsTheOldFolder(t *testing.T) {
	home := filepath.Join(t.TempDir(), "Hostbay")
	seedStore(t, home)
	before := tree(t, home, homeOnly...)
	to := emptyDir(t)
	requestMove(t, home, to)
	withHook(t, func(step string, n int) error {
		if step == "copy" && n == 3 {
			return errors.New("disk full")
		}
		return nil
	})
	s := openT(t, home)
	defer s.Close()
	if s.Dir() != home || !strings.Contains(s.Notice(), "disk full") {
		t.Fatalf("after a failed copy Dir() = %s, notice %q; want home and the failure", s.Dir(), s.Notice())
	}
	if got := tree(t, to); len(got) != 0 {
		t.Fatalf("partial copy left behind: %v", got)
	}
	if got := tree(t, home, homeOnly...); !maps.Equal(got, before) {
		t.Fatal("old folder changed")
	}
	if p, err := readPointer(home); err != nil || p.Pending != "" || p.Dir != "" {
		t.Fatalf("pointer after a failed copy: %+v, %v; want none", p, err)
	}
}

// The move must read its copy back before it trusts it: a copied file that
// differs from the original, or is gone, keeps the data at home, removes
// the copy and leaves the old folder as it was.
func TestBadCopyFailsVerifyAndKeepsTheOldFolder(t *testing.T) {
	for name, spoil := range map[string]func(p string) error{
		"corrupted": func(p string) error {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			b[0] ^= 0xff // same size, different bytes
			return os.WriteFile(p, b, 0o600)
		},
		"removed": os.Remove,
	} {
		t.Run(name, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "Hostbay")
			seedStore(t, home)
			before := tree(t, home, homeOnly...)
			to := emptyDir(t)
			requestMove(t, home, to)
			victim := filepath.Join(to, "tlspins.json")
			spoilt := false
			withHook(t, func(step string, n int) error {
				if step == "copy" && !spoilt && exists(victim) {
					spoilt = true
					return spoil(victim)
				}
				return nil
			})
			s := openT(t, home)
			defer s.Close()
			if !spoilt {
				t.Fatal("the copy never wrote tlspins.json")
			}
			if s.Dir() != home || !strings.Contains(s.Notice(), "tlspins.json") {
				t.Fatalf("after a bad copy Dir() = %s, notice %q; want home and the file named", s.Dir(), s.Notice())
			}
			if got := tree(t, to); len(got) != 0 {
				t.Fatalf("bad copy left behind: %v", got)
			}
			if got := tree(t, home, homeOnly...); !maps.Equal(got, before) {
				t.Fatal("old folder changed")
			}
			if p, err := readPointer(home); err != nil || p.Pending != "" || p.Dir != "" {
				t.Fatalf("pointer after a bad copy: %+v, %v; want none", p, err)
			}
		})
	}
}

func TestPointerWriteFailureDeletesNothingAndRetriesNextLaunch(t *testing.T) {
	home := filepath.Join(t.TempDir(), "Hostbay")
	seedStore(t, home)
	before := tree(t, home, homeOnly...)
	to := emptyDir(t)
	requestMove(t, home, to)
	withHook(t, func(step string, n int) error {
		if step == "pointer" {
			return errors.New("pointer write refused")
		}
		return nil
	})
	s := openT(t, home)
	if s.Dir() != home || !strings.Contains(s.Notice(), "pointer write refused") {
		t.Fatalf("Dir() = %s, notice %q; want home and the failure", s.Dir(), s.Notice())
	}
	if got := tree(t, home, homeOnly...); !maps.Equal(got, before) {
		t.Fatal("old folder changed")
	}
	if got := tree(t, to); !maps.Equal(got, before) {
		t.Fatal("the verified copy was deleted")
	}
	s.Close()
	moveHook = nil
	s = openT(t, home)
	defer s.Close()
	if s.Dir() != to {
		t.Fatalf("retry: Dir() = %s, want %s", s.Dir(), to)
	}
}

func TestMissingDataFolderIsNeverReplacedByAnEmptyStore(t *testing.T) {
	home := filepath.Join(t.TempDir(), "Hostbay")
	seedStore(t, home)
	to := emptyDir(t)
	requestMove(t, home, to)
	openT(t, home).Close()
	openT(t, home).Close() // cleanup: home holds no store any more
	for _, c := range []struct {
		name    string
		breakIt func()
	}{
		{"deleted", func() { os.RemoveAll(to) }},
		{"empty", func() { os.Mkdir(to, 0o700) }}, // an unmounted disk's mount point
	} {
		name := c.name
		c.breakIt()
		_, err := Open(home)
		var missing *FolderMissingError
		if !errors.As(err, &missing) || missing.Dir != to {
			t.Fatalf("%s: Open = %v; want FolderMissingError for %s", name, err, to)
		}
		if exists(filepath.Join(to, stateName)) || exists(filepath.Join(home, stateName)) {
			t.Fatalf("%s: an empty store was created", name)
		}
	}
	if got := openInOtherProcess(t, home); got == "locked" {
		t.Fatal("a second process opened a store for the missing folder")
	}
}

func TestMoveRefusesLinksAndNeverFollowsThem(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need a privilege on Windows")
	}
	home := filepath.Join(t.TempDir(), "Hostbay")
	seedStore(t, home)
	outside := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(outside, []byte("not Hostbay's"), 0o600)
	if err := os.Symlink(outside, filepath.Join(home, "logs", "escape")); err != nil {
		t.Fatal(err)
	}
	before := tree(t, home, homeOnly...)
	to := emptyDir(t)
	requestMove(t, home, to)
	s := openT(t, home)
	defer s.Close()
	if s.Dir() != home || !strings.Contains(s.Notice(), "link") {
		t.Fatalf("Dir() = %s, notice %q; want the move refused over the link", s.Dir(), s.Notice())
	}
	if got := tree(t, to); len(got) != 0 {
		t.Fatalf("target holds %v", got)
	}
	if got := tree(t, home, homeOnly...); !maps.Equal(got, before) {
		t.Fatal("old folder changed")
	}
	if b := readT(t, outside); string(b) != "not Hostbay's" {
		t.Fatal("the link's target changed")
	}
}

func TestPointToAcceptsOnlyAFolderHoldingAStore(t *testing.T) {
	home := filepath.Join(t.TempDir(), "Hostbay")
	other := filepath.Join(t.TempDir(), "Other")
	seedStore(t, other)
	empty := emptyDir(t)
	if err := PointTo(home, empty); err == nil {
		t.Fatal("PointTo accepted a folder without a store")
	}
	if err := PointTo(home, other); err != nil {
		t.Fatal(err)
	}
	s := openT(t, home)
	if s.Dir() != other {
		t.Fatalf("Dir() = %s, want %s", s.Dir(), other)
	}
	s.Close()
	os.WriteFile(filepath.Join(home, stateName), readT(t, filepath.Join(other, stateName)), 0o600)
	if err := PointTo(home, home); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(home, pointerName)) {
		t.Fatal("pointing at the default folder left a pointer")
	}
}

// Kill tests: a helper process (TestMain, moveKillEnv) runs Open and exits
// at a named step, as a crash or a power cut would.

func openAndDieAt(t *testing.T, home, step string, n int) {
	t.Helper()
	runHelper(t, home, moveKillEnv+"="+step+":"+strconv.Itoa(n))
}

func TestMoveKilledMidCopyResumesOnTheNextLaunch(t *testing.T) {
	home := filepath.Join(t.TempDir(), "Hostbay")
	seedStore(t, home)
	data := tree(t, home, homeOnly...)
	to := emptyDir(t)
	requestMove(t, home, to)
	openAndDieAt(t, home, "copy", 4)
	if got := tree(t, to); len(got) == 0 || maps.Equal(got, data) {
		t.Fatalf("the kill did not land mid-copy: target holds %v", got)
	}
	if got := tree(t, home, homeOnly...); !maps.Equal(got, data) {
		t.Fatal("source changed before the pointer was written")
	}
	s := openT(t, home)
	defer s.Close()
	if s.Dir() != to || s.Notice() != "" {
		t.Fatalf("resumed Dir() = %s, notice %q", s.Dir(), s.Notice())
	}
	if got := tree(t, to); !maps.Equal(got, data) {
		t.Fatalf("resumed copy:\n%v\nwant\n%v", got, data)
	}
}

func TestMoveKilledBeforeThePointerWriteResumes(t *testing.T) {
	home := filepath.Join(t.TempDir(), "Hostbay")
	seedStore(t, home)
	data := tree(t, home, homeOnly...)
	to := emptyDir(t)
	requestMove(t, home, to)
	openAndDieAt(t, home, "verified", 0)
	if p, _ := readPointer(home); p.Pending != to || p.Dir != "" {
		t.Fatalf("pointer after the kill: %+v; want the move still pending", p)
	}
	if got := tree(t, home, homeOnly...); !maps.Equal(got, data) {
		t.Fatal("source changed before the pointer was written")
	}
	s := openT(t, home)
	defer s.Close()
	if s.Dir() != to {
		t.Fatalf("resumed Dir() = %s, want %s", s.Dir(), to)
	}
}

func TestCleanupKilledMidwayFinishesOnTheNextLaunch(t *testing.T) {
	home := filepath.Join(t.TempDir(), "Hostbay")
	seedStore(t, home)
	data := tree(t, home, homeOnly...)
	to := emptyDir(t)
	requestMove(t, home, to)
	openT(t, home).Close()
	openAndDieAt(t, home, "clean", 2)
	left := tree(t, home, homeOnly...)
	if len(left) == 0 || len(left) == len(data) {
		t.Fatalf("the kill did not land mid-cleanup: old folder holds %v", left)
	}
	if got := tree(t, to); !maps.Equal(got, data) {
		t.Fatal("new folder changed")
	}
	s := openT(t, home)
	defer s.Close()
	if got := tree(t, home, homeOnly...); len(got) != 0 {
		t.Fatalf("old folder after the resumed cleanup holds %v", got)
	}
	if got := tree(t, to); !maps.Equal(got, data) {
		t.Fatal("new folder changed")
	}
}

// Moving again before the last move's old folder was cleaned up must not
// forget that folder: with it unplugged, the new move waits and the
// pointer still lists the old copy for removal.
func TestSecondMoveWaitsForThePendingCleanup(t *testing.T) {
	home := filepath.Join(t.TempDir(), "Hostbay")
	seedStore(t, home)
	data := tree(t, home, homeOnly...)
	a, b, c := emptyDir(t), emptyDir(t), emptyDir(t)
	requestMove(t, home, a)
	openT(t, home).Close() // copies to a
	openT(t, home).Close() // cleans up home
	requestMove(t, home, b)
	s := openT(t, home) // copies to b; a waits for cleanup
	if s.Dir() != b {
		t.Fatalf("Dir() = %s, want %s", s.Dir(), b)
	}
	if err := s.RequestMove(c); err != nil {
		t.Fatal(err)
	}
	s.Close()
	unplugged := a + "-unplugged"
	if err := os.Rename(a, unplugged); err != nil {
		t.Fatal(err)
	}
	s = openT(t, home)
	if s.Dir() != b || !strings.Contains(s.Notice(), a) {
		t.Fatalf("with %s unplugged Dir() = %s, notice %q; want %s and a notice naming it", a, s.Dir(), s.Notice(), b)
	}
	s.Close()
	if p, err := readPointer(home); err != nil || p.Pending != c || !samePath(p.CleanFrom, a) {
		t.Fatalf("pointer = %+v, %v; want the move to %s pending and %s still to clean", p, err, c, a)
	}
	if err := os.Rename(unplugged, a); err != nil {
		t.Fatal(err)
	}
	s = openT(t, home) // cleans up a, then moves to c
	defer s.Close()
	if s.Dir() != c || s.Notice() != "" {
		t.Fatalf("after reconnecting Dir() = %s, notice %q; want %s", s.Dir(), s.Notice(), c)
	}
	if got := tree(t, a); len(got) != 0 {
		t.Fatalf("old folder %s still holds %v", a, got)
	}
	if got := tree(t, c); !maps.Equal(got, data) {
		t.Fatal("the moved data changed")
	}
}
