package terminal

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"sync"
	"time"
)

// Zmodem (R43). The protocol runs in zmodem.js on the page; here a tap in
// an SSH terminal's output notices `sz` or `rz` starting, then hands the
// raw bytes to the page (internal/app/zmodem.go streams them) until the
// page ends the transfer, nothing moves for zmodemIdle, or the terminal
// closes. Only SSH terminals are tapped: Mosh is not 8-bit clean, and
// Telnet, serial and local shells are out of scope.

const (
	zmodemIdle   = 2 * time.Minute
	zmodemBufCap = 1 << 20  // raw output held for the page before the transport read blocks
	zmodemFrame  = 64 << 10 // most bytes one Read returns
)

// zmodemCancel is lrzsz's abort: ten CANs stop either end, then ten
// backspaces erase them from a shell that was not listening.
var zmodemCancel = []byte("\x18\x18\x18\x18\x18\x18\x18\x18\x18\x18\b\b\b\b\b\b\b\b\b\b")

// A hex header up to its CRC: ZPAD ZPAD ZDLE 'B', then the type, four flag
// bytes and a CRC-16, each byte as two hex digits.
var zmodemHexPrefix = []byte("**\x18B")

const zmodemHeaderLen = 4 + 2*7

var errZmodemEnded = errors.New("the Zmodem transfer has ended")

// zmodemStart finds the hex header a transfer opens with: ZRQINIT from a
// sender (`sz` on the server, so a download) or ZRINIT from a receiver
// (`rz`, an upload). The CRC must match, so "**" in an editor or a stray
// CAN never starts one. It returns the header's index and direction, or -1.
func zmodemStart(b []byte) (int, string) {
	for off := 0; ; {
		i := bytes.Index(b[off:], zmodemHexPrefix)
		if i < 0 {
			return -1, ""
		}
		i += off
		if len(b)-i < zmodemHeaderLen {
			return -1, "" // cut short; the next write completes it
		}
		var h [7]byte
		if _, err := hex.Decode(h[:], b[i+4:i+zmodemHeaderLen]); err == nil && crc16(h[:5]) == uint16(h[5])<<8|uint16(h[6]) {
			switch h[0] {
			case 0:
				return i, "download"
			case 1:
				return i, "upload"
			}
		}
		off = i + 1
	}
}

// crc16 is the CRC-16/XMODEM Zmodem headers carry.
func crc16(b []byte) uint16 {
	var c uint16
	for _, x := range b {
		c ^= uint16(x) << 8
		for range 8 {
			if c&0x8000 != 0 {
				c = c<<1 ^ 0x1021
			} else {
				c <<= 1
			}
		}
	}
	return c
}

// zmodemTap sits between an SSH transport and its emitter. Until a header
// appears it passes every byte to text unchanged and at once: it never holds
// output back, because a header split across reads is found again from
// recent.
type zmodemTap struct {
	text   io.Writer // the emitter
	remote io.Writer // the transport
	notify func(*Zmodem)
	idle   time.Duration

	mu     sync.Mutex
	cond   *sync.Cond
	recent []byte  // the last zmodemHeaderLen-1 bytes passed to text
	cur    *Zmodem // the transfer in progress, if any
}

func newZmodemTap(text, remote io.Writer, notify func(*Zmodem)) *zmodemTap {
	z := &zmodemTap{text: text, remote: remote, notify: notify, idle: zmodemIdle,
		recent: make([]byte, 0, 2*zmodemHeaderLen)}
	z.cond = sync.NewCond(&z.mu)
	return z
}

// Zmodem is one transfer in a terminal, from its header until it ends.
type Zmodem struct {
	tap *zmodemTap
	// Direction is "download" (`sz` on the server) or "upload" (`rz`).
	Direction string

	buf   []byte // output from the header on, not yet Read
	last  time.Time
	ended bool
	done  chan struct{}
}

func (z *zmodemTap) Write(p []byte) (int, error) {
	z.mu.Lock()
	if x := z.cur; x != nil {
		x.buf = append(x.buf, p...)
		x.last = time.Now()
		z.cond.Broadcast()
		for z.cur == x && len(x.buf) > zmodemBufCap {
			z.cond.Wait()
		}
		z.mu.Unlock()
		return len(p), nil
	}
	n := len(z.recent)
	// recent's spare capacity holds the head of p, so this does not allocate.
	at, dir := zmodemStart(append(z.recent, p[:min(len(p), zmodemHeaderLen-1)]...))
	if at < 0 {
		if at, dir = zmodemStart(p); at >= 0 {
			at += n
		}
	}
	if at < 0 {
		z.remember(p)
		z.mu.Unlock()
		return z.text.Write(p)
	}
	x := &Zmodem{tap: z, Direction: dir, last: time.Now(), done: make(chan struct{})}
	if at < n { // the header began in output already shown; show it, and give the page all of it
		x.buf = append(append(x.buf, z.recent[at:n]...), p...)
	} else {
		x.buf = append(x.buf, p[at-n:]...)
	}
	z.recent = z.recent[:0]
	z.mu.Unlock()
	if at > n {
		if _, err := z.text.Write(p[:at-n]); err != nil {
			return 0, err
		}
	}
	z.mu.Lock()
	z.cur = x
	z.mu.Unlock()
	go x.guard()
	z.notify(x)
	return len(p), nil
}

