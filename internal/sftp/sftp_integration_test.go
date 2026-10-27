package sftp

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"hostbay/internal/apperr"
	"hostbay/internal/sftp/sftptest"
)

const loginPW = "s3cret pw'$(x)"

// setup starts an in-process server and returns an SFTP connection to it and
// a scratch directory on its (local) filesystem.
func setup(t *testing.T) (*sftptest.Server, *Conn, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the test server runs commands with /bin/sh")
	}
	s := sftptest.New(t, loginPW)
	var p Pool
	c, err := p.Get(s.Dial(t))
	if err != nil {
		t.Fatal(err)
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s, c, dir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestWriteShorterContentLeavesNoOldTail(t *testing.T) {
	_, c, dir := setup(t)
	p := filepath.Join(dir, "f.txt")
	writeFile(t, p, "a much longer original content\n")
	if err := c.Write(p, "short\n", false, ""); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, p); got != "short\n" {
		t.Fatalf("content = %q", got)
	}
}

var tmpRe = regexp.MustCompile(`/tmp/\.hostbay-[0-9a-f]+`)

func TestSudoWriteFeedsPasswordOnStdinAndKeepsTheFile(t *testing.T) {
	s, c, dir := setup(t)
	p := filepath.Join(dir, "root owned.conf")
	writeFile(t, p, "old content that is longer\n")
	s.Protect(t, p)
	before, _ := os.Stat(p)

	if err := c.Write(p, "new\n", true, loginPW); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, p); got != "new\n" {
		t.Fatalf("content = %q", got)
	}
	after, _ := os.Stat(p)
	if !os.SameFile(before, after) || after.Mode().Perm() != 0o444 {
		t.Fatalf("file replaced or mode changed: %v -> %v", before.Mode(), after.Mode())
	}
	cmds := s.Commands()
	if len(cmds) == 0 {
		t.Fatal("no sudo command ran")
	}
	for _, cmd := range cmds {
		if strings.Contains(cmd, loginPW) || strings.Contains(cmd, "s3cret") {
			t.Fatalf("password on the command line: %q", cmd)
		}
		if tmp := tmpRe.FindString(cmd); tmp != "" {
			if _, err := os.Stat(tmp); !os.IsNotExist(err) {
				t.Fatalf("temp file %s left behind", tmp)
			}
		}
	}
}

func TestPermissionDeniedWriteFallsBackToSudo(t *testing.T) {
	s, c, dir := setup(t)
	p := filepath.Join(dir, "hosts")
	writeFile(t, p, "old\n")
	s.Protect(t, p)
	if err := c.Write(p, "new\n", false, loginPW); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, p); got != "new\n" {
		t.Fatalf("content = %q", got)
	}
}

func TestSudoWrongPasswordLeavesFileUnchanged(t *testing.T) {
	s, c, dir := setup(t)
	p := filepath.Join(dir, "hosts")
	writeFile(t, p, "old\n")
	s.Protect(t, p)

	err := c.Write(p, "new\n", true, "wrong")
	var e *apperr.Error
	if !errors.As(err, &e) || e.Code != apperr.SecretNeeded || e.Details["kind"] != "sudo" ||
		!strings.Contains(e.Message, "password incorrect") {
		t.Fatalf("err = %#v", err)
	}
	if got := readFile(t, p); got != "old\n" {
		t.Fatalf("content = %q", got)
	}
}

func TestSudoWithoutPasswordAsksForOne(t *testing.T) {
	_, c, dir := setup(t)
	err := c.Remove(filepath.Join(dir, "x"), false, true, "")
	if apperr.CodeOf(err) != apperr.SecretNeeded || !strings.Contains(err.Error(), "requires a password") {
		t.Fatalf("err = %v", err)
	}
}

func TestHostileNamesStayOnePathThroughTheShell(t *testing.T) {
	_, c, dir := setup(t)
	names := []string{"sp ace", "it's", "two\nlines", "-rf", "$(touch PWNED)", "`touch PWNED2`", "a;touch PWNED3", "*", `"dq" \ $HOME`}
	for _, n := range names {
		p := filepath.Join(dir, n)
		if err := c.Write(p, "body of "+n, true, loginPW); err != nil {
			t.Fatalf("write %q: %v", n, err)
		}
		if got := readFile(t, p); got != "body of "+n {
			t.Fatalf("%q content = %q", n, got)
		}
		if err := c.Copy(p, p+".copy", true, loginPW); err != nil {
			t.Fatalf("copy %q: %v", n, err)
		}
		if got := readFile(t, p+".copy"); got != "body of "+n {
			t.Fatalf("%q copy = %q", n, got)
		}
		if err := c.Chmod(p, 0o600, true, loginPW); err != nil {
			t.Fatalf("chmod %q: %v", n, err)
		}
		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
			t.Fatalf("%q mode = %v", n, fi.Mode())
		}
		if err := c.Remove(p, false, true, loginPW); err != nil {
			t.Fatalf("remove %q: %v", n, err)
		}
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%q still exists", n)
		}
	}
	left, _ := os.ReadDir(dir)
	if len(left) != len(names) { // only the .copy files
		t.Fatalf("unexpected files: %v", left)
	}
	for _, d := range left {
		if !strings.HasSuffix(d.Name(), ".copy") {
			t.Fatalf("unexpected file %q", d.Name())
		}
	}
}

