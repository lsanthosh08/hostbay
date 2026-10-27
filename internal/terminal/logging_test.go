package terminal

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLinesStripANSIAndResolveRedraws(t *testing.T) {
	var lp lines
	var out []string
	emit := func(s string) { out = append(out, s) }
	lp.feed([]byte("\x1b[1;32muser@web\x1b[0m:~$ ls\r\n"), emit)
	lp.feed([]byte("50%\r100%\r\n"), emit)              // a progress line redrawn in place
	lp.feed([]byte("tpyo\b\b\b\bty\x1b[Kpo\r\n"), emit) // readline editing
	lp.feed([]byte("\x1b]0;title\x07caf\xc3"), emit)    // an OSC title, then a split character
	lp.feed([]byte("\xa9\n"), emit)
	want := []string{"user@web:~$ ls", "100%", "typo", "café"}
	if strings.Join(out, "|") != strings.Join(want, "|") {
		t.Fatalf("lines = %q, want %q", out, want)
	}
	lp.feed([]byte("Password: "), emit)
	if got := lp.current(); got != "Password: " {
		t.Fatalf("current = %q", got)
	}
}

// A remote can ask to move the cursor two billion columns right. The line
// must stay within maxLine, or the logger pads it with gigabytes of spaces
// and takes the whole app down.
func TestLinesBoundCursorForward(t *testing.T) {
	for _, in := range []string{"\x1b[200000Cx", "\x1b[2000000000Cx", "ab\x1b[9223372036854775807Cx"} {
		var lp lines
		var out []string
		lp.feed([]byte(in), func(s string) { out = append(out, s) })
		for _, s := range append(out, lp.current()) {
			if n := len([]rune(s)); n > maxLine+1 {
				t.Fatalf("%q made a %d-rune line, want at most %d", in, n, maxLine+1)
			}
		}
	}
}

// lockedBuf is a log file the test reads while the logger writes.
type lockedBuf struct {
	mu     sync.Mutex
	b      bytes.Buffer
	closed bool
	fail   error
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fail != nil {
		return 0, l.fail
	}
	return l.b.Write(p)
}
func (l *lockedBuf) Close() error { l.mu.Lock(); l.closed = true; l.mu.Unlock(); return nil }
func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// On SSH the remote echoes nothing while sudo reads a password, so the
// output a log holds has no password in it; the saved password, printed by
// the remote, is scrubbed.
func TestLogHoldsOutputOnlyAndScrubsSavedSecrets(t *testing.T) {
	r, _ := newLoggedRegistry()
	p := newPipe()
	id := r.Open("s", p)
	sub, _ := r.Subscribe(id)
	f := &lockedBuf{}
	l := StartLog(sub, f, []string{"s3cret-pw"}, nil)

	p.remote.Write([]byte("$ sudo -k; sudo ls\r\n[sudo] password for root: "))
	r.Write(id, []byte("hunter2\r")) // the remote echoes nothing back
	p.remote.Write([]byte("\r\nfile.txt\r\n$ echo s3cret-pw\r\ns3cret-pw\r\n"))
	waitFor(t, "the output", func() bool {
		return strings.Contains(f.String(), "file.txt") && strings.Count(f.String(), "[secret]") == 2
	})
	l.Stop()
	got := f.String()
	if strings.Contains(got, "hunter2") || strings.Contains(got, "s3cret-pw") {
		t.Fatalf("the log holds a secret:\n%s", got)
	}
	if !f.closed {
		t.Fatal("Stop left the file open")
	}
}

// Serial and raw TCP add typed bytes to the output in Go when local echo is
// on; the log reads the tap, which never sees them.
func TestLogOfSerialWithLocalEchoHoldsNoTypedText(t *testing.T) {
	r, _ := newLoggedRegistry()
	port, dev := newFakePort()
	id := r.Open("s", newStreamTerm(port, true, "lost"))
	sub, _ := r.Subscribe(id)
	f := &lockedBuf{}
	l := StartLog(sub, f, nil, nil)
	go io.Copy(io.Discard, dev.hear)
	r.Write(id, []byte("hunter2\r"))
	dev.say.Write([]byte("login ok\r\n"))
	waitFor(t, "the output", func() bool { return strings.Contains(f.String(), "login ok") })
	l.Stop()
	if strings.Contains(f.String(), "hunter2") {
		t.Fatalf("typed text reached the log: %q", f.String())
	}
}

func TestLogShowsAGapAndStopsOnAWriteError(t *testing.T) {
	r, _ := newLoggedRegistry()
	p := newPipe()
	id := r.Open("s", p)
	sub, _ := r.Subscribe(id)
	// Fill the queue before the logger reads, so chunks are dropped.
	for i := 0; i < tapQueue+5; i++ {
		p.remote.Write([]byte("x\n"))
	}
	waitFor(t, "a gap", func() bool { return sub.Gaps() > 0 })
	f := &lockedBuf{}
	stopped := make(chan error, 1)
	StartLog(sub, f, nil, func(err error) { stopped <- err })
	p.remote.Write([]byte("after\n"))
	waitFor(t, "the gap line", func() bool { return strings.Contains(f.String(), gapLine) })

	f.mu.Lock()
	f.fail = errors.New("disk full")
	f.mu.Unlock()
	p.remote.Write([]byte("more\n"))
	select {
	case err := <-stopped:
		if err == nil || !strings.Contains(err.Error(), "disk full") {
			t.Fatalf("stopped with %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a write error did not stop logging")
	}
	// The terminal keeps working.
	if err := r.Write(id, []byte("ls\r")); err != nil {
		t.Fatal(err)
	}
	p.remote.Write([]byte("still here\n"))
}

func TestOpenLogFileIsPrivateAndNeverFollowsALink(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	at := time.Date(2026, 10, 25, 9, 30, 0, 0, time.Local)
	f, err := OpenLogFile(dir, "web/../prod", at)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	want := filepath.Join(dir, "web-..-prod-20261025-093000.log")
	if f.Name() != want {
		t.Fatalf("name = %s, want %s", f.Name(), want)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(want); fi.Mode().Perm() != 0o600 {
			t.Fatalf("file mode %v", fi.Mode().Perm())
		}
		if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
			t.Fatalf("folder mode %v", fi.Mode().Perm())
		}
	}

	// A collision gets a numbered suffix.
	g, err := OpenLogFile(dir, "web/../prod", at)
	if err != nil {
		t.Fatal(err)
	}
	g.Close()
	if g.Name() != strings.TrimSuffix(want, ".log")+"-2.log" {
		t.Fatalf("second name = %s", g.Name())
	}

	if runtime.GOOS == "windows" {
		return // symlinks need privileges there
	}
	// A link planted at the next name is never written through.
	target := filepath.Join(t.TempDir(), "target")
	os.WriteFile(target, []byte("untouched"), 0o600)
	os.Symlink(target, strings.TrimSuffix(want, ".log")+"-3.log")
	h, err := OpenLogFile(dir, "web/../prod", at)
	if err != nil {
		t.Fatal(err)
	}
	h.WriteString("log line")
	h.Close()
	if b, _ := os.ReadFile(target); string(b) != "untouched" {
		t.Fatalf("wrote through a link: %q", b)
	}
	// A folder that is a link is refused.
	linked := filepath.Join(t.TempDir(), "linked")
	os.Symlink(t.TempDir(), linked)
	if _, err := OpenLogFile(linked, "web", at); err == nil {
		t.Fatal("logged into a linked folder")
	}
}
