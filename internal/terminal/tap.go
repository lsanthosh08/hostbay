package terminal

import (
	"bytes"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"unicode/utf8"
)

// The output tap (KTD38). Every terminal keeps its last tapRing bytes for
// agent reads and copies its output to subscribers (the completion scanner,
// the logger, post-login rules) over bounded queues. It never blocks: the
// emitter already blocks the transport read when the page falls behind
// (KTD9), so a full queue drops that chunk for that subscriber and counts a
// gap instead. Go keeps no other scrollback.
const (
	tapRing  = 256 << 10
	tapQueue = 64 // chunks a subscriber may fall behind before it loses one
)

type tap struct {
	mu     sync.Mutex
	ring   []byte
	subs   []*Subscription
	closed bool
}

func newTap() *tap { return &tap{} }

// Subscription receives a terminal's output from when it subscribed. C is
// closed when the terminal ends or the subscription is closed. Chunks are
// shared between subscribers and must not be changed.
type Subscription struct {
	C    <-chan []byte
	c    chan []byte
	gaps atomic.Uint64
	t    *tap
}

// Gaps counts the chunks dropped because the subscriber fell behind.
func (s *Subscription) Gaps() uint64 { return s.gaps.Load() }

// Close ends the subscription.
func (s *Subscription) Close() { s.t.unsubscribe(s) }

// Write records p and offers it to every subscriber. It always takes all
// of p at once.
func (t *tap) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return len(p), nil
	}
	t.ring = append(t.ring, p...)
	if len(t.ring) > tapRing+tapRing/4 { // trim in batches, not on every write
		t.ring = append(t.ring[:0], t.ring[len(t.ring)-tapRing:]...)
	}
	if len(t.subs) > 0 {
		b := bytes.Clone(p) // the caller reuses p
		for _, s := range t.subs {
			select {
			case s.c <- b:
			default:
				s.gaps.Add(1)
			}
		}
	}
	return len(p), nil
}

// recent is the last tapRing bytes, without a character split by that edge
// or one whose last bytes have not arrived.
func (t *tap) recent() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	b := t.ring[max(0, len(t.ring)-tapRing):]
	for i := 0; i < utf8.UTFMax && len(b) > 0 && !utf8.RuneStart(b[0]); i++ {
		b = b[1:]
	}
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if !utf8.FullRune(b[i:]) {
				b = b[:i]
			}
			break
		}
	}
	return bytes.Clone(b)
}

// subscribe returns a new subscription, or nil once the tap is closed.
func (t *tap) subscribe() *Subscription {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	c := make(chan []byte, tapQueue)
	s := &Subscription{C: c, c: c, t: t}
	t.subs = append(t.subs, s)
	return s
}

func (t *tap) unsubscribe(s *Subscription) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if i := slices.Index(t.subs, s); i >= 0 {
		t.subs = slices.Delete(t.subs, i, i+1)
		close(s.c)
	}
}

// close ends every subscription and frees the ring. Later writes are dropped.
func (t *tap) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed, t.ring = true, nil
	for _, s := range t.subs {
		close(s.c)
	}
	t.subs = nil
}

// teeTap shows output to the tap, then hands it on.
type teeTap struct {
	t    *tap
	next io.Writer
}

func (w teeTap) Write(p []byte) (int, error) {
	w.t.Write(p)
	return w.next.Write(p)
}
