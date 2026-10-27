package transfer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"sync"
	"time"
)

// progressEvery is the shortest gap between two progress events of one
// task; state changes are sent at once.
const progressEvery = 100 * time.Millisecond

// Choice answers a name conflict.
type Choice string

const (
	Overwrite Choice = "overwrite"
	Skip      Choice = "skip"
	Rename    Choice = "rename" // copy as "name (1).ext", or the first free number
)

// Event is a task's state. State is running, conflict (waiting on Answer),
// done, failed (some files failed, or the task could not start) or
// cancelled; the last event of every task has one of the final three and
// carries the total.
type Event struct {
	TaskID    string    `json:"taskId"`
	State     string    `json:"state"`
	Bytes     int64     `json:"bytes"` // copied or skipped so far
	Total     int64     `json:"total"`
	Files     int       `json:"files"` // files finished: copied, skipped or failed
	FileCount int       `json:"fileCount"`
	Current   string    `json:"current,omitempty"`
	Skipped   []string  `json:"skipped,omitempty"`
	Failed    []Failure `json:"failed,omitempty"`
	Conflict  *Conflict `json:"conflict,omitempty"`
	Error     string    `json:"error,omitempty"`
}

type Failure struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// Conflict asks what to do about a destination file that already exists.
type Conflict struct {
	RequestID string `json:"requestId"`
	Path      string `json:"path"`
	Size      uint64 `json:"size"`  // the existing file's
	Mtime     int64  `json:"mtime"` // the existing file's
}

type answer struct {
	choice Choice
	all    bool
}

// Engine runs transfer tasks between any two Sources (KTD17).
type Engine struct {
	emit func(Event)

	mu      sync.Mutex
	tasks   map[string]context.CancelFunc
	pending map[string]chan answer // conflict request ID -> its waiting task
	busy    map[Source]int         // files moving per source
	freed   chan struct{}          // closed and replaced when a slot frees
}

// New returns an engine that reports every task's progress to emit.
func New(emit func(Event)) *Engine {
	return &Engine{emit: emit, tasks: map[string]context.CancelFunc{}, pending: map[string]chan answer{},
		busy: map[Source]int{}, freed: make(chan struct{})}
}

// Start copies paths of from, files or whole folders, into the folder toDir
// of to, and returns the task's ID at once; the work and its events follow
// in the background.
func (e *Engine) Start(from Source, paths []string, to Source, toDir string) string {
	ctx, cancel := context.WithCancel(context.Background())
	t := &task{e: e, ctx: ctx, from: from, to: to, toDir: toDir, ev: Event{TaskID: newID(), State: "running"}}
	e.mu.Lock()
	e.tasks[t.ev.TaskID] = cancel
	e.mu.Unlock()
	go func() {
		defer func() {
			e.mu.Lock()
			delete(e.tasks, t.ev.TaskID)
			e.mu.Unlock()
			cancel()
		}()
		t.run(paths)
	}()
	return t.ev.TaskID
}

// Cancel stops a task. Its file in progress is aborted, so nothing partial
// is left (except an SCP overwrite, which writes in place).
func (e *Engine) Cancel(taskID string) error {
	e.mu.Lock()
	cancel := e.tasks[taskID]
	e.mu.Unlock()
	if cancel == nil {
		return errors.New("this transfer has already ended")
	}
	cancel()
	return nil
}

// Answer resolves a conflict question; with all, the choice also answers
// the task's later conflicts. Each request ID works once.
func (e *Engine) Answer(requestID string, choice Choice, all bool) error {
	switch choice {
	case Overwrite, Skip, Rename:
	default:
		return fmt.Errorf("unknown choice %q", choice)
	}
	e.mu.Lock()
	ch := e.pending[requestID]
	delete(e.pending, requestID)
	e.mu.Unlock()
	if ch == nil {
		return errors.New("this question has already been answered or its transfer ended")
	}
	ch <- answer{choice, all}
	return nil
}

