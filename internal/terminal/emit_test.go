package terminal

import (
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// recorder collects what an emitter sends to the UI.
type recorder struct {
	mu     sync.Mutex
	chunks []string
}

func (r *recorder) emit(s string) {
	r.mu.Lock()
	r.chunks = append(r.chunks, s)
	r.mu.Unlock()
}

func (r *recorder) all() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.chunks, "")
}

func (r *recorder) check(t *testing.T, maxLen int) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.chunks {
		if !utf8.ValidString(c) {
			t.Fatalf("chunk %q is not valid UTF-8", c)
		}
		if len(c) > maxLen {
			t.Fatalf("chunk of %d bytes exceeds the %d-byte cap", len(c), maxLen)
		}
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestMultiByteCharacterSplitAcrossReadsArrivesIntact(t *testing.T) {
	r := &recorder{}
	e := newEmitter(r.emit)
	e.start()
	euro := []byte("€") // three bytes
	e.Write(append([]byte("a"), euro[:2]...))
	waitFor(t, "the complete prefix", func() bool { return r.all() == "a" })
	time.Sleep(3 * emitTick) // the partial rune must stay held over several ticks
	r.check(t, maxEmit)
	if r.all() != "a" {
		t.Fatalf("emitted %q before the rune was complete", r.all())
	}
	e.Write(append(euro[2:], 'b'))
	e.close()
	<-e.done
	r.check(t, maxEmit)
	if got := r.all(); got != "a€b" {
		t.Fatalf("got %q, want %q", got, "a€b")
	}
}

// Output written before the UI subscribes is held, a writer the UI cannot
// keep up with blocks once the buffer is full instead of growing it or
// dropping data, and everything arrives once the UI attaches.
func TestHeldOutputStaysBoundedAndNothingIsDropped(t *testing.T) {
	r := &recorder{}
	e := newEmitter(r.emit)
	e.limit, e.chunk = 64, 16
	want := strings.Repeat("héllo wörld € ", 30)

	wrote := make(chan struct{})
	go func() {
		// Odd-sized writes so runes split across Write calls too.
		for b := []byte(want); len(b) > 0; {
			n := min(7, len(b))
			if _, err := e.Write(b[:n]); err != nil {
				t.Error(err)
			}
			b = b[n:]
		}
		close(wrote)
	}()

	time.Sleep(20 * time.Millisecond)
	select {
	case <-wrote:
		t.Fatal("writer was not held back by the full buffer")
	default:
	}
	e.mu.Lock()
	buffered := len(e.buf)
	e.mu.Unlock()
	if buffered > e.limit {
		t.Fatalf("buffer grew to %d bytes, cap %d", buffered, e.limit)
	}
	if r.all() != "" {
		t.Fatal("output emitted before the UI attached")
	}

	e.start()
	<-wrote
	e.close()
	<-e.done
	r.check(t, e.chunk)
	if got := r.all(); got != want {
		t.Fatalf("lost or reordered output: got %d bytes, want %d", len(got), len(want))
	}
}

func TestStopReleasesABlockedWriter(t *testing.T) {
	e := newEmitter(func(string) {})
	e.limit = 4
	errc := make(chan error, 1)
	go func() { _, err := e.Write([]byte("more than four")); errc <- err }()
	time.Sleep(10 * time.Millisecond)
	e.stop()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("blocked write succeeded after stop")
		}
	case <-time.After(time.Second):
		t.Fatal("stop left the writer blocked")
	}
	<-e.done
}
