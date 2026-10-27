package terminal

// Mosh (R19, KTD10): mosh-server is started over the host's verified SSH
// connection, then the terminal runs over UDP with mosh-go. The bootstrap
// follows uniterm's backend/session/mosh_session.go (Apache-2.0,
// github.com/ys-ll/uniterm), but uses the lease's host-key-checked
// connection instead of a second, unchecked one.
//
// mosh-go v0.5.2's Client is not used as is, for two reasons:
//   - Dial resolves nothing (net.ParseIP of a host name dials localhost) and
//     its connected UDP socket stops receiving for good after one ICMP error,
//     so here an unconnected socket talks to the IP SSH already reached.
//   - Its reader appends every state diff to the output, but mosh diffs are
//     relative to a base state; one arriving after a newer state, or from a
//     base the screen has moved past, garbles the screen. moshScreen applies
//     each diff to its own base, as mosh-go's cmd/mosh-wasm does.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	mosh "github.com/unixshells/mosh-go"
	vt "github.com/unixshells/vt-go"
	"golang.org/x/crypto/ssh"

	"hostbay/internal/sshx"
)

// moshServerCmd runs mosh-server bound to the address SSH reached (-s), with
// 256 colours. It prints "MOSH CONNECT <port> <key>" and detaches. Closing a
// tab sends mosh-server nothing, so it exits after a day without packets
// rather than keeping the shell running forever.
const moshServerCmd = "MOSH_SERVER_NETWORK_TMOUT=86400 mosh-server new -s -c 256"

// moshReplyTimeout is how long the server has to answer over UDP before the
// session is given up as blocked.
var moshReplyTimeout = 10 * time.Second

// OpenMosh starts mosh-server over l's connection and connects to it over
// UDP at the address that connection reached. It needs a direct connection:
// mosh's UDP cannot pass through SSH jump hosts, which the caller checks.
// The session key never leaves this function except into the cipher.
func OpenMosh(ctx context.Context, l *sshx.Lease, cols, rows int) (Transport, error) {
	cl, err := l.Reconnect(ctx)
	if err != nil {
		return nil, err
	}
	port, key, err := startMoshServer(ctx, cl.Client)
	if err != nil {
		return nil, err
	}
	host, _, _ := net.SplitHostPort(cl.RemoteAddr().String())
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, errors.New("mosh needs a direct connection to the host")
	}
	return dialMosh(ctx, &net.UDPAddr{IP: ip, Port: port}, key, cols, rows)
}

// startMoshServer runs mosh-server and returns its UDP port and raw key.
func startMoshServer(ctx context.Context, c *ssh.Client) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	sess, err := c.NewSession()
	if err != nil {
		return 0, nil, err
	}
	defer sess.Close()
	var out capBuf
	sess.Stdout, sess.Stderr = &out, &out
	stop := context.AfterFunc(ctx, func() { sess.Close() })
	err = sess.Run(moshServerCmd)
	stop()
	text := out.String()
	if port, key, ok := parseMoshConnect(text); ok {
		return port, key, nil
	}
	var exit *ssh.ExitError
	switch {
	case errors.As(err, &exit) && exit.ExitStatus() == 127, strings.Contains(text, "mosh-server: not found"),
		strings.Contains(text, "mosh-server: command not found"):
		return 0, nil, errors.New("mosh-server is not installed on the host. Install the mosh package " +
			"there (for example: sudo apt install mosh), or open an SSH terminal instead")
	case ctx.Err() != nil:
		return 0, nil, errors.New("mosh-server did not start in time")
	}
	return 0, nil, fmt.Errorf("mosh-server did not start: %s", lastLine(text))
}

// capBuf collects a command's combined output, up to 64 KB. The SSH session
// writes stdout and stderr from two goroutines.
type capBuf struct {
	mu sync.Mutex
	b  []byte
}

func (c *capBuf) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.b) < 64<<10 {
		c.b = append(c.b, p...)
	}
	return len(p), nil
}

func (c *capBuf) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.b)
}

