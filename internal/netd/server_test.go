package netd

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// session is one Hostbay connection to a helper serving a fake table.
type session struct {
	table  *fakeTable
	client *Client
	done   chan struct{} // closed when the helper has finished with the connection
	server *Server
}

func newServer(t *testing.T, table *fakeTable) *Server {
	t.Helper()
	j, err := OpenJournal(filepath.Join(t.TempDir(), "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	return &Server{Table: table, Journal: j, Auth: func(net.Conn) error { return nil }}
}

func (s *Server) pipe() (net.Conn, chan struct{}) {
	a, b := net.Pipe()
	done := make(chan struct{})
	go func() { s.handle(a); close(done) }()
	return b, done
}

func connect(t *testing.T, table *fakeTable) *session {
	t.Helper()
	s := newServer(t, table)
	conn, done := s.pipe()
	c, err := newClient(conn)
	if err != nil {
		t.Fatal(err)
	}
	// The helper sweeps on disconnect; let it finish before the journal's directory goes.
	t.Cleanup(func() { c.Close(); <-done })
	return &session{table: table, client: c, done: done, server: s}
}

func (s *session) disconnect(t *testing.T) {
	t.Helper()
	s.client.Close()
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the helper did not finish the connection")
	}
}

func TestAddThenDisconnectLeavesNothing(t *testing.T) {
	ctx := context.Background()
	table := newFakeTable(defaultRoute, lanRoute)
	before := table.snapshot()
	s := connect(t, table)

	if err := s.client.Add(ctx, Route{Dst: "10.20.0.0/16", Iface: "utun3"}); err != nil {
		t.Fatal(err)
	}
	if err := s.client.Add(ctx, Route{Dst: "203.0.113.7/32", Gateway: true}); err != nil {
		t.Fatal(err)
	}
	if !table.has(sr("10.20.0.0/16", "utun3")) {
		t.Fatal("the tunnel route was not added")
	}
	pinned := SysRoute{Dst: sr("203.0.113.7/32", "").Dst, Iface: "en0", Gateway: lanGateway}
	if !table.has(pinned) {
		t.Fatalf("the gateway route did not go via the default gateway: %v", table.snapshot())
	}

	s.disconnect(t)
	if got := table.snapshot(); !slices.Equal(got, before) {
		t.Errorf("routes after disconnect = %v, want %v", got, before)
	}
	if n := len(s.server.Journal.entries); n != 0 {
		t.Errorf("journal keeps %d entries after the sweep", n)
	}
}

func TestNarrowVendorRouteRestoredOnDisconnect(t *testing.T) {
	ctx := context.Background()
	vendor := SysRoute{Dst: sr("10.0.0.0/8", "").Dst, Iface: "utun5", Gateway: lanGateway.Next()}
	table := newFakeTable(defaultRoute, vendor)
	s := connect(t, table)

	if err := s.client.Delete(ctx, Route{Dst: "10.0.0.0/8", Iface: "utun5"}); err != nil {
		t.Fatal(err)
	}
	if err := s.client.Add(ctx, Route{Dst: "10.1.0.0/16", Iface: "utun5"}); err != nil {
		t.Fatal(err)
	}
	if table.has(vendor) {
		t.Fatal("the vendor route was not deleted")
	}

	s.disconnect(t)
	if !table.has(vendor) {
		t.Errorf("10.0.0.0/8 was not restored on utun5 with its gateway: %v", table.snapshot())
	}
	if table.has(sr("10.1.0.0/16", "utun5")) {
		t.Error("the narrowed route outlived the client")
	}
}

func TestRouteBounds(t *testing.T) {
	ctx := context.Background()
	table := newFakeTable(defaultRoute, lanRoute, sr("172.16.0.0/12", "utun3"))
	before := table.snapshot()
	s := connect(t, table)

	refused := map[string]error{
		"add via physical non-gateway iface": s.client.Add(ctx, Route{Dst: "10.0.0.0/8", Iface: "en1"}),
		"add on-link on gateway iface":       s.client.Add(ctx, Route{Dst: "10.0.0.0/8", Iface: "en0"}),
		"add via missing iface":              s.client.Add(ctx, Route{Dst: "10.0.0.0/8", Iface: "utun9"}),
		"delete unjournaled physical route":  s.client.Delete(ctx, Route{Dst: "192.168.1.0/24", Iface: "en0"}),
		"delete default route on en0":        s.client.Delete(ctx, Route{Dst: "0.0.0.0/0", Iface: "en0"}),
		"mtu of physical iface":              s.client.SetMTU(ctx, "en0", 1400),
	}
	for name, err := range refused {
		if errCode(err) != CodeRefused {
			t.Errorf("%s: err = %v, want %s", name, err, CodeRefused)
		}
	}
	if errCode(s.client.Delete(ctx, Route{Dst: "10.9.0.0/16", Iface: "utun3"})) == "" {
		t.Error("deleting a route that does not exist succeeded")
	}
	if got := table.snapshot(); !slices.Equal(got, before) {
		t.Errorf("refused requests changed the table: %v", got)
	}

	invalid := map[string]error{
		"shell string": s.client.Add(ctx, Route{Dst: "10.0.0.0/8", Iface: "utun3; rm -rf /"}),
		"mtu 100":      s.client.SetMTU(ctx, "utun3", 100),
	}
	for name, err := range invalid {
		if errCode(err) != CodeInvalid {
			t.Errorf("%s: err = %v, want %s", name, err, CodeInvalid)
		}
	}

	if err := s.client.SetMTU(ctx, "utun3", 1380); err != nil || table.mtu["utun3"] != 1380 {
		t.Errorf("SetMTU(utun3, 1380) = %v, mtu %d", err, table.mtu["utun3"])
	}
	// A vendor route on a tunnel interface may be deleted, and comes back.
	if err := s.client.Delete(ctx, Route{Dst: "172.16.0.0/12", Iface: "utun3"}); err != nil {
		t.Fatal(err)
	}
	s.disconnect(t)
	if got := table.snapshot(); !slices.Equal(got, before) {
		t.Errorf("routes after disconnect = %v, want %v", got, before)
	}
}

func TestApplyReportsEachChange(t *testing.T) {
	table := newFakeTable(defaultRoute)
	s := connect(t, table)
	errs := s.client.Apply(context.Background(), []Change{
		{Op: Add, Route: Route{Dst: "10.0.0.0/8", Iface: "utun3"}},
		{Op: Add, Route: Route{Dst: "10.0.0.0/8", Iface: "utun3"}}, // already there
		{Op: Add, Route: Route{Dst: "10.2.0.0/16", Iface: "en1"}},
	})
	if errs[0] != nil || errCode(errs[1]) != CodeFailed || errCode(errs[2]) != CodeRefused {
		t.Errorf("Apply = %v", errs)
	}
}

// rawSession speaks the protocol by hand, for messages the client never sends.
func rawSession(t *testing.T, s *Server) (*json.Encoder, *bufio.Scanner) {
	t.Helper()
	conn, _ := s.pipe()
	t.Cleanup(func() { conn.Close() })
	in := bufio.NewScanner(conn)
	if !in.Scan() {
		t.Fatal("no hello")
	}
	return json.NewEncoder(conn), in
}

func readMsg(t *testing.T, in *bufio.Scanner) Msg {
	t.Helper()
	var m Msg
	if !in.Scan() || json.Unmarshal(in.Bytes(), &m) != nil {
		t.Fatal("no reply")
	}
	return m
}

func TestOldProtocolVersionRefused(t *testing.T) {
	table := newFakeTable(defaultRoute)
	enc, in := rawSession(t, newServer(t, table))
	enc.Encode(Msg{T: Add, V: Version - 1, ID: 1, Route: &Route{Dst: "10.0.0.0/8", Iface: "utun3"}})
	if m := readMsg(t, in); m.Code != CodeVersion || m.ID != 1 {
		t.Errorf("reply = %+v, want %s", m, CodeVersion)
	}
	if table.has(sr("10.0.0.0/8", "utun3")) {
		t.Error("an old-version request changed the table")
	}

	// And Hostbay refuses a helper that greets it with another version.
	a, b := net.Pipe()
	go json.NewEncoder(a).Encode(Msg{T: Hello, V: Version + 1})
	if _, err := newClient(b); errCode(err) != CodeVersion {
		t.Errorf("newClient with a newer helper = %v, want %s", err, CodeVersion)
	}
}

func TestReservedOperationsUnsupported(t *testing.T) {
	enc, in := rawSession(t, newServer(t, newFakeTable()))
	for i, op := range []string{OpenVPNStart, OpenVPNStop, OpenConnectStart, OpenConnectStop, WireGuardStart, WireGuardStop} {
		enc.Encode(Msg{T: op, V: Version, ID: uint64(i + 1)})
		if m := readMsg(t, in); m.Code != CodeUnsupported {
			t.Errorf("%s: reply = %+v, want %s", op, m, CodeUnsupported)
		}
	}
}

func TestRefusedCallerGetsNothing(t *testing.T) {
	table := newFakeTable(defaultRoute)
	s := newServer(t, table)
	s.Auth = func(net.Conn) error { return &Error{Code: CodeDenied, Reason: "not the paired Hostbay"} }
	conn, done := s.pipe()
	if _, err := newClient(conn); errCode(err) != CodeDenied {
		t.Errorf("newClient = %v, want %s", err, CodeDenied)
	}
	<-done
}

func TestOneClientAtATime(t *testing.T) {
	s := connect(t, newFakeTable(defaultRoute))
	conn, _ := s.server.pipe()
	if _, err := newClient(conn); errCode(err) != CodeBusy {
		t.Errorf("second client = %v, want %s", err, CodeBusy)
	}
}

func TestRouteEventsReachTheClient(t *testing.T) {
	s := connect(t, newFakeTable(defaultRoute))
	s.server.Notify()
	select {
	case <-s.client.Events():
	case <-time.After(5 * time.Second):
		t.Fatal("no route event")
	}
}

// Hostbay removes a gateway pin the way it asked for it, without learning
// which interface the helper chose.
func TestDeleteThroughTheGateway(t *testing.T) {
	ctx := context.Background()
	table := newFakeTable(defaultRoute, lanRoute)
	before := table.snapshot()
	s := connect(t, table)
	if err := s.client.Delete(ctx, Route{Dst: "203.0.113.7/32", Gateway: true}); errCode(err) != CodeRefused {
		t.Errorf("deleting a gateway route Hostbay never added = %v, want %s", err, CodeRefused)
	}
	if err := s.client.Add(ctx, Route{Dst: "203.0.113.7/32", Gateway: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.client.Delete(ctx, Route{Dst: "203.0.113.7/32", Gateway: true}); err != nil {
		t.Fatal(err)
	}
	if got := table.snapshot(); !slices.Equal(got, before) {
		t.Errorf("routes = %v, want %v", got, before)
	}
}

func TestNoChangeAfterStop(t *testing.T) {
	table := newFakeTable(defaultRoute)
	s := connect(t, table)
	s.server.Stop()
	if err := s.client.Add(context.Background(), Route{Dst: "10.0.0.0/8", Iface: "utun3"}); errCode(err) != CodeRefused || table.has(sr("10.0.0.0/8", "utun3")) {
		t.Errorf("add after Stop = %v", err)
	}
}
