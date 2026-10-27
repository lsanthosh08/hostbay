package claudecode

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"hostbay/internal/ai/claudecode/claudetest"
)

func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("mode and ownership rules are Unix rules")
	}
}

// program writes a small executable file in dir and returns its path.
func program(t *testing.T, dir, name string, mode os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestListingCandidatesRunsNoneOfThem(t *testing.T) {
	var fakes []*claudetest.Fake
	var dirs []string
	for range 3 {
		f := claudetest.Install(t, claudetest.Script{})
		fakes = append(fakes, f)
		dirs = append(dirs, f.Dir)
	}
	got := DefaultRules().Discover(t.TempDir(), joinPath(dirs), "", "")
	if len(got) != 3 {
		t.Fatalf("candidates = %+v", got)
	}
	for i, f := range fakes {
		if n := len(f.Calls(t)); n != 0 {
			t.Fatalf("listing ran fake %d (%d calls)", i, n)
		}
	}
}

func joinPath(dirs []string) string {
	out := ""
	for i, d := range dirs {
		if i > 0 {
			out += string(os.PathListSeparator)
		}
		out += d
	}
	return out
}

func TestDiscoverOrdersThePickedPathFirstAndAddsTheMacOSFoldersAndDesktop(t *testing.T) {
	home := t.TempDir()
	picked := program(t, t.TempDir(), "claude", 0o755)
	local := program(t, filepath.Join(home, ".local", "bin"), "claude", 0o755)
	desktop := filepath.Join(home, "Library", "Application Support", "Claude", "claude-code")
	if err := os.MkdirAll(desktop, 0o755); err != nil {
		t.Fatal(err)
	}
	got := Rules{GOOS: "darwin"}.Discover(home, "", picked, "")
	want := []Candidate{{picked, "picked"}, {local, "path"}, {desktop, "desktop"}}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	}
}

func TestSelectionRefusesWhatCouldBeSwappedOrIsNotAProgram(t *testing.T) {
	skipOnWindows(t)
	rules := DefaultRules()
	base := t.TempDir()
	if err := os.Chmod(base, 0o755); err != nil {
		t.Fatal(err)
	}
	good := program(t, filepath.Join(base, "ok"), "claude", 0o755)
	if _, err := rules.Check(good); err != nil {
		t.Fatalf("a program in a closed folder is refused: %v", err)
	}
	groupWritable := program(t, filepath.Join(base, "gw"), "claude", 0o775)
	worldWritable := program(t, filepath.Join(base, "ww"), "claude", 0o757)
	notExec := program(t, filepath.Join(base, "ne"), "claude", 0o644)
	openDir := filepath.Join(base, "od")
	inOpenDir := program(t, openDir, "claude", 0o755)
	if err := os.Chmod(openDir, 0o775); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "dir", "claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(base, "dangling")
	if err := os.Symlink(filepath.Join(base, "nowhere"), dangling); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]string{
		"a group-writable file": groupWritable, "a world-writable file": worldWritable,
		"a file that is not executable": notExec, "a file in a group-writable folder": inOpenDir,
		"a directory": dir, "a dangling link": dangling, "a relative path": "claude", "a missing file": filepath.Join(base, "none"),
	} {
		if _, err := rules.Check(p); err != errNotFound {
			t.Errorf("%s: err = %v, want not found", name, err)
		}
	}
	other := rules
	other.Owner = func(fs.FileInfo) (uint32, bool) { return rules.UID + 1000, true }
	if _, err := other.Check(good); err != errNotFound {
		t.Errorf("a file owned by another user: err = %v", err)
	}
	root := rules
	root.Owner = func(fs.FileInfo) (uint32, bool) { return 0, true }
	if _, err := root.Check(good); err != nil {
		t.Errorf("a file owned by root is refused: %v", err)
	}
}