// acquire waits for a free slot on both sources and takes them together,
// so two tasks running opposite ways cannot each hold one and wait.
func (e *Engine) acquire(ctx context.Context, a, b Source) (func(), error) {
	srcs := []Source{a}
	if b != a {
		srcs = append(srcs, b)
	}
	for {
		e.mu.Lock()
		free := true
		for _, s := range srcs {
			free = free && e.busy[s] < limit(s)
		}
		if free {
			for _, s := range srcs {
				e.busy[s]++
			}
			e.mu.Unlock()
			return func() {
				e.mu.Lock()
				for _, s := range srcs {
					if e.busy[s]--; e.busy[s] == 0 {
						delete(e.busy, s)
					}
				}
				close(e.freed)
				e.freed = make(chan struct{})
				e.mu.Unlock()
			}, nil
		}
		wait := e.freed
		e.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

type task struct {
	e        *Engine
	ctx      context.Context
	from, to Source
	toDir    string
	ev       Event
	last     time.Time // last event sent
	all      Choice    // the "apply to all" answer, once given
}

// item is one thing to create at the destination: a folder, or a file
// copied from src. rel is its slash-separated path under toDir.
type item struct {
	src, rel string
	size     int64
	dir      bool
}

func (t *task) run(paths []string) {
	items := t.walk(paths)
	for _, it := range items {
		if !it.dir {
			t.ev.FileCount++
			t.ev.Total += it.size
		}
	}
	t.send(true)
	for _, it := range items {
		if t.ctx.Err() != nil {
			break
		}
		var err error
		if it.dir {
			err = t.mkdir(it.rel)
		} else {
			t.ev.Current = it.rel
			err = t.copy(it)
			t.ev.Files++
		}
		if err != nil && t.ctx.Err() == nil {
			t.fail(it.src, err)
		}
	}
	t.ev.Current = ""
	switch {
	case t.ctx.Err() != nil:
		t.ev.State = "cancelled"
	case len(t.ev.Failed) > 0:
		t.ev.State = "failed"
	default:
		t.ev.State = "done"
	}
	t.send(true)
}

func (t *task) fail(p string, err error) {
	t.ev.Failed = append(t.ev.Failed, Failure{Path: p, Error: err.Error()})
}

// walk lists the selection, folders before what they hold. A name the
// source sent that is not one plain path element is refused, and a
// symlinked folder is not followed, so a loop cannot run forever.
func (t *task) walk(paths []string) []item {
	var items []item
	var add func(p, rel string, e Entry)
	add = func(p, rel string, e Entry) {
		if !e.IsDir {
			items = append(items, item{src: p, rel: rel, size: int64(e.Size)})
			return
		}
		items = append(items, item{src: p, rel: rel, dir: true})
		children, err := t.from.List(t.ctx, p)
		if err != nil {
			t.fail(p, err)
			return
		}
		for _, c := range children {
			cp := path.Join(p, c.Name)
			if !validName(c.Name) {
				t.fail(p+"/"+c.Name, errors.New("refusing a name that is not a plain file name"))
				continue
			}
			if isSymlink(c) {
				var err error
				if c, err = t.from.Stat(t.ctx, cp); err != nil {
					t.fail(cp, err)
					continue
				}
				if c.IsDir {
					t.fail(cp, errors.New("is a link to a folder, which is not followed"))
					continue
				}
			}
			add(cp, rel+"/"+c.Name, c)
		}
	}
	for _, p := range paths {
		e, err := t.from.Stat(t.ctx, p)
		if err != nil {
			t.fail(p, err)
			continue
		}
		add(p, path.Base(p), e)
	}
	return items
}

// mkdir makes a folder unless one is already there; its files then meet
// the destination's files one by one.
func (t *task) mkdir(rel string) error {
	e, err := t.to.Stat(t.ctx, path.Join(t.toDir, rel))
	switch {
	case err == nil && e.IsDir:
		return nil
	case err == nil:
		return errors.New("a file with this name is in the way")
	case errors.Is(err, fs.ErrNotExist):
		return t.to.Mkdir(t.ctx, t.toDir, rel)
	}
	return err
}

// copy copies one file, checking the destination just before writing.
func (t *task) copy(it item) error {
	rel := it.rel
	existing, err := t.to.Stat(t.ctx, path.Join(t.toDir, rel))
	if err == nil {
		choice, err := t.ask(path.Join(t.toDir, rel), existing)
		if err != nil {
			return err
		}
		switch choice {
		case Skip:
			t.ev.Skipped = append(t.ev.Skipped, path.Join(t.toDir, rel))
			t.ev.Bytes += it.size
			return nil
		case Rename:
			if rel, err = t.freeName(rel); err != nil {
				return err
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	release, err := t.e.acquire(t.ctx, t.from, t.to)
	if err != nil {
		return err
	}
	defer release()
	return copyFile(t.ctx, t.from, it.src, t.to, t.toDir, rel, it.size, func(n int) {
		t.ev.Bytes += int64(n)
		t.send(false)
	})
}

// ask returns the "apply to all" answer, or puts the question to the user
// and waits.
func (t *task) ask(p string, existing Entry) (Choice, error) {
	if t.all != "" {
		return t.all, nil
	}
	id := newID()
	ch := make(chan answer, 1)
	t.e.mu.Lock()
	t.e.pending[id] = ch
	t.e.mu.Unlock()
	defer func() {
		t.e.mu.Lock()
		delete(t.e.pending, id)
		t.e.mu.Unlock()
	}()

	t.ev.State, t.ev.Conflict = "conflict", &Conflict{RequestID: id, Path: p, Size: existing.Size, Mtime: existing.Mtime}
	t.send(true)
	defer func() { t.ev.State, t.ev.Conflict = "running", nil }()
	select {
	case a := <-ch:
		if a.all {
			t.all = a.choice
		}
		return a.choice, nil
	case <-t.ctx.Done():
		return "", t.ctx.Err()
	}
}

// freeName returns rel renamed "name (1).ext", or the first higher number
// that is free at the destination.
func (t *task) freeName(rel string) (string, error) {
	dir, base := path.Split(rel)
	ext := path.Ext(base)
	if ext == base { // a dotfile such as .bashrc has no extension
		ext = ""
	}
	stem := strings.TrimSuffix(base, ext)
	for n := 1; ; n++ {
		cand := fmt.Sprintf("%s%s (%d)%s", dir, stem, n, ext)
		_, err := t.to.Stat(t.ctx, path.Join(t.toDir, cand))
		if errors.Is(err, fs.ErrNotExist) {
			return cand, nil
		}
		if err != nil {
			return "", err
		}
	}
}

// send emits the task's state: always when force is set, otherwise at most
// once per progressEvery.
func (t *task) send(force bool) {
	now := time.Now()
	if !force && now.Sub(t.last) < progressEvery {
		return
	}
	t.last = now
	t.e.emit(t.ev)
}

// CopyFile copies the file p of from to rel inside the folder dir of to,
// replacing what is there, as one task's file is copied but without the
// task: no conflict question and no events.
func CopyFile(ctx context.Context, from Source, p string, to Source, dir, rel string) error {
	e, err := from.Stat(ctx, p)
	if err != nil {
		return err
	}
	return copyFile(ctx, from, p, to, dir, rel, int64(e.Size), func(int) {})
}

func copyFile(ctx context.Context, from Source, p string, to Source, dir, rel string, size int64, progress func(int)) error {
	r, err := from.Open(ctx, p)
	if err != nil {
		return err
	}
	// Cancel closes the reader, so a read blocked on a stalled server returns.
	// A reader's Close must not run twice, so cancel and return share one.
	var once sync.Once
	closeOnce := func() { once.Do(func() { r.Close() }) }
	defer closeOnce()
	stop := context.AfterFunc(ctx, closeOnce)
	defer stop()
	w, err := to.Create(ctx, dir, rel, size)
	if err != nil {
		return err
	}
	// 256 KiB chunks, not io.Copy's 32 KiB: fewer writes and progress calls per file.
	_, err = io.CopyBuffer(&counter{ctx: ctx, w: w, progress: progress}, r, make([]byte, 256<<10))
	if ctx.Err() != nil {
		err = ctx.Err() // not the closed reader's error
	}
	if err != nil {
		w.Abort()
		return err
	}
	return w.Commit()
}

// counter passes writes through, reporting each and stopping at cancel.
type counter struct {
	ctx      context.Context
	w        io.Writer
	progress func(int)
}

func (c *counter) Write(b []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := c.w.Write(b)
	c.progress(n)
	return n, err
}

// newID returns an opaque random ID for a task or a question.
func newID() string {
	var b [16]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
