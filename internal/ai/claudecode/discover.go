package claudecode

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// Candidate is a program that may be Claude Code, found by path only.
// Listing never runs it (KTD6): a planted file in a writable folder does
// not execute when Settings opens.
type Candidate struct {
	Path   string // as found: the PATH entry, the install link, or the Desktop folder
	Source string // "picked", "path", "native" or "desktop"
}

// Resolved is a candidate that passed the path checks.
type Resolved struct {
	Path   string // as found; consent binds to this
	Target string // the executable it resolves to, which is what runs
}

// errNotFound covers every reason a program is refused. The person is told
// it was not found, never which check failed (KTD6).
var errNotFound = errors.New("the Claude Code program was not found")

// Rules are the platform rules of the path checks. DefaultRules is for this
// computer; tests inject others to run the Windows rules anywhere.
type Rules struct {
	GOOS  string
	Owner func(fs.FileInfo) (uid uint32, ok bool) // who owns a file; nil: unknown
	UID   uint32                                  // this process's user
}

func DefaultRules() Rules {
	return Rules{GOOS: runtime.GOOS, Owner: fileOwner, UID: currentUID()}
}

func (r Rules) windows() bool { return r.GOOS == "windows" }

func (r Rules) owner(fi fs.FileInfo) (uint32, bool) {
	if r.Owner == nil {
		return 0, false
	}
	return r.Owner(fi)
}

// desktopFolderName is the folder Claude Desktop keeps its copies in.
const desktopFolderName = "claude-code"

// Discover lists where Claude Code may be, best first, without running
// anything. picked is a path the person chose in a file dialog (or "").
func (r Rules) Discover(home, pathEnv, picked, appData string) []Candidate {
	var out []Candidate
	seen := map[string]bool{}
	add := func(p, src string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, Candidate{Path: p, Source: src})
	}
	name := r.exeName()
	add(picked, "picked")
	dirs := filepath.SplitList(pathEnv)
	if r.GOOS == "darwin" { // a launched app has a short PATH
		dirs = append(dirs, filepath.Join(home, ".local", "bin"), "/opt/homebrew/bin", "/usr/local/bin")
	}
	for _, d := range dirs {
		if d != "" && filepath.IsAbs(d) {
			add(filepath.Join(d, name), "path")
		}
	}
	if home != "" {
		add(filepath.Join(home, ".local", "bin", name), "native")
	}
	switch {
	case r.GOOS == "darwin" && home != "":
		add(filepath.Join(home, "Library", "Application Support", "Claude", desktopFolderName), "desktop")
	case r.windows() && appData != "":
		add(filepath.Join(appData, "Claude", desktopFolderName), "desktop")
	case home != "":
		add(filepath.Join(home, ".config", "Claude", desktopFolderName), "desktop")
	}
	// Only what exists is worth showing.
	kept := out[:0]
	for _, c := range out {
		if _, err := os.Stat(c.Path); err == nil || c.Source == "picked" {
			kept = append(kept, c)
		}
	}
	return kept
}

// Check applies the path rules (KTD6) and returns what would run. It is run
// again right before every spawn.
func (r Rules) Check(path string) (Resolved, error) {
	res, _, err := r.check(path)
	return res, err
}

// OpenTo names the file or folder that other users could change, when that
// is why Check refuses path. It is "" for every other reason, so the person
// is told how to fix the one problem they can fix and nothing else about the
// machine (KTD6).
func (r Rules) OpenTo(path string) string {
	_, open, _ := r.check(path)
	return open
}

func (r Rules) check(path string) (res Resolved, openTo string, err error) {
	if !filepath.IsAbs(path) {
		return Resolved{}, "", errNotFound
	}
	target, err := r.locate(path)
	if err != nil {
		return Resolved{}, "", errNotFound
	}
	target, err = filepath.EvalSymlinks(target)
	if err != nil {
		return Resolved{}, "", errNotFound
	}
	fi, err := os.Stat(target)
	if err != nil || !fi.Mode().IsRegular() {
		return Resolved{}, "", errNotFound
	}
	if r.windows() {
		// Go reports synthesized modes there, so there is no mode check;
		// only a real executable runs, never a script shim.
		if !strings.EqualFold(filepath.Ext(target), ".exe") {
			return Resolved{}, "", errNotFound
		}
		return Resolved{Path: path, Target: target}, "", nil
	}
	if fi.Mode().Perm()&0o111 == 0 {
		return Resolved{}, "", errNotFound
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return Resolved{}, target, errNotFound
	}
	if uid, ok := r.owner(fi); ok && uid != r.UID && uid != 0 {
		return Resolved{}, target, errNotFound
	}
	// Replacing the program, or the link to it, must be impossible for
	// anyone else: every folder above both is closed to other users.
	for _, p := range []string{target, path} {
		if open, closed := r.openFolder(filepath.Dir(p)); !closed {
			return Resolved{}, open, errNotFound
		}
	}
	return Resolved{Path: path, Target: target}, "", nil
}

// openFolder reports whether dir and every parent can be changed only by this
// user or root. When one can be changed by others it is returned; a folder that
// cannot be read at all is refused without a name. A folder everyone may write
// to is accepted when it is sticky, like /tmp: others cannot replace this
// user's entries in it.
func (r Rules) openFolder(dir string) (open string, closed bool) {
	for {
		fi, err := os.Stat(dir)
		if err != nil || !fi.IsDir() {
			return "", false
		}
		if fi.Mode().Perm()&0o022 != 0 && fi.Mode()&os.ModeSticky == 0 {
			return dir, false
		}
		if uid, ok := r.owner(fi); ok && uid != r.UID && uid != 0 {
			return dir, false
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", true
		}
		dir = parent
	}
}

// locate turns the Desktop folder into its newest program; any other path
// is the program itself.
func (r Rules) locate(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return path, nil
	}
	if filepath.Base(path) != desktopFolderName {
		return "", errNotFound
	}
	exe := r.exeName()
	var found []string
	for _, pat := range []string{
		filepath.Join(path, "*", "*", "claude.app", "Contents", "MacOS", exe),
		filepath.Join(path, "*", "*", exe),
		filepath.Join(path, "*", exe),
	} {
		m, _ := filepath.Glob(pat)
		found = append(found, m...)
	}
	if len(found) == 0 {
		return "", errNotFound
	}
	sort.Slice(found, func(i, j int) bool { return versionOfPath(found[i], path).newer(versionOfPath(found[j], path)) })
	return found[0], nil
}

// exeName is the program's file name on this system.
func (r Rules) exeName() string {
	if r.windows() {
		return "claude.exe"
	}
	return "claude"
}

type semver [3]int

func (a semver) newer(b semver) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

// versionOfPath reads the version folder under the Desktop folder.
func versionOfPath(p, root string) semver {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return semver{}
	}
	v, _, _ := strings.Cut(filepath.ToSlash(rel), "/")
	return parseSemver(v)
}

var semverRE = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

func parseSemver(s string) semver {
	m := semverRE.FindStringSubmatch(s)
	if m == nil {
		return semver{}
	}
	var v semver
	for i := range v {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	return v
}
