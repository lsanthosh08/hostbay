package transfer_test

import (
	"context"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"hostbay/internal/transfer"
)

type stamped struct {
	transfer.Event
	at time.Time
}

func newEngine() (*transfer.Engine, chan stamped) {
	ch := make(chan stamped, 10000)
	return transfer.New(func(ev transfer.Event) { ch <- stamped{ev, time.Now()} }), ch
}

// wait collects task id's events until it ends, calling onEvent (if set)
// for each one, and returns them all; the last is the final one.
func wait(t *testing.T, ch chan stamped, id string, onEvent func(transfer.Event)) []stamped {
	t.Helper()
	var evs []stamped
	timeout := time.After(20 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.TaskID != id {
				continue
			}
			evs = append(evs, ev)
			if onEvent != nil {
				onEvent(ev.Event)
			}
			switch ev.State {
			case "done", "failed", "cancelled":
				return evs
			}
		case <-timeout:
			t.Fatalf("task %s did not end; events: %+v", id, evs)
		}
	}
}

// answerAll answers every conflict with choice.
func answerAll(t *testing.T, e *transfer.Engine, choice transfer.Choice, all bool) func(transfer.Event) {
	return func(ev transfer.Event) {
		if ev.State == "conflict" {
			if err := e.Answer(ev.Conflict.RequestID, choice, all); err != nil {
				t.Error(err)
			}
		}
	}
}

// slow is a local source whose reads return 1 KiB after a pause, so a test
// can act while a copy is under way.
type slow struct{ transfer.Local }

func (s slow) Open(ctx context.Context, p string) (io.ReadCloser, error) {
	r, err := s.Local.Open(ctx, p)
	return slowReader{r}, err
}

type slowReader struct{ io.ReadCloser }

func (r slowReader) Read(b []byte) (int, error) {
	time.Sleep(2 * time.Millisecond)
	return r.ReadCloser.Read(b[:min(len(b), 1024)])
}

func TestRenamePicksTheFirstFreeNumberedName(t *testing.T) {
	src, dst := tempDir(t), tempDir(t)
	writeFile(t, src+"/a.txt", "new", 0o644)
	writeFile(t, dst+"/a.txt", "old", 0o644)
	e, ch := newEngine()
	for _, want := range []string{"a (1).txt", "a (2).txt"} {
		id := e.Start(transfer.Local{}, []string{src + "/a.txt"}, transfer.Local{}, dst)
		evs := wait(t, ch, id, answerAll(t, e, transfer.Rename, false))
		if last := evs[len(evs)-1]; last.State != "done" {
			t.Fatalf("final %+v", last.Event)
		}
		if got := readFile(t, dst+"/"+want); got != "new" {
			t.Fatalf("%s = %q", want, got)
		}
	}
	if readFile(t, dst+"/a.txt") != "old" {
		t.Fatal("original changed")
	}
}

func TestCancelMidCopyLeavesTheOriginalAndNoTempFile(t *testing.T) {
	src, dst := tempDir(t), tempDir(t)
	writeFile(t, src+"/big.bin", strings.Repeat("x", 1<<20), 0o644)
	writeFile(t, dst+"/big.bin", "original", 0o644)
	e, ch := newEngine()
	id := e.Start(slow{}, []string{src + "/big.bin"}, transfer.Local{}, dst)
	cancelled := false
	evs := wait(t, ch, id, func(ev transfer.Event) {
		answerAll(t, e, transfer.Overwrite, false)(ev)
		if ev.State == "running" && ev.Bytes > 0 && !cancelled {
			cancelled = true
			if err := e.Cancel(id); err != nil {
				t.Error(err)
			}
		}
	})
	if last := evs[len(evs)-1]; last.State != "cancelled" {
		t.Fatalf("final %+v", last.Event)
	}
	if got := readFile(t, dst+"/big.bin"); got != "original" {
		t.Fatalf("destination = %d bytes", len(got))
	}
	if got := dirNames(t, dst); len(got) != 1 {
		t.Fatalf("destination holds %v", got)
	}
}

