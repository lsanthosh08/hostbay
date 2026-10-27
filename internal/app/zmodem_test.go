package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Handshakes as lrzsz sends them (see internal/terminal/zmodem_test.go).
const (
	szHello = "rz\r**\x18B00000000000000\r\x8a\x11"
	rzHello = "**\x18B0100000023be50\r\x8a\x11"
)

// zmodemPage plays the page's side of ZmodemStreamName.
type zmodemPage struct {
	ctx    context.Context
	cancel context.CancelFunc
	in     chan []byte // page to Go
	out    chan []byte // Go to page
	served chan struct{}
	remote bytes.Buffer // server bytes received so far
}

func (p *zmodemPage) Receive() ([]byte, error) {
	select {
	case f := <-p.in:
		return f, nil
	case <-p.ctx.Done():
		return nil, errors.New("closed")
	}
}

func (p *zmodemPage) Send(b []byte) error {
	select {
	case p.out <- b:
		return nil
	case <-p.ctx.Done():
		return errors.New("closed")
	}
}

func (p *zmodemPage) Close() error             { p.cancel(); return nil }
func (p *zmodemPage) Context() context.Context { return p.ctx }

func (p *zmodemPage) send(typ byte, b []byte) { p.in <- append([]byte{typ}, b...) }

func (p *zmodemPage) sendJSON(typ byte, v any) {
	b, _ := json.Marshal(v)
	p.send(typ, b)
}

