// Package terminal runs interactive terminal sessions and streams their
// output to the UI through a paced emitter (KTD9). SSH is the first
// transport; Telnet, Mosh, serial and local shells add their own.
package terminal

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"sync"
)

// Transport is one terminal's byte stream. Read returns the remote output
// and ends with an error when the terminal ends; Close ends it from this side.
type Transport interface {
	io.ReadWriteCloser
	Resize(cols, rows int) error
}

// Ender is implemented by a transport that can say why its output ended,
// for example "connection lost" instead of the default "session closed".
type Ender interface {
	EndReason() string
}

// Registry owns the open terminals, keyed by an opaque random ID.
type Registry struct {
	data   func(id, s string)         // a batch of output
	closed func(id, reason string)    // after the last output, however it ended
	zmodem func(id, direction string) // a Zmodem transfer started; see OnZmodem

	mu sync.Mutex
	m  map[string]*session
}

type session struct {
	owner string
	t     Transport
	em    *emitter
	zm    *zmodemTap // SSH terminals only
	tap   *tap
	once  sync.Once
}

func NewRegistry(data func(id, s string), closed func(id, reason string)) *Registry {
	return &Registry{data: data, closed: closed, m: map[string]*session{}}
}

var errUnknown = errors.New("terminal is closed")

// Open starts streaming t and returns its ID. Output is held until Attach.
// owner groups terminals for CloseOwner (the SSH session ID).
func (r *Registry) Open(owner string, t Transport) string { return r.OpenWith(owner, t, nil) }

// OpenWith is Open, calling ready with the ID before any output is read,
// so a subscriber it adds sees the terminal's first output.
func (r *Registry) OpenWith(owner string, t Transport, ready func(id string)) string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	id := hex.EncodeToString(b[:])
	s := &session{owner: owner, t: t, em: newEmitter(func(d string) { r.data(id, d) }), tap: newTap()}
	// The output tap sits after the Zmodem tap and before the emitter. A
	// serial or raw TCP terminal feeds it device bytes itself, before its
	// local echo joins them, so a typed password never reaches it (KTD38).
	var text io.Writer = teeTap{s.tap, s.em}
	if st, ok := t.(*streamTerm); ok {
		st.tap.Store(s.tap)
		text = s.em
	}
	out := r.tapZmodem(id, s, text)
	r.mu.Lock()
	r.m[id] = s
	r.mu.Unlock()
	if ready != nil {
		ready(id)
	}

	go func() {
		io.Copy(out, t)
		s.tap.close()
		if s.zm != nil {
			s.zm.close()
		}
		// Stay registered until the output is delivered, so a terminal that
		// ends before the UI attaches still shows its last words.
		s.em.close()
		<-s.em.done
		r.end(id, s)
		reason := "session closed"
		if e, ok := t.(Ender); ok {
			reason = e.EndReason()
		}
		r.closed(id, reason)
	}()
	return id
}

// Attach starts delivering output once the UI is listening, so a login
// banner sent before it subscribed is not lost.
func (r *Registry) Attach(id string) error {
	s := r.get(id)
	if s == nil {
		return errUnknown
	}
	s.em.start()
	return nil
}

// Recent returns the terminal's last output, up to 256 KiB, without a
// split character (KTD38).
func (r *Registry) Recent(id string) ([]byte, error) {
	s := r.get(id)
	if s == nil {
		return nil, errUnknown
	}
	return s.tap.recent(), nil
}

// Subscribe copies the terminal's output from now on to a bounded queue.
// A subscriber that falls behind loses chunks, counted in Gaps, and never
// slows the terminal.
func (r *Registry) Subscribe(id string) (*Subscription, error) {
	s := r.get(id)
	if s == nil {
		return nil, errUnknown
	}
	if sub := s.tap.subscribe(); sub != nil {
		return sub, nil
	}
	return nil, errUnknown
}

// LocalEcho reports a terminal that shows typed text itself, a serial or
// raw TCP one with local echo on: what is on screen there is not what the
// device echoed.
func (r *Registry) LocalEcho(id string) bool {
	s := r.get(id)
	if s == nil {
		return false
	}
	st, ok := s.t.(*streamTerm)
	return ok && st.echo
}

func (r *Registry) Write(id string, p []byte) error {
	s := r.get(id)
	if s == nil {
		return errUnknown
	}
	_, err := s.t.Write(p)
	return err
}

func (r *Registry) Resize(id string, cols, rows int) error {
	s := r.get(id)
	if s == nil {
		return errUnknown
	}
	return s.t.Resize(cols, rows)
}

// Close ends a terminal and discards output not yet shown. Closing an
// unknown or already ended terminal does nothing.
func (r *Registry) Close(id string) {
	if s := r.get(id); s != nil {
		s.em.stop()
		r.end(id, s)
	}
}

// CloseOwner closes every terminal opened under owner.
func (r *Registry) CloseOwner(owner string) {
	r.mu.Lock()
	var ids []string
	for id, s := range r.m {
		if s.owner == owner {
			ids = append(ids, id)
		}
	}
	r.mu.Unlock()
	for _, id := range ids {
		r.Close(id)
	}
}

func (r *Registry) get(id string) *session {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.m[id]
}

// end forgets the terminal and closes its transport, once.
func (r *Registry) end(id string, s *session) {
	s.once.Do(func() {
		r.mu.Lock()
		delete(r.m, id)
		r.mu.Unlock()
		s.tap.close()
		s.t.Close()
	})
}
