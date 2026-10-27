// Package rdp runs RDP sessions through the bundled Rust helper (KTD3): one
// helper process per session, driven over its stdin and stdout.
//
// The protocol is newline-delimited JSON tagged by "t" (rdp-helper/src/
// protocol.rs). The helper opens with hello and its version. Go sends
// connect, which carries the password: stdin is the only place it goes. The
// helper sends cert after the TLS upgrade and waits; Go answers trust, and
// nothing reaches CredSSP before that. Then size, frame, clipboardText,
// clipboardFiles and error flow out, and input, clipboardText,
// clipboardFiles and close flow in. A frame line is followed by its raw
// RGBA bytes.
//
// Frames, sizes and the end of the session reach the page over a Wails
// GoStream instead of the loopback WebSocket KTD3 first described: there is
// no listening port, so the token, Origin, Host and single-upgrade checks
// that a WebSocket needs have nothing to guard (docs/spikes/
// 2026-10-22-wails-streaming-spike.md).
package rdp

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"hostbay/internal/apperr"
)

// ProtocolVersion must equal rdp-helper's protocol::VERSION.
const ProtocolVersion = 1

// Messages to the page, one per stream frame. The first byte is the kind;
// numbers are little-endian uint16.
const (
	MsgFrame  = 1 // x, y, w, h, then w*h*4 RGBA bytes
	MsgSize   = 2 // w, h: the desktop size, at connect and on any change
	MsgClosed = 3 // then a UTF-8 reason; empty when the user closed it
)

var (
	// handshakeTimeout bounds the wait for the helper's hello.
	handshakeTimeout = 10 * time.Second
	// connectTimeout bounds TCP, TLS and CredSSP together.
	connectTimeout = 60 * time.Second
	// exitGrace is how long a helper may take to exit once its stdin closes.
	exitGrace = 3 * time.Second
)

const (
	maxDesktop   = 8192       // RDP's largest desktop side
	maxFrameLen  = 64<<20 - 9 // Wails' largest stream frame, less our header
	maxClipText  = 8 << 20
	maxClipFiles = 1000
	heldMessages = 256       // held for the page before the helper is made to wait
	heldBytes    = 128 << 20 // likewise; frames can be ~9 MB, so the count alone is not a bound
)

// HelperName is the helper's file name beside the Hostbay executable.
func HelperName() string {
	if runtime.GOOS == "windows" {
		return "hostbay-rdp-helper.exe"
	}
	return "hostbay-rdp-helper"
}

// HelperPath finds the helper next to the running executable, which is also
// where `task build:rdp-helper` puts it for development. It never searches
// PATH or the working directory, and nothing the user sets can move it.
func HelperPath() (string, error) {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return "", apperr.New(apperr.HelperMissing, "Cannot locate Hostbay's RDP helper: "+err.Error())
	}
	return filepath.Join(filepath.Dir(exe), HelperName()), nil
}

// Connect is what a session needs. DialHost and DialPort are where the helper
// opens TCP (a local forward for a host behind a jump chain); TargetHost is
// the real server, for TLS SNI and CredSSP.
type Connect struct {
	DialHost   string
	DialPort   int
	TargetHost string
	User       string
	Password   string
	Width      int
	Height     int
	Security   string // auto | nla | tls
}

// Clipboard is the Hostbay window's clipboard (Wails' Clipboard).
type Clipboard interface {
	Text() (string, bool)
	SetText(text string) bool
}

// Hooks are how a session reports back. All are optional except CheckCert.
type Hooks struct {
	// CheckCert decides on the server's certificate (DER) before any
	// credential is sent; a non-nil error ends the connect with that error.
	CheckCert func(der []byte) error
	// ClipboardText and ClipboardFiles report what the remote copied.
	ClipboardText  func(text string)
	ClipboardFiles func(paths []string)
	// Exited runs once the helper has exited and been reaped, after the
	// connect succeeded. reason is empty when Close ended the session.
	Exited func(reason string)
	// Clipboard is the window clipboard, used on Linux only, where the
	// helper has no window to own the clipboard with (KTD3).
	Clipboard Clipboard
}