func TestAStickyWorldWritableFolderAboveTheProgramIsAccepted(t *testing.T) {
	skipOnWindows(t)
	base := t.TempDir()
	sticky := filepath.Join(base, "tmpish")
	if err := os.Mkdir(sticky, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sticky, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	p := program(t, filepath.Join(sticky, "x"), "claude", 0o755)
	if _, err := DefaultRules().Check(p); err != nil {
		t.Fatalf("a sticky folder like /tmp must not refuse the program: %v", err)
	}
}

func TestWindowsRulesTakeOnlyARealExeAndSkipTheModeCheck(t *testing.T) {
	win := DefaultRules()
	win.GOOS = "windows"
	dir := t.TempDir()
	exe := program(t, dir, "claude.exe", 0o644) // modes are synthesized on Windows
	if _, err := win.Check(exe); err != nil {
		t.Fatalf("a .exe is refused: %v", err)
	}
	for _, name := range []string{"claude.cmd", "claude.bat", "claude.ps1", "claude"} {
		if _, err := win.Check(program(t, dir, name, 0o755)); err != errNotFound {
			t.Errorf("%s: err = %v, want not found", name, err)
		}
	}
}

func TestSelectionIsCheckedAgainBeforeEveryCall(t *testing.T) {
	skipOnWindows(t)
	p, f := newProvider(t, claudetest.Script{Steps: []claudetest.Step{step(stream("ok"))}})
	if _, _, err := collect(t, p, req(userText("x"))); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f.Path, 0o777); err != nil { // the target became writable by anyone
		t.Fatal(err)
	}
	before := len(f.Calls(t))
	_, _, err := collect(t, p, req(userText("x")))
	if err == nil || err.Error() != errMissing.Message || len(f.Calls(t)) != before {
		t.Fatalf("err = %v, calls %d -> %d", err, before, len(f.Calls(t)))
	}
}

func TestRepointingTheInstallLinkKeepsTheSamePathForConsent(t *testing.T) {
	skipOnWindows(t)
	rules := DefaultRules()
	base := t.TempDir()
	v1 := program(t, filepath.Join(base, "versions", "1"), "claude", 0o755)
	v2 := program(t, filepath.Join(base, "versions", "2"), "claude", 0o755)
	link := filepath.Join(base, "bin", "claude")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(v1, link); err != nil {
		t.Fatal(err)
	}
	a, err := rules.Check(link)
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(link)
	if err := os.Symlink(v2, link); err != nil {
		t.Fatal(err)
	}
	b, err := rules.Check(link)
	if err != nil || a.Path != b.Path || a.Target == b.Target {
		t.Fatalf("path %q -> %q, target %q -> %q, err %v", a.Path, b.Path, a.Target, b.Target, err)
	}
}

func TestTheDesktopFolderResolvesToItsNewestProgram(t *testing.T) {
	skipOnWindows(t)
	rules := DefaultRules()
	root := filepath.Join(t.TempDir(), "claude-code")
	old := program(t, filepath.Join(root, "2.1.304", "aa", "claude.app", "Contents", "MacOS"), "claude", 0o755)
	newest := program(t, filepath.Join(root, "2.1.308", "bb", "claude.app", "Contents", "MacOS"), "claude", 0o755)
	program(t, filepath.Join(root, "2.1.99", "cc", "claude.app", "Contents", "MacOS"), "claude", 0o755) // 99 < 308 as numbers
	got, err := rules.Check(root)
	if err != nil || got.Path != root || got.Target != resolved(t, newest) {
		t.Fatalf("got %+v err %v (old %s)", got, err, old)
	}
	if _, err := rules.Check(filepath.Dir(root)); err != errNotFound {
		t.Fatalf("a folder not named claude-code was accepted: %v", err)
	}
}

func resolved(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A refusal for access other users have names what to fix, and every other
// refusal names nothing (KTD6).
func TestOpenToNamesOnlyWhatOtherUsersCouldChange(t *testing.T) {
	skipOnWindows(t)
	rules := DefaultRules()
	base, err := filepath.EvalSymlinks(t.TempDir()) // OpenTo names resolved paths
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(base, 0o755); err != nil {
		t.Fatal(err)
	}
	openDir := filepath.Join(base, "od")
	inOpenDir := program(t, openDir, "claude", 0o755)
	if err := os.Chmod(openDir, 0o775); err != nil {
		t.Fatal(err)
	}
	groupWritable := program(t, filepath.Join(base, "gw"), "claude", 0o775)
	notExec := program(t, filepath.Join(base, "ne"), "claude", 0o644)
	good := program(t, filepath.Join(base, "ok"), "claude", 0o755)
	for name, c := range map[string]struct{ path, want string }{
		"a folder open to the group":  {inOpenDir, openDir},
		"a group-writable file":       {groupWritable, groupWritable},
		"a file that is not runnable": {notExec, ""},
		"a missing file":              {filepath.Join(base, "none"), ""},
		"a program that passes":       {good, ""},
	} {
		if got := rules.OpenTo(c.path); got != c.want {
			t.Errorf("%s: OpenTo = %q, want %q", name, got, c.want)
		}
	}
}
