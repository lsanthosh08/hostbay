package dbtest

import (
	"bufio"
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
)

// MySQL is a MySQL server that speaks only the sign-in and a hostile first
// answer: after the handshake response it asks for the cleartext password
// plugin (AskCleartext), or it signs the client in and answers every query
// with a request for the local file InFile. It records every packet the
// client sent after that ask.
type MySQL struct {
	Host string
	Port int

	mu           sync.Mutex
	askCleartext bool
	inFile       string
	after        [][]byte
}

// NewMySQL serves until the test ends.
func NewMySQL(t testing.TB) *MySQL {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s := &MySQL{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s
}

// AskCleartext makes the server switch the sign-in to mysql_clear_password.
func (s *MySQL) AskCleartext() { s.mu.Lock(); s.askCleartext = true; s.mu.Unlock() }

// AskFile makes the server answer queries with LOAD DATA LOCAL INFILE name.
func (s *MySQL) AskFile(name string) { s.mu.Lock(); s.inFile = name; s.mu.Unlock() }

// After is what the client sent after the server's ask, packet by packet.
func (s *MySQL) After() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.after...)
}

// Addr is the server's host:port.
func (s *MySQL) Addr() string { return net.JoinHostPort(s.Host, strconv.Itoa(s.Port)) }

func (s *MySQL) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	// Initial handshake v10, offering mysql_native_password, no TLS.
	hs := []byte{10}
	hs = append(hs, "8.0.0-hostbay-fake"...)
	hs = append(hs, 0, 1, 0, 0, 0)                    // connection ID 1
	hs = append(hs, "abcdefgh"...)                    // scramble, first part
	hs = append(hs, 0)                                // filler
	hs = binary.LittleEndian.AppendUint16(hs, 0xa285) // long password, long flag, local files, 4.1, transactions, secure connection
	hs = append(hs, 0x21, 2, 0)                       // charset, status
	hs = binary.LittleEndian.AppendUint16(hs, 0x002b) // multi statements and results, plugin auth, lenenc data
	hs = append(hs, 21)
	hs = append(hs, make([]byte, 10)...)
	hs = append(hs, "ijklmnopqrst"...)
	hs = append(hs, 0)
	hs = append(hs, "mysql_native_password"...)
	hs = append(hs, 0)
	if writePacket(c, 0, hs) != nil {
		return
	}
	seq, _, err := readPacket(r) // the handshake response
	if err != nil {
		return
	}
	s.mu.Lock()
	clear, file := s.askCleartext, s.inFile
	s.mu.Unlock()
	if clear {
		writePacket(c, seq+1, append([]byte("\xfemysql_clear_password\x00"), "abcdefghijklmnopqrst\x00"...))
		if _, body, err := readPacket(r); err == nil { // a password, if the client sent one
			s.mu.Lock()
			s.after = append(s.after, body)
			s.mu.Unlock()
		}
		return
	}
	if writePacket(c, seq+1, ok) != nil {
		return
	}
	for {
		_, cmd, err := readPacket(r)
		if err != nil || len(cmd) == 0 || cmd[0] == 1 { // COM_QUIT
			return
		}
		if cmd[0] != 3 { // anything but COM_QUERY: OK
			writePacket(c, 1, ok)
			continue
		}
		writePacket(c, 1, append([]byte{0xfb}, file...))
		seq, body, err := readPacket(r)
		if err != nil {
			return
		}
		s.mu.Lock()
		s.after = append(s.after, body)
		s.mu.Unlock()
		writePacket(c, seq+1, append([]byte{0xff, 0x16, 0x05, '#', 'H', 'Y', '0', '0', '0'}, "no file arrived"...))
	}
}

// ok is an OK packet: nothing affected.
var ok = []byte{0, 0, 0, 2, 0, 0, 0}

func writePacket(w io.Writer, seq byte, body []byte) error {
	h := []byte{byte(len(body)), byte(len(body) >> 8), byte(len(body) >> 16), seq}
	_, err := w.Write(append(h, body...))
	return err
}

func readPacket(r io.Reader) (byte, []byte, error) {
	var h [4]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := int(h[0]) | int(h[1])<<8 | int(h[2])<<16
	b := make([]byte, n)
	_, err := io.ReadFull(r, b)
	return h[3], b, err
}