// msg is any message from the helper.
type msg struct {
	T       string   `json:"t"`
	Version int      `json:"version"`
	Der     string   `json:"der"`
	Width   int      `json:"width"`
	Height  int      `json:"height"`
	X       int      `json:"x"`
	Y       int      `json:"y"`
	W       int      `json:"w"`
	H       int      `json:"h"`
	Len     int      `json:"len"`
	Text    string   `json:"text"`
	Paths   []string `json:"paths"`
	Message string   `json:"message"`
}

// Session is one running helper.
type Session struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	hooks Hooks

	wmu sync.Mutex // one message per write

	size     atomic.Uint32 // width<<16 | height, for input checks
	out      chan []byte   // for the page; held until Attach
	queued   atomic.Int64  // bytes in out
	room     chan struct{} // signalled when Attach takes a message from out
	attached atomic.Bool
	lastClip atomic.Value // Linux: the clipboard text last seen or set

	closeOnce sync.Once
	closing   chan struct{} // Close was called: stop holding messages
	done      chan struct{} // the helper exited and was reaped
}

// Start launches the helper at path (absolute), checks its version, sends
// connect and runs the certificate check. It returns once the session is up.
// On any failure the helper is killed and reaped before Start returns.
func Start(ctx context.Context, path string, c Connect, h Hooks) (*Session, error) {
	if !filepath.IsAbs(path) {
		return nil, apperr.New(apperr.HelperMissing, "The RDP helper path is not absolute: "+path)
	}
	if _, err := os.Stat(path); err != nil {
		return nil, apperr.New(apperr.HelperMissing, "Hostbay's RDP helper is missing ("+path+"). Reinstall Hostbay.")
	}
	cmd := exec.Command(path) // no arguments: the password goes over stdin only
	cmd.Stderr = os.Stderr    // diagnostics; the helper never prints the password
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, apperr.New(apperr.HelperMissing, "Cannot start Hostbay's RDP helper: "+err.Error())
	}
	s := &Session{
		cmd: cmd, stdin: stdin, hooks: h,
		out: make(chan []byte, heldMessages), room: make(chan struct{}, 1), closing: make(chan struct{}), done: make(chan struct{}),
	}
	r := bufio.NewReaderSize(stdout, 1<<16)
	if err := s.handshake(ctx, r, c); err != nil {
		cmd.Process.Kill()
		stdin.Close()
		cmd.Wait()
		return nil, err
	}
	go s.readLoop(r)
	return s, nil
}

func (s *Session) handshake(ctx context.Context, r *bufio.Reader, c Connect) error {
	// Killing the helper unblocks the reads below on a timeout or cancel.
	kill := func() { s.cmd.Process.Kill() }
	t := time.AfterFunc(handshakeTimeout, kill)
	m, err := readMsg(r)
	t.Stop()
	if err != nil {
		return apperr.New(apperr.HelperMissing, "Hostbay's RDP helper did not start: "+err.Error())
	}
	if m.T != "hello" || m.Version != ProtocolVersion {
		return apperr.New(apperr.HelperMissing, fmt.Sprintf(
			"Hostbay's RDP helper speaks protocol version %d, but this Hostbay needs %d. Reinstall Hostbay.", m.Version, ProtocolVersion))
	}

	t = time.AfterFunc(connectTimeout, kill)
	defer t.Stop()
	defer context.AfterFunc(ctx, kill)()
	if err := s.send(map[string]any{
		"t": "connect", "dialHost": c.DialHost, "dialPort": c.DialPort, "targetHost": c.TargetHost,
		"user": c.User, "password": c.Password, "width": c.Width, "height": c.Height, "security": c.Security,
	}); err != nil {
		return err
	}
	for {
		m, err := readMsg(r)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("the RDP helper stopped while connecting: %w", err)
		}
		switch m.T {
		case "cert":
			der, err := base64.StdEncoding.DecodeString(m.Der)
			if err == nil {
				err = s.hooks.CheckCert(der)
			}
			if aerr := s.send(map[string]any{"t": "trust", "accept": err == nil}); err == nil {
				err = aerr
			}
			if err != nil {
				return err
			}
		case "error":
			return errors.New("RDP: " + m.Message)
		case "size":
			if !s.setSize(m.Width, m.Height) {
				return errors.New("the RDP helper reported an impossible desktop size")
			}
			s.out <- sizeMsg(m.Width, m.Height) // the channel is empty here
			return nil
		}
	}
}

