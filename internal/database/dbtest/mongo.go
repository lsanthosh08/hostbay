package dbtest

import (
	"bufio"
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Mongo is a MongoDB server that speaks just enough of the wire protocol
// for a client to connect: it answers hello as the primary of replica set
// "rs0" whose other member, Other, is never served, refuses every
// sign-in, and lists one database. It records each command's name and the
// first byte each connection sent.
type Mongo struct {
	Host  string
	Port  int
	Other string // a member hello lists that a direct client must not dial

	tls *tls.Config

	mu    sync.Mutex
	cmds  []string
	first []byte
}

// NewMongo serves until the test ends; with a certificate it speaks TLS.
func NewMongo(t testing.TB, cert *tls.Certificate) *Mongo {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s := &Mongo{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, Other: "10.255.255.1:27017"}
	if cert != nil {
		s.tls = &tls.Config{Certificates: []tls.Certificate{*cert}}
	}
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

func (s *Mongo) Addr() string { return net.JoinHostPort(s.Host, strconv.Itoa(s.Port)) }

// Commands are the names of the commands received.
func (s *Mongo) Commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.cmds...)
}

// FirstBytes holds each connection's first byte, in order.
func (s *Mongo) FirstBytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.first...)
}

func (s *Mongo) serve(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	b, err := br.Peek(1)
	if err != nil {
		return
	}
	s.mu.Lock()
	s.first = append(s.first, b[0])
	s.mu.Unlock()
	var rw io.ReadWriter = struct {
		io.Reader
		io.Writer
	}{br, c}
	if s.tls != nil {
		tc := tls.Server(&peeked{Conn: c, r: br}, s.tls)
		if tc.Handshake() != nil {
			return
		}
		rw = tc
	}
	for {
		var h [16]byte
		if _, err := io.ReadFull(rw, h[:]); err != nil {
			return
		}
		n := int(binary.LittleEndian.Uint32(h[0:]))
		reqID := binary.LittleEndian.Uint32(h[4:])
		op := binary.LittleEndian.Uint32(h[12:])
		body := make([]byte, n-16)
		if _, err := io.ReadFull(rw, body); err != nil {
			return
		}
		var cmd bson.Raw
		switch op {
		case 2004: // OP_QUERY: flags, collection name, skip, return, query
			i := 4
			for body[i] != 0 {
				i++
			}
			cmd = bson.Raw(body[i+9:])
		case 2013: // OP_MSG: flags, then a kind 0 section
			cmd = bson.Raw(body[5:])
		default:
			return
		}
		el, err := cmd.IndexErr(0)
		if err != nil {
			return
		}
		name := el.Key()
		s.mu.Lock()
		s.cmds = append(s.cmds, name)
		s.mu.Unlock()
		reply, _ := bson.Marshal(s.reply(name))
		var out []byte
		if op == 2004 { // OP_REPLY: flags, cursor ID, starting from, count
			out = binary.LittleEndian.AppendUint32(out, 0)
			out = binary.LittleEndian.AppendUint64(out, 0)
			out = binary.LittleEndian.AppendUint32(out, 0)
			out = binary.LittleEndian.AppendUint32(out, 1)
		} else {
			out = binary.LittleEndian.AppendUint32(out, 0)
			out = append(out, 0)
		}
		out = append(out, reply...)
		msg := binary.LittleEndian.AppendUint32(nil, uint32(16+len(out)))
		msg = binary.LittleEndian.AppendUint32(msg, reqID+1)
		msg = binary.LittleEndian.AppendUint32(msg, reqID)
		replyOp := uint32(2013)
		if op == 2004 {
			replyOp = 1
		}
		msg = binary.LittleEndian.AppendUint32(msg, replyOp)
		if _, err := rw.Write(append(msg, out...)); err != nil {
			return
		}
	}
}

func (s *Mongo) reply(name string) bson.D {
	switch name {
	case "hello", "isMaster", "ismaster":
		return bson.D{{Key: "ok", Value: 1.0}, {Key: "isWritablePrimary", Value: true}, {Key: "ismaster", Value: true},
			{Key: "setName", Value: "rs0"}, {Key: "hosts", Value: bson.A{s.Addr(), s.Other}}, {Key: "primary", Value: s.Addr()}, {Key: "me", Value: s.Addr()},
			{Key: "minWireVersion", Value: int32(0)}, {Key: "maxWireVersion", Value: int32(21)},
			{Key: "maxBsonObjectSize", Value: int32(16 << 20)}, {Key: "maxMessageSizeBytes", Value: int32(48 << 20)}, {Key: "maxWriteBatchSize", Value: int32(100000)},
			{Key: "logicalSessionTimeoutMinutes", Value: int32(30)}, {Key: "localTime", Value: bson.NewDateTimeFromTime(time.Now())}}
	case "saslStart", "saslContinue", "authenticate":
		return bson.D{{Key: "ok", Value: 0.0}, {Key: "code", Value: int32(18)}, {Key: "codeName", Value: "AuthenticationFailed"}, {Key: "errmsg", Value: "Authentication failed."}}
	case "ping", "endSessions":
		return bson.D{{Key: "ok", Value: 1.0}}
	case "listDatabases":
		return bson.D{{Key: "ok", Value: 1.0}, {Key: "databases", Value: bson.A{bson.D{{Key: "name", Value: "app"}}}}}
	}
	return bson.D{{Key: "ok", Value: 0.0}, {Key: "code", Value: int32(59)}, {Key: "errmsg", Value: "no such command: " + name}}
}