// lastLine is the last meaningful line of mosh-server's output, for an
// error message. Lines that could carry the key are skipped.
func lastLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l != "" && !strings.Contains(l, "MOSH") {
			return l
		}
	}
	return "no output"
}

// parseMoshConnect finds "MOSH CONNECT <port> <key>" and returns the port and
// the decoded 16-byte key.
func parseMoshConnect(s string) (int, []byte, bool) {
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) != 4 || f[0] != "MOSH" || f[1] != "CONNECT" {
			continue
		}
		port, err := strconv.Atoi(f[2])
		key, kerr := base64.RawStdEncoding.DecodeString(strings.TrimRight(f[3], "="))
		if err == nil && kerr == nil && port > 0 && port < 65536 && len(key) == 16 {
			return port, key, true
		}
	}
	return 0, nil, false
}

// dialMosh connects to a mosh server and waits for its first reply.
func dialMosh(ctx context.Context, server *net.UDPAddr, key []byte, cols, rows int) (Transport, error) {
	ocb, err := mosh.NewOCB(key)
	if err != nil {
		return nil, err
	}
	uc, err := net.ListenUDP("udp", nil)
	if err != nil {
		return nil, err
	}
	c, err := mosh.DialConnRaw(&udpConn{UDPConn: uc, server: server}, ocb)
	if err != nil {
		uc.Close()
		return nil, err
	}
	c.Resize(uint16(cols), uint16(rows))
	pr, pw := io.Pipe()
	m := &moshTerm{c: c, out: pr, pw: pw, done: make(chan struct{}), scr: newMoshScreen(cols, rows)}
	replied := make(chan struct{})
	go m.send()
	go m.recv(replied)

	select {
	case <-replied:
		return m, nil
	case <-ctx.Done():
		m.Close()
		return nil, ctx.Err()
	case <-time.After(moshReplyTimeout):
		m.Close()
		return nil, fmt.Errorf("no reply from mosh-server at %s over UDP; a firewall may be blocking "+
			"UDP port %d (mosh uses 60000-61000)", server.IP, server.Port)
	}
}

// udpConn is an unconnected socket that only talks to the server. Each
// datagram leaves from whatever address the route has now, so the session
// survives a network change, and an ICMP error cannot end the reader.
type udpConn struct {
	*net.UDPConn
	server *net.UDPAddr
}

func (u *udpConn) Read(b []byte) (int, error) {
	for {
		n, from, err := u.ReadFromUDP(b)
		if err == nil && !(from.IP.Equal(u.server.IP) && from.Port == u.server.Port) {
			continue
		}
		var ne net.Error
		if err != nil && !errors.Is(err, net.ErrClosed) && !(errors.As(err, &ne) && ne.Timeout()) {
			time.Sleep(10 * time.Millisecond) // a transient socket error; the deadline still ends the wait
			continue
		}
		return n, err
	}
}

func (u *udpConn) Write(b []byte) (int, error) { return u.WriteToUDP(b, u.server) }

type moshTerm struct {
	c    *mosh.Client
	out  *io.PipeReader
	pw   *io.PipeWriter
	done chan struct{}
	once sync.Once

	mu  sync.Mutex // guards scr between the reader and Resize
	scr *moshScreen
}

// send flushes keystrokes and acknowledgements at mosh's own pace (8 ms).
func (m *moshTerm) send() {
	t := time.NewTicker(8 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-m.done:
			return
		case <-t.C:
			m.c.Tick()
		}
	}
}