// next takes Go's next frame, collecting server bytes; ok is false once the
// stream has closed.
func (p *zmodemPage) next(t *testing.T) (f []byte, ok bool) {
	t.Helper()
	take := func(f []byte) ([]byte, bool) {
		if f[0] == zmRemote {
			p.remote.Write(f[1:])
		}
		return f, true
	}
	select {
	case f := <-p.out:
		return take(f)
	case <-p.ctx.Done():
		select { // frames sent before the close are all queued by now
		case f := <-p.out:
			return take(f)
		default:
			return nil, false
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for a frame")
		return nil, false
	}
}

func (p *zmodemPage) waitRemote(t *testing.T, want string) {
	t.Helper()
	for !strings.Contains(p.remote.String(), want) {
		if _, ok := p.next(t); !ok {
			t.Fatalf("stream closed; server sent %q, want %q", p.remote.String(), want)
		}
	}
}

func (p *zmodemPage) reply(t *testing.T) zmodemReply {
	t.Helper()
	for {
		f, ok := p.next(t)
		if !ok {
			t.Fatal("stream closed before a reply")
		}
		if f[0] == zmReply {
			var r zmodemReply
			if err := json.Unmarshal(f[1:], &r); err != nil {
				t.Fatal(err)
			}
			return r
		}
	}
}

func (p *zmodemPage) waitServed(t *testing.T) {
	t.Helper()
	select {
	case <-p.served:
	case <-time.After(3 * time.Second):
		t.Fatal("the stream handler did not return")
	}
}

type zmodemFixture struct {
	s   *ZmodemService
	c   *ConnService
	ev  *termEvents
	id  string
	dir string // the folder the dialog picks
}

// startZmodem opens a terminal on the echo shell and echoes hello, so the
// handshake comes back as the server's output, as from sz or rz.
func startZmodem(t *testing.T, hello, direction string) *zmodemFixture {
	t.Helper()
	c, _, ev, sid := newTermConn(t)
	s := NewZmodemService(c)
	started := make(chan string, 4)
	s.emit = func(name string, data any) { started <- name + " " + data.(string) }
	f := &zmodemFixture{s: s, c: c, ev: ev, dir: t.TempDir()}
	s.pick = func(bool) ([]string, error) { return []string{f.dir}, nil }
	f.id = openTerm(t, c, sid)
	if err := c.PtyWrite(f.id, hello); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-started:
		if want := ZmodemEvent(f.id) + " " + direction; got != want {
			t.Fatalf("event %q, want %q", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no Zmodem event")
	}
	return f
}

// open starts the handler; out is how many frames the page lets Go queue
// before it reads them.
func (f *zmodemFixture) open(t *testing.T, out int) *zmodemPage {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	p := &zmodemPage{ctx: ctx, cancel: cancel, in: make(chan []byte, 64), out: make(chan []byte, out), served: make(chan struct{})}
	t.Cleanup(cancel)
	go func() { f.s.serve(p); close(p.served) }()
	p.in <- []byte(f.id)
	return p
}

func TestZmodemDownloadSavesIntoTheChosenFolder(t *testing.T) {
	f := startZmodem(t, szHello, "download")
	if got, err := f.s.ZmodemChooseFolder(f.id); err != nil || got != f.dir {
		t.Fatalf("ZmodemChooseFolder = %q, %v", got, err)
	}
	p := f.open(t, 0) // a page slow to read, so output queues in Go
	p.waitRemote(t, "**\x18B00000000000000\r\x8a\x11")
	p.send(zmRemote, []byte("**\x18B0100000023be50\r\x8a\x11")) // zmodem.js's ZRINIT reaches the server
	p.waitRemote(t, "**\x18B0100000023be50")                    // and the echo shell sends it back

	var want []byte
	for i := range 1024 {
		want = append(want, byte(i))
	}
	p.sendJSON(zmSave, zmodemSave{Name: "data.bin", Size: int64(len(want))})
	if r := p.reply(t); r != (zmodemReply{Name: "data.bin"}) {
		t.Fatalf("save reply = %+v", r)
	}
	p.send(zmData, want[:300])
	p.send(zmData, want[300:])
	p.send(zmDone, nil)
	if r := p.reply(t); r != (zmodemReply{Name: "data.bin"}) {
		t.Fatalf("done reply = %+v", r)
	}
	if got, err := os.ReadFile(filepath.Join(f.dir, "data.bin")); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("saved file differs: %v", err)
	}

	// The shell's prompt races the page's end: wherever it lands, on the
	// stream for the page to show or in the text events, it appears once.
	// Its first part is taken by Go and waits on the page; the rest is
	// still unread in Go when the page ends.
	f.c.PtyWrite(f.id, "$ pro")
	time.Sleep(100 * time.Millisecond)
	f.c.PtyWrite(f.id, "mpt")
	time.Sleep(100 * time.Millisecond)
	p.send(zmEnd, nil)
	for {
		if _, ok := p.next(t); !ok {
			break
		}
	}
	p.waitServed(t)
	shown := func() int {
		return strings.Count(p.remote.String(), "$ prompt") + strings.Count(f.ev.joined(PtyDataEvent(f.id)), "$ prompt")
	}
	waitUntil(t, "the prompt", func() bool { return shown() > 0 })
	echoes(t, f.c, f.ev, f.id, "after\r")
	if n := shown(); n != 1 {
		t.Fatalf("the prompt was shown %d times", n)
	}
	if _, err := f.s.ZmodemChooseFolder(f.id); err == nil {
		t.Fatal("ZmodemChooseFolder worked after the transfer ended")
	}
}

func TestZmodemUploadReadsOnlyTheChosenFiles(t *testing.T) {
	f := startZmodem(t, rzHello, "upload")
	a, b := filepath.Join(f.dir, "a.txt"), filepath.Join(f.dir, "b.bin")
	os.WriteFile(a, []byte("alpha"), 0o600)
	os.WriteFile(b, []byte("\x00\x01\x02\xff"), 0o600)
	f.s.pick = func(folder bool) ([]string, error) {
		if folder {
			t.Error("asked for a folder for an upload")
		}
		return []string{a, b}, nil
	}
	files, err := f.s.ZmodemChooseFiles(f.id)
	if err != nil || len(files) != 2 || files[0] != (ZmodemFile{"a.txt", 5}) || files[1] != (ZmodemFile{"b.bin", 4}) {
		t.Fatalf("ZmodemChooseFiles = %+v, %v", files, err)
	}

	p := f.open(t, 64)
	p.waitRemote(t, rzHello)
	// The page sends each file as zmodem.js would, reading it through Go.
	var sent []byte
	for i, fl := range files {
		for off := int64(0); off < fl.Size; off += 2 {
			chunk, err := f.s.ZmodemReadFile(f.id, i, off, 2)
			if err != nil {
				t.Fatal(err)
			}
			sent = append(sent, chunk...)
			p.send(zmRemote, chunk)
		}
	}
	if string(sent) != "alpha\x00\x01\x02\xff" {
		t.Fatalf("read %q", sent)
	}
	p.waitRemote(t, rzHello+"alpha\x00\x01\x02\xff") // the server got every byte

	if got, err := f.s.ZmodemReadFile(f.id, 0, 5, 10); err != nil || len(got) != 0 {
		t.Fatalf("read at the end = %q, %v", got, err)
	}
	for _, idx := range []int{-1, 2} {
		if _, err := f.s.ZmodemReadFile(f.id, idx, 0, 1); err == nil {
			t.Fatalf("read file %d that was never chosen", idx)
		}
	}
	p.send(zmEnd, nil)
	p.waitServed(t)
	if _, err := f.s.ZmodemReadFile(f.id, 0, 0, 1); err == nil {
		t.Fatal("read a chosen file after the transfer ended")
	}
}

func TestZmodemRefusesNamesOutsideTheFolder(t *testing.T) {
	f := startZmodem(t, szHello, "download")
	f.dir = filepath.Join(f.dir, "in")
	os.Mkdir(f.dir, 0o700)
	f.s.ZmodemChooseFolder(f.id)
	p := f.open(t, 64)
	for _, name := range []string{"../x", "/etc/passwd", "a/b", `a\b`, "..", ".", ""} {
		p.sendJSON(zmSave, zmodemSave{Name: name, Size: 1})
		if r := p.reply(t); r.Error == "" || r.Name != "" {
			t.Fatalf("save %q = %+v, want refused", name, r)
		}
		p.send(zmData, []byte("x"))
		p.send(zmDone, nil)
		if r := p.reply(t); r.Error == "" {
			t.Fatalf("done after refusing %q = %+v", name, r)
		}
	}
	for _, d := range []string{f.dir, filepath.Dir(f.dir)} {
		if des, _ := os.ReadDir(d); d == f.dir && len(des) != 0 || d != f.dir && len(des) != 1 {
			t.Fatalf("%s holds %v", d, des)
		}
	}
	p.send(zmEnd, nil)
	p.waitServed(t)
}

func TestZmodemAsksBeforeReplacingAFile(t *testing.T) {
	f := startZmodem(t, szHello, "download")
	f.s.ZmodemChooseFolder(f.id)
	old := filepath.Join(f.dir, "a.txt")
	os.WriteFile(old, []byte("old"), 0o600)
	p := f.open(t, 64)
	save := func(conflict, body string) zmodemReply {
		p.sendJSON(zmSave, zmodemSave{Name: "a.txt", Size: int64(len(body)), Conflict: conflict})
		r := p.reply(t)
		if r.Exists || r.Error != "" {
			return r
		}
		p.send(zmData, []byte(body))
		p.send(zmDone, nil)
		return p.reply(t)
	}
	if r := save("", "new"); r != (zmodemReply{Name: "a.txt", Exists: true}) {
		t.Fatalf("save over a file = %+v, want exists", r)
	}
	if r := save("rename", "renamed"); r.Name != "a (1).txt" {
		t.Fatalf("rename = %+v", r)
	}
	if b, _ := os.ReadFile(old); string(b) != "old" {
		t.Fatalf("a.txt = %q before overwrite was chosen", b)
	}
	if b, _ := os.ReadFile(filepath.Join(f.dir, "a (1).txt")); string(b) != "renamed" {
		t.Fatalf("a (1).txt = %q", b)
	}
	if r := save("overwrite", "new"); r.Name != "a.txt" {
		t.Fatalf("overwrite = %+v", r)
	}
	if b, _ := os.ReadFile(old); string(b) != "new" {
		t.Fatalf("a.txt = %q after overwrite", b)
	}
	p.send(zmEnd, nil)
	p.waitServed(t)
}

func TestZmodemCancelMidTransferLeavesNoFile(t *testing.T) {
	f := startZmodem(t, szHello, "download")
	f.s.ZmodemChooseFolder(f.id)
	p := f.open(t, 64)
	p.sendJSON(zmSave, zmodemSave{Name: "big.bin", Size: 1 << 20})
	if r := p.reply(t); r.Name != "big.bin" {
		t.Fatalf("save = %+v", r)
	}
	p.send(zmData, []byte("partial"))
	p.waitRemote(t, "**\x18B") // the handler is running
	p.Close()                  // the tab closed or the page reloaded
	p.waitServed(t)
	if des, _ := os.ReadDir(f.dir); len(des) != 0 {
		t.Fatalf("folder holds %v after cancel", des)
	}
	// The echo shell returns the cancel sequence as text, so the server got it.
	waitUntil(t, "the cancel sequence", func() bool {
		return strings.Contains(f.ev.joined(PtyDataEvent(f.id)), "\x18\x18\x18\x18\x18\x18\x18\x18\x18\x18")
	})
	echoes(t, f.c, f.ev, f.id, "after\r")
}

func TestZmodemCancelBeforeTheStreamOpens(t *testing.T) {
	f := startZmodem(t, rzHello, "upload")
	if err := f.s.ZmodemCancel(f.id); err != nil {
		t.Fatal(err)
	}
	echoes(t, f.c, f.ev, f.id, "after\r")
	if err := f.s.ZmodemCancel(f.id); err == nil {
		t.Fatal("cancelled a transfer that had ended")
	}
}
