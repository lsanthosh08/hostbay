package terminal

import (
	"io"
	"sync"
	"testing"
	"time"
)

// pipeTransport is a terminal whose remote side the test drives.
type pipeTransport struct {
	r      *io.PipeReader
	remote *io.PipeWriter

	mu     sync.Mutex
	input  string
	closed bool
}

func newPipe() *pipeTransport {
	r, w := io.Pipe()
	return &pipeTransport{r: r, remote: w}
}

func (p *pipeTransport) Read(b []byte) (int, error) { return p.r.Read(b) }
func (p *pipeTransport) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.input += string(b)
	return len(b), nil
}
func (p *pipeTransport) Resize(int, int) error { return nil }
func (p *pipeTransport) Close() error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	return p.r.Close()
}
func (p *pipeTransport) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// log records data and closed callbacks in the order they happen.
type log struct {
	mu     sync.Mutex
	events []string
}

func (l *log) add(s string) { l.mu.Lock(); l.events = append(l.events, s); l.mu.Unlock() }
func (l *log) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

func newLoggedRegistry() (*Registry, *log) {
	l := &log{}
	return NewRegistry(
		func(id, s string) { l.add(id + " data " + s) },
		func(id, reason string) { l.add(id + " closed " + reason) },
	), l
}

func TestBannerSentBeforeAttachStillAppears(t *testing.T) {
	r, l := newLoggedRegistry()
	p := newPipe()
	id := r.Open("s1", p)
	p.remote.Write([]byte("Welcome\r\n$ "))
	time.Sleep(3 * emitTick)
	if len(l.get()) != 0 {
		t.Fatalf("emitted before attach: %v", l.get())
	}
	if err := r.Attach(id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the banner", func() bool { ev := l.get(); return len(ev) == 1 && ev[0] == id+" data Welcome\r\n$ " })
	r.Close(id)
}

func TestRemoteEndFlushesOutputThenReportsClosed(t *testing.T) {
	r, l := newLoggedRegistry()
	p := newPipe()
	id := r.Open("s1", p)
	p.remote.Write([]byte("bye"))
	p.remote.Close() // the shell exits before the UI attached
	time.Sleep(3 * emitTick)
	r.Attach(id)
	waitFor(t, "closed", func() bool { return len(l.get()) == 2 })
	if ev := l.get(); ev[0] != id+" data bye" || ev[1] != id+" closed session closed" {
		t.Fatalf("events = %q", ev)
	}
	if err := r.Write(id, []byte("x")); err == nil {
		t.Fatal("write to an ended terminal succeeded")
	}
	if !p.isClosed() {
		t.Fatal("transport left open")
	}
}

func TestCloseOwnerClosesOnlyThatSessionsTerminals(t *testing.T) {
	r, l := newLoggedRegistry()
	a1, a2, b := newPipe(), newPipe(), newPipe()
	ida1, ida2, idb := r.Open("A", a1), r.Open("A", a2), r.Open("B", b)
	if len(ida1) != 32 || ida1 == ida2 || ida2 == idb {
		t.Fatalf("ids not opaque and distinct: %q %q %q", ida1, ida2, idb)
	}

	r.CloseOwner("A")
	r.Close(ida1) // closing again does nothing
	if !a1.isClosed() || !a2.isClosed() || b.isClosed() {
		t.Fatal("CloseOwner closed the wrong terminals")
	}
	if r.Write(ida1, []byte("x")) == nil {
		t.Fatal("write after close succeeded")
	}
	if err := r.Write(idb, []byte("ls\r")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "two closed reports", func() bool { return len(l.get()) == 2 })
	r.Close(idb)
}