// readMsg reads one line, which must be a JSON message.
func readMsg(r *bufio.Reader) (msg, error) {
	var m msg
	line, err := r.ReadBytes('\n')
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return m, err
	}
	err = json.Unmarshal(line, &m)
	return m, err
}

func (s *Session) setSize(w, h int) bool {
	if w < 1 || h < 1 || w > maxDesktop || h > maxDesktop {
		return false
	}
	s.size.Store(uint32(w)<<16 | uint32(h))
	return true
}

func (s *Session) desktop() (int, int) {
	v := s.size.Load()
	return int(v >> 16), int(v & 0xffff)
}

func sizeMsg(w, h int) []byte {
	b := []byte{MsgSize, 0, 0, 0, 0}
	binary.LittleEndian.PutUint16(b[1:], uint16(w))
	binary.LittleEndian.PutUint16(b[3:], uint16(h))
	return b
}

// readLoop turns helper output into page messages and hook calls until the
// helper exits, then reaps it and reports why it ended.
func (s *Session) readLoop(r *bufio.Reader) {
	stop := s.watchClipboard()
	reason := s.pump(r)
	stop()
	// Keep draining so a helper blocked on a write can still exit.
	go io.Copy(io.Discard, r)
	s.stdin.Close()
	t := time.AfterFunc(exitGrace, func() { s.cmd.Process.Kill() })
	err := s.cmd.Wait()
	t.Stop()
	select {
	case <-s.closing:
		reason = "" // the user closed it; whatever followed is not news
	default:
		if reason == "" && err != nil {
			reason = "The RDP helper stopped unexpectedly (" + err.Error() + ")."
		} else if reason == "" {
			reason = "The remote desktop session ended."
		}
	}
	s.hold(append([]byte{MsgClosed}, reason...))
	close(s.done) // before out, so a viewer that sees out end also sees done
	close(s.out)
	if s.hooks.Exited != nil {
		s.hooks.Exited(reason)
	}
}

// pump handles messages until EOF or a protocol error; it returns the reason
// to report, or "" for a plain exit.
func (s *Session) pump(r *bufio.Reader) string {
	for {
		m, err := readMsg(r)
		if err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, os.ErrClosed) {
				return ""
			}
			s.cmd.Process.Kill()
			return "The RDP helper sent a malformed message."
		}
		switch m.T {
		case "frame":
			w, h := s.desktop()
			if m.W < 1 || m.H < 1 || m.X < 0 || m.Y < 0 || m.X+m.W > w || m.Y+m.H > h ||
				m.Len != m.W*m.H*4 || m.Len > maxFrameLen {
				s.cmd.Process.Kill()
				return "The RDP helper sent a malformed frame."
			}
			b := make([]byte, 9+m.Len)
			b[0] = MsgFrame
			for i, v := range []int{m.X, m.Y, m.W, m.H} {
				binary.LittleEndian.PutUint16(b[1+2*i:], uint16(v))
			}
			if _, err := io.ReadFull(r, b[9:]); err != nil {
				return ""
			}
			s.hold(b)
		case "size":
			if !s.setSize(m.Width, m.Height) {
				s.cmd.Process.Kill()
				return "The RDP helper reported an impossible desktop size."
			}
			s.hold(sizeMsg(m.Width, m.Height))
		case "clipboardText":
			s.remoteCopied(m.Text)
			if s.hooks.ClipboardText != nil {
				s.hooks.ClipboardText(m.Text)
			}
		case "clipboardFiles":
			if s.hooks.ClipboardFiles != nil {
				s.hooks.ClipboardFiles(m.Paths)
			}
		case "error":
			return "RDP: " + m.Message
		}
	}
}