// recv turns server states into screen output. replied is closed on the
// first authenticated datagram.
func (m *moshTerm) recv(replied chan struct{}) {
	tr := m.c.Transport()
	start := tr.LastRecv()
	var seen uint64
	for {
		select {
		case <-m.done:
			return
		default:
		}
		diff := m.c.RecvRaw(time.Second)
		if replied != nil && tr.LastRecv().After(start) {
			close(replied)
			replied = nil
		}
		// RecvRaw returns nil both for an empty state and for no state, so a
		// new state shows as a new number.
		n := tr.LastRecvNewNum()
		if n == seen {
			continue
		}
		seen = n
		if n == math.MaxUint64 { // the shell exited and mosh-server is shutting down
			m.c.Tick() // acknowledge it
			m.pw.Close()
			return
		}
		m.mu.Lock()
		b := m.scr.apply(diff, tr.LastRecvOldNum(), n, tr.ThrowawayNum())
		m.mu.Unlock()
		if len(b) > 0 {
			if _, err := m.pw.Write(b); err != nil {
				return
			}
		}
	}
}

func (m *moshTerm) Read(b []byte) (int, error) { return m.out.Read(b) }

func (m *moshTerm) Write(b []byte) (int, error) {
	select {
	case <-m.done:
		return 0, errStopped
	default:
	}
	m.c.Send(b)
	return len(b), nil
}

func (m *moshTerm) Resize(cols, rows int) error {
	m.mu.Lock()
	m.scr.reset(cols, rows)
	m.mu.Unlock()
	m.c.Resize(uint16(cols), uint16(rows))
	return nil
}

func (m *moshTerm) Close() error {
	m.once.Do(func() {
		close(m.done)
		m.c.Close()
		m.pw.Close()
	})
	return nil
}

// moshScreen applies each state diff to the state it was computed from and
// outputs what changed on screen since the last output. Adapted from
// mosh-go's cmd/mosh-wasm/state.go (MIT, github.com/unixshells/mosh-go).
const maxMoshStates = 64

type moshScreen struct {
	cols, rows  int
	shadow      *vt.Emulator
	shadowState uint64
	states      map[uint64]*mosh.Framebuffer
	latest      uint64
	shown       *mosh.Framebuffer
}

func newMoshScreen(cols, rows int) *moshScreen {
	s := &moshScreen{}
	s.reset(cols, rows)
	return s
}

// reset starts over at a new size; the next output redraws the screen.
func (s *moshScreen) reset(cols, rows int) {
	s.cols, s.rows = cols, rows
	s.shadow = newShadow(cols, rows)
	s.shadowState = 0
	s.states = map[uint64]*mosh.Framebuffer{}
	s.shown = nil
}

// newShadow is an emulator whose replies to terminal queries are dropped:
// nobody reads them, and an unread reply would block its Write.
func newShadow(cols, rows int) *vt.Emulator {
	e := vt.NewEmulator(cols, rows)
	e.InputPipe().(io.Closer).Close()
	return e
}

func (s *moshScreen) apply(diff []byte, oldNum, newNum, throwaway uint64) []byte {
	if _, ok := s.states[newNum]; ok {
		return nil
	}
	if oldNum == s.shadowState {
		if _, ok := s.states[oldNum]; !ok {
			s.states[oldNum] = mosh.SnapshotEmulator(s.shadow, true)
		}
	}
	base, ok := s.states[oldNum]
	if !ok {
		// No base, as after a resize: the server's diff starts from blank.
		s.shadow = newShadow(s.cols, s.rows)
		s.states[oldNum] = mosh.SnapshotEmulator(s.shadow, true)
	} else if s.shadowState != oldNum {
		s.shadow = newShadow(s.cols, s.rows)
		s.shadow.Write(base.Diff(nil))
	}
	if instrs, err := mosh.UnmarshalHostMessage(diff); err == nil {
		for _, hi := range instrs {
			s.shadow.Write(hi.Hoststring)
		}
	}
	s.shadowState = newNum
	s.states[newNum] = mosh.SnapshotEmulator(s.shadow, true)
	if newNum > s.latest {
		s.latest = newNum
	}
	for n := range s.states {
		// mosh-go's own server never sends a throwaway number, so old states
		// are also dropped once far behind.
		if n < throwaway || n+maxMoshStates < s.latest {
			delete(s.states, n)
		}
	}
	latest := s.states[s.latest]
	if latest == nil {
		return nil
	}
	out := latest.Diff(s.shown)
	s.shown = latest
	return out
}
