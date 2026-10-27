package terminal

import (
	"errors"
	"sync"
	"time"
	"unicode/utf8"
)

// Pacing for output sent to the UI (KTD9). Unpaced Wails events starve the
// page (docs/spikes/2026-10-22-wails-streaming-spike.md); 64 KB per 16 ms is
// about 4 MB/s, under the 5 MB/s the spike measured as safe.
const (
	emitTick  = 16 * time.Millisecond
	maxEmit   = 64 << 10  // bytes per event
	bufferCap = 256 << 10 // bytes held before the transport read blocks
)

var errStopped = errors.New("terminal closed")

// emitter batches a terminal's output into paced, bounded events. It holds
// output until start (the UI has subscribed), never splits a UTF-8 character
// across events, and never drops data: when the buffer is full, Write blocks,
// which stops the transport read and lets SSH flow control slow the sender.
type emitter struct {
	emit         func(string)
	limit, chunk int // buffer cap, bytes per event

	mu      sync.Mutex
	cond    *sync.Cond
	buf     []byte
	started bool
	closed  bool // no more writes; flush what is left
	stopped bool // discard everything and end now
	done    chan struct{}
}

func newEmitter(emit func(string)) *emitter {
	e := &emitter{emit: emit, limit: bufferCap, chunk: maxEmit, done: make(chan struct{})}
	e.cond = sync.NewCond(&e.mu)
	go e.run()
	return e
}

// Write queues p, blocking while the buffer is full.
func (e *emitter) Write(p []byte) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for len(p) > 0 {
		for !e.stopped && !e.closed && len(e.buf) >= e.limit {
			e.cond.Wait()
		}
		if e.stopped || e.closed {
			return n, errStopped
		}
		k := min(len(p), e.limit-len(e.buf))
		e.buf = append(e.buf, p[:k]...)
		p, n = p[k:], n+k
		e.cond.Broadcast()
	}
	return n, nil
}

func (e *emitter) start() { e.set(func() { e.started = true }) }
func (e *emitter) close() { e.set(func() { e.closed = true }) }
func (e *emitter) stop()  { e.set(func() { e.stopped = true }) }

func (e *emitter) set(f func()) {
	e.mu.Lock()
	f()
	e.cond.Broadcast()
	e.mu.Unlock()
}

// run sends at most one chunk per tick. It ends once stopped, or once closed
// and flushed; held output that was never started is discarded at stop.
func (e *emitter) run() {
	defer close(e.done)
	for {
		e.mu.Lock()
		var n int
		for {
			if e.stopped {
				e.mu.Unlock()
				return
			}
			if e.started {
				n = e.cut()
				if n > 0 || (e.closed && len(e.buf) == 0) {
					break
				}
			}
			e.cond.Wait()
		}
		if n == 0 { // closed and flushed
			e.mu.Unlock()
			return
		}
		s := string(e.buf[:n])
		e.buf = append(e.buf[:0], e.buf[n:]...)
		e.cond.Broadcast()
		e.mu.Unlock()

		e.emit(s)
		time.Sleep(emitTick)
	}
}

// cut is how many buffered bytes can go out now: at most one chunk, ending
// on a character boundary. A trailing partial character waits for the rest,
// unless the stream has closed and it can never complete.
func (e *emitter) cut() int {
	n := min(len(e.buf), e.chunk)
	if e.closed && n == len(e.buf) {
		return n
	}
	for i := n - 1; i >= 0 && i >= n-utf8.UTFMax; i-- {
		if utf8.RuneStart(e.buf[i]) {
			if !utf8.FullRune(e.buf[i:n]) {
				n = i
			}
			break
		}
	}
	return n
}