// hold queues a message for the page. It waits while the queue is full, so
// a page that has not subscribed yet slows the helper instead of losing
// frames, and drops messages once the session is closing.
func (s *Session) hold(b []byte) {
	// Only readLoop calls hold, so queued can only fall while this waits.
	// A frame is under heldBytes, so an empty queue always takes it.
	for s.queued.Load()+int64(len(b)) > heldBytes {
		select {
		case <-s.room:
		case <-s.closing:
			return
		}
	}
	s.queued.Add(int64(len(b)))
	select {
	case s.out <- b:
	case <-s.closing:
	}
}

// Attach streams the session's messages to the page, starting with the ones
// held since connect, until the session ends, send fails or ctx is done.
// Only one subscriber is allowed.
func (s *Session) Attach(ctx context.Context, send func([]byte) error) error {
	if s.attached.Swap(true) {
		return errors.New("this RDP session already has a viewer")
	}
	for {
		select {
		case b, ok := <-s.out:
			if !ok {
				return nil
			}
			s.queued.Add(-int64(len(b)))
			select {
			case s.room <- struct{}{}:
			default:
			}
			if err := send(b); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Done is closed once the helper has exited and been reaped.
func (s *Session) Done() <-chan struct{} { return s.done }

// Close asks the helper to end the session; closing its stdin ends it even
// if it is stuck, and readLoop kills it after a grace period.
func (s *Session) Close() {
	s.closeOnce.Do(func() {
		close(s.closing)
		s.send(map[string]any{"t": "close"})
		s.stdin.Close()
	})
}

func (s *Session) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	select {
	case <-s.done:
		return errors.New("the RDP session has ended")
	default:
	}
	if _, err := s.stdin.Write(append(b, '\n')); err != nil {
		return errors.New("the RDP session has ended")
	}
	return nil
}

// Input forwards pointer and keyboard input after checking it: a known kind,
// a point on the desktop, a browser mouse button, a set-1 scancode with at
// most an E0/E1 prefix, and a wheel delta that fits RDP's int16.
func (s *Session) Input(kind string, x, y, button int, down bool, scancode, delta int) error {
	w, h := s.desktop()
	switch kind {
	case "move", "button", "wheel":
		if x < 0 || y < 0 || x >= w || y >= h {
			return fmt.Errorf("pointer position %d,%d is outside the %dx%d desktop", x, y, w, h)
		}
	case "key":
		if p := scancode >> 8; scancode&0xff == 0 || (p != 0 && p != 0xe0 && p != 0xe1) {
			return fmt.Errorf("invalid scancode %#x", scancode)
		}
	default:
		return fmt.Errorf("unknown input kind %q", kind)
	}
	if button < 0 || button > 2 {
		return fmt.Errorf("invalid mouse button %d", button)
	}
	if delta < -32768 || delta > 32767 {
		return fmt.Errorf("invalid wheel delta %d", delta)
	}
	return s.send(map[string]any{
		"t": "input", "kind": kind, "x": x, "y": y, "button": button, "down": down, "scancode": scancode, "delta": delta,
	})
}

// SetClipboardText offers local text to the remote.
func (s *Session) SetClipboardText(text string) error {
	if !utf8.ValidString(text) || len(text) > maxClipText {
		return errors.New("clipboard text must be UTF-8 and at most 8 MB")
	}
	return s.send(map[string]any{"t": "clipboardText", "text": text})
}

// SetClipboardFiles offers local files to the remote. Each path must be
// absolute, clean and exist.
func (s *Session) SetClipboardFiles(paths []string) error {
	if len(paths) == 0 || len(paths) > maxClipFiles {
		return fmt.Errorf("copy between 1 and %d files", maxClipFiles)
	}
	for _, p := range paths {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || strings.ContainsRune(p, 0) {
			return fmt.Errorf("%q is not an absolute, clean path", p)
		}
		if _, err := os.Lstat(p); err != nil {
			return err
		}
	}
	return s.send(map[string]any{"t": "clipboardFiles", "paths": paths})
}