func TestProgressIsThrottledAndTheFinalEventCarriesTheTotal(t *testing.T) {
	src, dst := tempDir(t), tempDir(t)
	const size = 300 << 10
	writeFile(t, src+"/f.bin", strings.Repeat("y", size), 0o644)
	e, ch := newEngine()
	id := e.Start(slow{}, []string{src + "/f.bin"}, transfer.Local{}, dst)
	evs := wait(t, ch, id, nil)

	var running []stamped
	for _, ev := range evs {
		if ev.State == "running" {
			running = append(running, ev)
		}
	}
	if len(running) < 3 {
		t.Fatalf("only %d progress events", len(running))
	}
	for i := 1; i < len(running); i++ {
		if gap := running[i].at.Sub(running[i-1].at); gap < 95*time.Millisecond {
			t.Fatalf("progress events %v apart", gap)
		}
	}
	last := evs[len(evs)-1]
	if last.State != "done" || last.Total != size || last.Bytes != size || last.Files != 1 || last.FileCount != 1 {
		t.Fatalf("final %+v", last.Event)
	}
	if fi, err := os.Stat(dst + "/f.bin"); err != nil || fi.Size() != size {
		t.Fatalf("copied file: %v", err)
	}
}

func TestAnswerAndCancelRefuseUnknownIDs(t *testing.T) {
	e, _ := newEngine()
	if err := e.Answer("nope", transfer.Skip, false); err == nil {
		t.Fatal("unknown request answered")
	}
	if err := e.Cancel("nope"); err == nil {
		t.Fatal("unknown task cancelled")
	}
}

// A listing name with a backslash is a path on an SMB share or a Windows
// disk, so ..\x would land outside the folder it was listed in.
func TestBackslashNameFromASourceIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a Windows file name cannot hold a backslash")
	}
	src, dst := tempDir(t), tempDir(t)
	if err := os.Mkdir(src+"/d", 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, src+"/d/ok.txt", "fine", 0o644)
	writeFile(t, src+`/d/..\x`, "escaped", 0o644)
	e, ch := newEngine()
	id := e.Start(transfer.Local{}, []string{src + "/d"}, transfer.Local{}, dst)
	evs := wait(t, ch, id, nil)
	last := evs[len(evs)-1]
	if last.State != "failed" || len(last.Failed) != 1 || !strings.HasSuffix(last.Failed[0].Path, `/..\x`) {
		t.Fatalf("final %+v", last.Event)
	}
	if got := dirNames(t, dst+"/d"); len(got) != 1 || got[0] != "ok.txt" {
		t.Fatalf("destination holds %v", got)
	}
}

// stalled is a local source whose reads block until the reader is closed,
// as a read from a server that stopped answering does.
type stalled struct {
	transfer.Local
	reading chan struct{}
}

func (s stalled) Open(context.Context, string) (io.ReadCloser, error) {
	return &stalledReader{reading: s.reading, closed: make(chan struct{})}, nil
}

type stalledReader struct {
	reading chan struct{}
	once    sync.Once
	closed  chan struct{}
}

func (r *stalledReader) Read([]byte) (int, error) {
	r.once.Do(func() { close(r.reading) })
	<-r.closed
	return 0, os.ErrClosed
}

// Close panics if called twice, as closing a closed channel does.
func (r *stalledReader) Close() error {
	close(r.closed)
	return nil
}

func TestCancelInterruptsAReadThatNeverReturns(t *testing.T) {
	src, dst := tempDir(t), tempDir(t)
	writeFile(t, src+"/f.bin", "data", 0o644)
	e, ch := newEngine()
	reading := make(chan struct{})
	id := e.Start(stalled{reading: reading}, []string{src + "/f.bin"}, transfer.Local{}, dst)
	select {
	case <-reading:
	case <-time.After(10 * time.Second):
		t.Fatal("the copy never started reading")
	}
	start := time.Now()
	if err := e.Cancel(id); err != nil {
		t.Fatal(err)
	}
	evs := wait(t, ch, id, nil)
	if last := evs[len(evs)-1]; last.State != "cancelled" || len(last.Failed) != 0 {
		t.Fatalf("final %+v", last.Event)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("cancel took %v", d)
	}
	if got := dirNames(t, dst); len(got) != 0 {
		t.Fatalf("destination holds %v", got)
	}
}