// remember keeps the last zmodemHeaderLen-1 bytes of text output.
func (z *zmodemTap) remember(p []byte) {
	r := p
	if len(p) < zmodemHeaderLen-1 {
		r = append(z.recent, p...)
	}
	k := min(len(r), zmodemHeaderLen-1)
	z.recent = append(z.recent[:0], r[len(r)-k:]...)
}

// close ends the transfer in progress when the terminal ends.
func (z *zmodemTap) close() {
	z.mu.Lock()
	defer z.mu.Unlock()
	if x := z.cur; x != nil {
		x.finish()
		x.buf = nil
	}
}

func (z *zmodemTap) current() *Zmodem {
	z.mu.Lock()
	defer z.mu.Unlock()
	return z.cur
}

// Read returns the server's next bytes for the page. It returns io.EOF once
// the transfer has ended.
func (x *Zmodem) Read(ctx context.Context) ([]byte, error) {
	z := x.tap
	stop := context.AfterFunc(ctx, func() {
		z.mu.Lock()
		z.cond.Broadcast()
		z.mu.Unlock()
	})
	defer stop()
	z.mu.Lock()
	defer z.mu.Unlock()
	for len(x.buf) == 0 && !x.ended && ctx.Err() == nil {
		z.cond.Wait()
	}
	if len(x.buf) == 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, io.EOF
	}
	k := min(len(x.buf), zmodemFrame)
	b := append([]byte(nil), x.buf[:k]...)
	x.buf = x.buf[k:]
	z.cond.Broadcast()
	return b, nil
}

// Write sends the page's bytes to the server.
func (x *Zmodem) Write(p []byte) error {
	z := x.tap
	z.mu.Lock()
	if x.ended {
		z.mu.Unlock()
		return errZmodemEnded
	}
	x.last = time.Now()
	z.mu.Unlock()
	_, err := z.remote.Write(p)
	return err
}

// End returns the terminal to text once the page's protocol has finished.
// It returns the output the page has not read: shell output after the
// transfer, for the page to show before anything that follows.
func (x *Zmodem) End() []byte {
	z := x.tap
	z.mu.Lock()
	defer z.mu.Unlock()
	if !x.finish() {
		return nil
	}
	rest := x.buf
	x.buf = nil
	return rest
}

// Abort cancels the transfer at the server and returns the terminal to text.
func (x *Zmodem) Abort() {
	z := x.tap
	z.mu.Lock()
	ok := x.markEnded()
	x.buf = nil
	z.mu.Unlock()
	if !ok {
		return
	}
	// The cancel goes out before Done, so nothing sent once the page sees
	// the transfer end can reach the server ahead of the abort.
	z.remote.Write(zmodemCancel)
	z.mu.Lock()
	x.signalEnded()
	z.mu.Unlock()
}

// Done is closed when the transfer ends, however it ends.
func (x *Zmodem) Done() <-chan struct{} { return x.done }

// finish marks the transfer ended; it reports false if it already was.
// The caller holds the tap's lock.
func (x *Zmodem) finish() bool {
	if !x.markEnded() {
		return false
	}
	x.signalEnded()
	return true
}

// markEnded is finish without closing Done. It wakes the tap's waiters at
// once, so server output blocked on a full buffer flows on as text while an
// abort writes. The caller holds the tap's lock.
func (x *Zmodem) markEnded() bool {
	if x.ended {
		return false
	}
	x.ended = true
	if x.tap.cur == x {
		x.tap.cur = nil
	}
	x.tap.cond.Broadcast()
	return true
}

// signalEnded closes Done; the caller holds the tap's lock.
func (x *Zmodem) signalEnded() { close(x.done) }

// guard aborts a transfer once no bytes have moved either way for the idle
// time, so a page that never answers cannot leave the terminal stuck.
func (x *Zmodem) guard() {
	z := x.tap
	for {
		z.mu.Lock()
		wait := time.Until(x.last.Add(z.idle))
		z.mu.Unlock()
		if wait <= 0 {
			x.Abort()
			return
		}
		select {
		case <-x.done:
			return
		case <-time.After(wait):
		}
	}
}

var errNoZmodem = errors.New("no Zmodem transfer is running in this terminal")

// OnZmodem sets what runs when `sz` or `rz` starts in an SSH terminal: id is
// the terminal, direction is Zmodem.Direction. Set it before the first Open.
func (r *Registry) OnZmodem(f func(id, direction string)) {
	r.mu.Lock()
	r.zmodem = f
	r.mu.Unlock()
}

// Zmodem returns the terminal's transfer in progress.
func (r *Registry) Zmodem(id string) (*Zmodem, error) {
	s := r.get(id)
	if s == nil {
		return nil, errUnknown
	}
	if s.zm == nil {
		return nil, errNoZmodem
	}
	if x := s.zm.current(); x != nil {
		return x, nil
	}
	return nil, errNoZmodem
}

// tapZmodem puts a tap in front of text, the way to an SSH terminal's
// emitter.
func (r *Registry) tapZmodem(id string, s *session, text io.Writer) io.Writer {
	if _, ok := s.t.(*sshPty); !ok {
		return text
	}
	s.zm = newZmodemTap(text, s.t, func(x *Zmodem) {
		r.mu.Lock()
		f := r.zmodem
		r.mu.Unlock()
		if f != nil {
			f(id, x.Direction)
		}
	})
	return s.zm
}