func TestListPutsDirsFirstWithIsDir(t *testing.T) {
	_, c, dir := setup(t)
	writeFile(t, filepath.Join(dir, "b.txt"), "12345")
	writeFile(t, filepath.Join(dir, "A.txt"), "")
	if err := os.Mkdir(filepath.Join(dir, "zdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := c.List(dir + "/")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range got {
		names = append(names, e.Name)
	}
	if strings.Join(names, ",") != "zdir,A.txt,b.txt" {
		t.Fatalf("order = %v", names)
	}
	if !got[0].IsDir || got[0].Path != dir+"/zdir" || got[2].Size != 5 || got[2].Mode&0o777 != 0o644 || got[2].Mtime == 0 {
		t.Fatalf("entries = %+v", got)
	}
}

func TestUploadDownloadRoundTripAndDownloadIsAtomic(t *testing.T) {
	_, c, dir := setup(t)
	data := make([]byte, 3<<20+17)
	rand.Read(data)
	local := filepath.Join(t.TempDir(), "in.bin")
	if err := os.WriteFile(local, data, 0o600); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(dir, "up.bin")
	writeFile(t, remote, strings.Repeat("x", 5<<20)) // longer: upload must truncate
	if err := c.Upload(local, remote); err != nil {
		t.Fatal(err)
	}
	outDir := t.TempDir()
	out := filepath.Join(outDir, "out.bin")
	if err := c.Download(remote, out); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, data) {
		t.Fatalf("round trip differs: %d bytes", len(got))
	}

	// A failed download keeps the existing local file and leaves no temp.
	if err := c.Download(filepath.Join(dir, "missing"), out); err == nil {
		t.Fatal("download of a missing file succeeded")
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, data) {
		t.Fatal("failed download changed the local file")
	}
	if left, _ := os.ReadDir(outDir); len(left) != 1 {
		t.Fatalf("temp files left: %v", left)
	}
}

func TestPoolSharesOneClientPerConnectionAndDropsDeadOnes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test server runs commands with /bin/sh")
	}
	s := sftptest.New(t, loginPW)
	var p Pool
	ssh1 := s.Dial(t)
	a, err := p.Get(ssh1)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := p.Get(ssh1)
	if a.sftp != b.sftp || s.SFTPOpens() != 1 {
		t.Fatalf("second Get opened another client (%d opens)", s.SFTPOpens())
	}

	ssh1.Close() // what the manager does to the old client on a rebuild or last release
	deadline := time.Now().Add(2 * time.Second)
	for p.size() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("closed connection's SFTP client still pooled")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := p.Get(s.Dial(t)); err != nil || s.SFTPOpens() != 2 {
		t.Fatalf("new connection: err=%v opens=%d", err, s.SFTPOpens())
	}
}

func (p *Pool) size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.m)
}

func TestPoolRemembersARefusedSubsystemUntilTheConnectionEnds(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test server runs commands with /bin/sh")
	}
	s := sftptest.New(t, loginPW)
	s.RefuseSFTP()
	var p Pool
	c := s.Dial(t)
	for range 2 {
		if _, err := p.Get(c); !errors.Is(err, ErrNoSFTP) {
			t.Fatalf("err = %v, want ErrNoSFTP", err)
		}
	}
	if n := s.SFTPOpens(); n != 1 {
		t.Fatalf("subsystem asked for %d times, want once per connection", n)
	}
	c.Close()
	if _, err := p.Get(s.Dial(t)); !errors.Is(err, ErrNoSFTP) || s.SFTPOpens() != 2 {
		t.Fatalf("new connection: err=%v requests=%d", err, s.SFTPOpens())
	}
}

func TestStatReportsAMissingFileAsNotExist(t *testing.T) {
	_, c, dir := setup(t)
	if _, err := c.Stat(filepath.Join(dir, "nope")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v", err)
	}
	writeFile(t, filepath.Join(dir, "f"), "abc")
	e, err := c.Stat(filepath.Join(dir, "f"))
	if err != nil || e.Size != 3 || e.IsDir || e.Name != "f" {
		t.Fatalf("entry %+v, err %v", e, err)
	}
}

func TestCreateOverwriteKeepsModeAndSymlink(t *testing.T) {
	_, c, dir := setup(t)
	target := filepath.Join(dir, "run.sh")
	writeFile(t, target, "old")
	if err := os.Chmod(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.sh")
	if err := os.Symlink("run.sh", link); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{target, link} {
		u, err := c.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(u, "new via "+filepath.Base(p)); err != nil {
			t.Fatal(err)
		}
		if err := u.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	if got := readFile(t, target); got != "new via link.sh" {
		t.Fatalf("target = %q", got)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("link replaced by a regular file")
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v", fi.Mode())
	}
	if names, _ := filepath.Glob(filepath.Join(dir, "*.part")); len(names) > 0 {
		t.Fatalf("temp files left: %v", names)
	}
}

func TestCreateAbortLeavesTheOriginalAndNoTempFile(t *testing.T) {
	_, c, dir := setup(t)
	p := filepath.Join(dir, "f.txt")
	writeFile(t, p, "original")
	u, err := c.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(u, "partial")
	if err := u.Abort(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, p); got != "original" {
		t.Fatalf("content = %q", got)
	}
	if names, _ := os.ReadDir(dir); len(names) != 1 {
		t.Fatalf("left behind: %v", names)
	}
}
