package terminal

// The Telnet client (R18, KTD10) learns from uniterm's
// backend/session/telnet_session.go (Apache-2.0, github.com/ys-ll/uniterm).
// Unlike that one, the parser keeps its state across reads, so a command
// split between two TCP segments is still understood; IAC is escaped on the
// way out; and an option is answered only when its state changes (RFC 1143),
// so two peers cannot loop on each other's replies.

import (
	"errors"
	"io"
	"net"
	"sync"
)

// Telnet commands and options (RFC 854, 857, 858, 1091, 1073).
const (
	se   = 240
	nop  = 241
	sb   = 250
	will = 251
	wont = 252
	do   = 253
	dont = 254
	iac  = 255

	optEcho  = 1
	optSGA   = 3
	optTTYPE = 24
	optNAWS  = 31

	ttypeIs   = 0
	ttypeSend = 1
)

const telnetTermType = "xterm-256color"

// localOK lists the options this side agrees to perform, remoteOK the ones
// it lets the host perform. Every other option is refused.
func localOK(o byte) bool  { return o == optSGA || o == optTTYPE || o == optNAWS }
func remoteOK(o byte) bool { return o == optEcho || o == optSGA }

// Parser states.
const (
	stData = iota
	stIAC
	stOpt   // after WILL/WONT/DO/DONT, waiting for the option
	stSB    // inside a subnegotiation
	stSBIAC // IAC inside a subnegotiation
)

// maxSB bounds a subnegotiation so a hostile host cannot grow it forever.
const maxSB = 512

type telnet struct {
	conn net.Conn

	// Read's own state.
	raw   []byte
	state int
	cmd   byte
	sbuf  []byte
	end   string

	mu         sync.Mutex // one writer at a time, and guards what follows
	local      [256]bool  // options this side has enabled
	remote     [256]bool  // options the host has enabled
	cols, rows int
}

// OpenTelnet runs Telnet on conn, which the caller dialled through the
// host's jump host or proxy. Telnet is unencrypted: anything typed,
// passwords included, crosses the network in the clear.
func OpenTelnet(conn net.Conn, cols, rows int) Transport {
	return newTelnet(conn, cols, rows)
}

func newTelnet(conn net.Conn, cols, rows int) *telnet {
	return &telnet{conn: conn, cols: cols, rows: rows}
}

// Read returns the host's data with every Telnet command removed, answering
// negotiations as it goes. It never returns (0, nil).
func (t *telnet) Read(b []byte) (int, error) {
	for {
		if len(t.raw) < len(b) {
			t.raw = make([]byte, len(b))
		}
		n, err := t.conn.Read(t.raw[:len(b)])
		out := t.parse(t.raw[:n], b[:0]) // never longer than its input
		if err != nil {
			t.end = endReason(err)
		}
		if len(out) > 0 || err != nil {
			return len(out), err
		}
	}
}

func endReason(err error) string {
	switch {
	case errors.Is(err, io.EOF):
		return "connection closed by the host"
	case errors.Is(err, net.ErrClosed):
		return "session closed"
	}
	return "connection lost"
}

func (t *telnet) EndReason() string { return t.end }

func (t *telnet) parse(in, out []byte) []byte {
	for _, c := range in {
		switch t.state {
		case stData:
			if c == iac {
				t.state = stIAC
			} else {
				out = append(out, c)
			}
		case stIAC:
			t.state = stData
			switch c {
			case iac:
				out = append(out, iac)
			case will, wont, do, dont:
				t.cmd, t.state = c, stOpt
			case sb:
				t.sbuf, t.state = t.sbuf[:0], stSB
			} // NOP, GA, AYT and the rest have nothing to show
		case stOpt:
			t.negotiate(t.cmd, c)
			t.state = stData
		case stSB:
			if c == iac {
				t.state = stSBIAC
			} else if len(t.sbuf) < maxSB {
				t.sbuf = append(t.sbuf, c)
			}
		case stSBIAC:
			t.state = stSB
			switch c {
			case se:
				t.subnegotiate(t.sbuf)
				t.state = stData
			case iac:
				if len(t.sbuf) < maxSB {
					t.sbuf = append(t.sbuf, iac)
				}
			}
		}
	}
	return out
}

func (t *telnet) negotiate(cmd, opt byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch cmd {
	case do:
		if !localOK(opt) {
			t.send(iac, wont, opt)
		} else if !t.local[opt] {
			t.local[opt] = true
			t.send(iac, will, opt)
			if opt == optNAWS {
				t.sendNAWS()
			}
		}
	case dont:
		if t.local[opt] {
			t.local[opt] = false
			t.send(iac, wont, opt)
		}
	case will:
		if !remoteOK(opt) {
			t.send(iac, dont, opt)
		} else if !t.remote[opt] {
			t.remote[opt] = true
			t.send(iac, do, opt)
		}
	case wont:
		if t.remote[opt] {
			t.remote[opt] = false
			t.send(iac, dont, opt)
		}
	}
}

func (t *telnet) subnegotiate(b []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(b) == 2 && b[0] == optTTYPE && b[1] == ttypeSend && t.local[optTTYPE] {
		p := append([]byte{iac, sb, optTTYPE, ttypeIs}, telnetTermType...)
		t.send(append(p, iac, se)...)
	}
}

// send writes with t.mu held. A failed write also ends the next Read, so
// the error is left to it.
func (t *telnet) send(b ...byte) error {
	_, err := t.conn.Write(b)
	return err
}

func (t *telnet) sendNAWS() error {
	p := []byte{iac, sb, optNAWS}
	for _, v := range []int{t.cols, t.rows} {
		for _, c := range []byte{byte(v >> 8), byte(v)} {
			p = append(p, c)
			if c == iac {
				p = append(p, iac)
			}
		}
	}
	return t.send(append(p, iac, se)...)
}

// Write sends keystrokes, doubling IAC and turning Enter's bare CR into the
// CR LF that Telnet means by end of line.
func (t *telnet) Write(b []byte) (int, error) {
	p := make([]byte, 0, len(b)+8)
	for i, c := range b {
		p = append(p, c)
		if c == iac {
			p = append(p, iac)
		} else if c == '\r' && (i+1 == len(b) || b[i+1] != '\n') {
			p = append(p, '\n')
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.send(p...); err != nil {
		return 0, err
	}
	return len(b), nil
}

// Resize records the size and sends it once the host has asked for NAWS.
func (t *telnet) Resize(cols, rows int) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cols, t.rows = cols, rows
	if !t.local[optNAWS] {
		return nil
	}
	return t.sendNAWS()
}

func (t *telnet) Close() error { return t.conn.Close() }
