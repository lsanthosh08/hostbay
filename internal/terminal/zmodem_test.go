package terminal

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// Handshakes as lrzsz sends them: sz prints "rz\r" then ZRQINIT; rz sends
// ZRINIT with CANFC32|CANFDX|CANOVIO, whose CRC (be50) comes from lrzsz, not
// from this package.
const (
	szHello = "rz\r**\x18B00000000000000\r\x8a\x11"
	rzHello = "**\x18B0100000023be50\r\x8a\x11"
)

func TestZmodemStartFindsTheHandshakes(t *testing.T) {
	for _, c := range []struct {
		in  string
		at  int
		dir string
	}{
		{szHello, 3, "download"},
		{rzHello, 0, "upload"},
		{"** bold ** in a vim buffer\r\n", -1, ""},
		{"**\x18B0000000000ffff\r\n", -1, ""}, // wrong CRC
		{"**\x18Bzz000000000000\r\n", -1, ""}, // not hex
		{"**\x18B000000", -1, ""},             // cut short
		{"x**\x18B0000000000ffff**\x18B00000000000000", 19, "download"},
	} {
		at, dir := zmodemStart([]byte(c.in))
		if at != c.at || dir != c.dir {
			t.Errorf("zmodemStart(%q) = %d, %q; want %d, %q", c.in, at, dir, c.at, c.dir)
		}
	}
}

// syncBuf is a bytes.Buffer safe to read while the tap writes.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newTestTap() (*zmodemTap, *syncBuf, *syncBuf, chan *Zmodem) {
	text, remote := &syncBuf{}, &syncBuf{}
	started := make(chan *Zmodem, 1)
	return newZmodemTap(text, remote, func(x *Zmodem) { started <- x }), text, remote, started
}

func TestTapPassesLookAlikeOutputThroughAtOnce(t *testing.T) {
	z, text, _, started := newTestTap()
	want := ""
	for _, chunk := range []string{
		"** bold **\r\n", "\x18\x18 cancel bytes", "**\x18B", "0000000000ffff\r\n", // a header with a bad CRC, split
		"**", "\x18B00", "zz00000000000\r\n", "héllo wörld *", "*", "\x18",
	} {
		if n, err := z.Write([]byte(chunk)); n != len(chunk) || err != nil {
			t.Fatalf("Write(%q) = %d, %v", chunk, n, err)
		}
		want += chunk
		if got := text.String(); got != want {
			t.Fatalf("text = %q, want %q (held back or altered)", got, want)
		}
	}
	select {
	case <-started:
		t.Fatal("a transfer started on ordinary output")
	default:
	}
}

func TestTapHandsATransferToThePageAndBack(t *testing.T) {
	z, text, remote, started := newTestTap()
	// The header arrives split across two reads.
	z.Write([]byte("hello\r\nrz\r**\x18B0000"))
	z.Write([]byte("0000000000\r\x8a\x11ZFILE"))
	x := <-started
	if x.Direction != "download" {
		t.Fatalf("direction = %q", x.Direction)
	}
	ctx := context.Background()
	if b, err := x.Read(ctx); err != nil || string(b) != "**\x18B00000000000000\r\x8a\x11ZFILE" {
		t.Fatalf("page got %q, %v", b, err)
	}
	if err := x.Write([]byte("ZRINIT")); err != nil || remote.String() != "ZRINIT" {
		t.Fatalf("server got %q, %v", remote.String(), err)
	}
	z.Write([]byte("binary\x00\xff"))
	if b, _ := x.Read(ctx); string(b) != "binary\x00\xff" {
		t.Fatalf("page got %q", b)
	}
	z.Write([]byte("$ ")) // the shell's prompt, not yet read when the page ends
	if rest := x.End(); string(rest) != "$ " {
		t.Fatalf("End returned %q", rest)
	}
	if _, err := x.Read(ctx); err != io.EOF {
		t.Fatalf("Read after End = %v, want EOF", err)
	}
	if x.Write([]byte("x")) == nil {
		t.Fatal("Write after End succeeded")
	}
	z.Write([]byte("ls\r\n"))
	if got, want := text.String(), "hello\r\nrz\r**\x18B0000ls\r\n"; got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
}

func TestTapIdleGuardCancelsTheServer(t *testing.T) {
	z, text, remote, started := newTestTap()
	z.idle = 20 * time.Millisecond
	z.Write([]byte("$ rz\r\n" + rzHello)) // shell output before the header in the same read
	x := <-started
	select {
	case <-x.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("idle transfer never ended")
	}
	if remote.String() != string(zmodemCancel) {
		t.Fatalf("server got %q, want the cancel sequence", remote.String())
	}
	if _, err := x.Read(context.Background()); err != io.EOF {
		t.Fatalf("Read = %v, want EOF", err)
	}
	z.Write([]byte("$ "))
	if text.String() != "$ rz\r\n$ " {
		t.Fatalf("text = %q", text.String())
	}
}

func TestTapCloseEndsTheTransfer(t *testing.T) {
	z, _, _, started := newTestTap()
	z.Write([]byte(szHello))
	x := <-started
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	x.Read(ctx) // the header
	go z.close()
	if _, err := x.Read(ctx); err != io.EOF {
		t.Fatalf("Read = %v, want EOF", err)
	}
}

func TestOnlySSHTerminalsAreTapped(t *testing.T) {
	r, l := newLoggedRegistry()
	r.OnZmodem(func(id, dir string) { t.Errorf("transfer started on a non-SSH terminal") })
	p := newPipe()
	id := r.Open("s1", p)
	r.Attach(id)
	p.remote.Write([]byte(szHello))
	waitFor(t, "the handshake as text", func() bool {
		ev := l.get()
		return len(ev) == 1 && strings.HasSuffix(ev[0], " data "+szHello)
	})
	if _, err := r.Zmodem(id); !errors.Is(err, errNoZmodem) {
		t.Fatalf("Zmodem = %v, want errNoZmodem", err)
	}
	r.Close(id)
}
