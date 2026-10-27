package terminal

import (
	"bytes"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// A subscriber that never reads must not slow the terminal: all of 10 MiB
// reaches the emitter while that subscriber's queue overflows and records
// a gap (KTD38). A tap that blocked would stall the copy and time out.
func TestStalledSubscriberDoesNotSlowTheTerminal(t *testing.T) {
	const total = 10 << 20
	var got atomic.Int64
	r := NewRegistry(func(_, s string) { got.Add(int64(len(s))) }, func(string, string) {})
	p := newPipe()
	id := r.Open("s1", p)
	sub, err := r.Subscribe(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Attach(id); err != nil {
		t.Fatal(err)
	}
	go func() {
		chunk := bytes.Repeat([]byte("x"), 32<<10)
		for sent := 0; sent < total; sent += len(chunk) {
			p.remote.Write(chunk)
		}
	}()
	// The emitter paces about 4 MB/s, so 10 MiB takes near 3 s on its own.
	for end := time.Now().Add(20 * time.Second); got.Load() < total; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("emitter got %d of %d bytes", got.Load(), total)
		}
	}
	if sub.Gaps() == 0 {
		t.Fatal("the stalled subscriber's overflow recorded no gap")
	}
	r.Close(id)
}

// Write returns at once with the whole length, however full a queue is.
func TestTapWriteNeverBlocks(t *testing.T) {
	tp := newTap()
	sub := tp.subscribe()
	chunk := bytes.Repeat([]byte("y"), 32<<10)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 4 * tapQueue {
			if n, err := tp.Write(chunk); n != len(chunk) || err != nil {
				t.Errorf("Write = %d, %v", n, err)
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Write blocked on a full subscriber queue")
	}
	if sub.Gaps() != 3*tapQueue {
		t.Fatalf("gaps = %d, want %d", sub.Gaps(), 3*tapQueue)
	}
}

// Recent is the last 256 KiB, cut so no UTF-8 character is split.
func TestRecentKeepsTheTailOnCharacterBoundaries(t *testing.T) {
	r, _ := newLoggedRegistry()
	p := newPipe()
	id := r.Open("s1", p)
	r.Attach(id)
	// 1 + 3k bytes, so 256 KiB from the end falls inside a "€".
	out := "a" + strings.Repeat("€", 100000)
	go p.remote.Write([]byte(out))
	var recent []byte
	waitFor(t, "the output in the ring", func() bool {
		recent, _ = r.Recent(id)
		return len(recent) > 0 && strings.HasSuffix(out, string(recent)) && len(recent) > tapRing-utf8.UTFMax
	})
	if !utf8.Valid(recent) || len(recent) > tapRing {
		t.Fatalf("recent is %d bytes, valid UTF-8 %v", len(recent), utf8.Valid(recent))
	}
	// A character cut at the end waits for its last byte.
	euro := []byte("€")
	p.remote.Write(euro[:2])
	time.Sleep(20 * time.Millisecond)
	if recent, _ = r.Recent(id); !utf8.Valid(recent) {
		t.Fatal("recent ends inside a character")
	}
	r.Close(id)
}

// After Close every subscriber sees the end and the ring is freed.
func TestCloseEndsSubscribersAndFreesTheRing(t *testing.T) {
	r, _ := newLoggedRegistry()
	p := newPipe()
	id := r.Open("s1", p)
	r.Attach(id)
	sub, err := r.Subscribe(id)
	if err != nil {
		t.Fatal(err)
	}
	p.remote.Write([]byte("hello"))
	select {
	case b := <-sub.C:
		if string(b) != "hello" {
			t.Fatalf("subscriber got %q", b)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber got nothing")
	}
	tp := r.get(id).tap
	r.Close(id)
	select {
	case _, open := <-sub.C:
		if open {
			t.Fatal("subscriber got data after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber did not see the end")
	}
	tp.mu.Lock()
	freed := tp.ring == nil
	tp.mu.Unlock()
	if !freed {
		t.Fatal("ring kept after Close")
	}
	if _, err := r.Recent(id); err == nil {
		t.Fatal("Recent works on a closed terminal")
	}
	if _, err := r.Subscribe(id); err == nil {
		t.Fatal("Subscribe works on a closed terminal")
	}
}

// A terminal that ends on its own ends its subscribers too.
func TestRemoteEndEndsSubscribers(t *testing.T) {
	r, _ := newLoggedRegistry()
	p := newPipe()
	id := r.Open("s1", p)
	r.Attach(id)
	sub, _ := r.Subscribe(id)
	p.remote.Close()
	select {
	case _, open := <-sub.C:
		if open {
			t.Fatal("unexpected data")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber did not see the end")
	}
}
