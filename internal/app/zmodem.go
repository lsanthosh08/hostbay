package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/wailsapp/wails/v3/pkg/application"

	"hostbay/internal/terminal"
	"hostbay/internal/transfer"
)

// ZmodemStreamName is the GoStream a Zmodem transfer runs over (R43). When
// `sz` or `rz` starts in an SSH terminal, ZmodemEvent(ptyID) fires with the
// direction; the page opens this stream, sends the ptyID as its first frame
// and runs zmodem.js over it. Every later frame starts with a type byte:
//
//	zmRemote both ways: raw terminal bytes from the server, or for it
//	zmReply  Go to page: a zmodemReply answering zmSave or zmDone
//	zmSave   page to Go: a zmodemSave, to start saving an offered file
//	zmData   page to Go: the next bytes of that file
//	zmDone   page to Go: the file is complete; move it into place
//	zmEnd    page to Go: the transfer is over
//
// After zmEnd, Go sends what the server printed that the page has not had
// yet, then closes the stream. The page shows, as terminal text, the bytes
// zmodem.js handed back after the session and every zmRemote frame after
// that, so the shell's prompt is neither lost nor shown twice. A stream that
// closes without zmEnd cancels the transfer at the server and deletes the
// file being saved.
const ZmodemStreamName = "zmodem"

const (
	zmRemote = 0
	zmReply  = 1
	zmSave   = 1
	zmData   = 2
	zmDone   = 3
	zmEnd    = 4
)

// zmodemReadMax caps one ZmodemReadFile.
const zmodemReadMax = 1 << 20

// ZmodemService owns every file a Zmodem transfer touches: a download saves
// only into the folder the user picked in the native dialog, under names
// checked here, by temp file then rename; an upload reads only the files the
// user picked. The page never names a path.
type ZmodemService struct {
	terms *terminal.Registry
	emit  func(name string, data any)
	pick  func(folder bool) ([]string, error) // the native dialog

	mu    sync.Mutex
	xfers map[string]*zmodemXfer // by terminal ID
}

type zmodemXfer struct {
	z      *terminal.Zmodem
	folder string
	files  []string
}

// ZmodemFile is one file the user picked to send.
type ZmodemFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

type zmodemSave struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Conflict string `json:"conflict"` // "", or "overwrite" or "rename" once the user chose
}

// zmodemReply answers zmSave with the name the file will have, or with
// exists when a file of that name is there and the user must choose
// overwrite, rename or skip (KTD17); it answers zmDone with the saved name.
type zmodemReply struct {
	Name   string `json:"name,omitempty"`
	Exists bool   `json:"exists,omitempty"`
	Error  string `json:"error,omitempty"`
}

func NewZmodemService(conns *ConnService) *ZmodemService {
	s := &ZmodemService{terms: conns.terms, emit: emitEvent, pick: pickNative, xfers: map[string]*zmodemXfer{}}
	conns.terms.OnZmodem(func(id, direction string) { s.emit(ZmodemEvent(id), direction) })
	return s
}

func pickNative(folder bool) ([]string, error) {
	d := application.Get().Dialog.OpenFile().CanChooseFiles(!folder).CanChooseDirectories(folder).CanCreateDirectories(folder)
	if !folder {
		return d.SetTitle("Send with Zmodem").PromptForMultipleSelection()
	}
	p, err := d.SetTitle("Save Zmodem downloads in").PromptForSingleSelection()
	if err != nil || p == "" {
		return nil, err
	}
	return []string{p}, nil
}

// ZmodemChooseFolder asks for the folder a download saves into and returns
// it, or "" when the user cancelled.
func (s *ZmodemService) ZmodemChooseFolder(ptyID string) (string, error) {
	x, err := s.xfer(ptyID)
	if err != nil {
		return "", err
	}
	paths, err := s.pick(true)
	if err != nil || len(paths) == 0 {
		return "", err
	}
	s.mu.Lock()
	x.folder = paths[0]
	s.mu.Unlock()
	return paths[0], nil
}

// ZmodemChooseFiles asks which files an upload sends and returns them in
// order, or none when the user cancelled. ZmodemReadFile reads them by index.
func (s *ZmodemService) ZmodemChooseFiles(ptyID string) ([]ZmodemFile, error) {
	x, err := s.xfer(ptyID)
	if err != nil {
		return nil, err
	}
	paths, err := s.pick(false)
	if err != nil {
		return nil, err
	}
	out := make([]ZmodemFile, 0, len(paths))
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("%s is not a file", filepath.Base(p))
		}
		out = append(out, ZmodemFile{Name: filepath.Base(p), Size: fi.Size()})
	}
	s.mu.Lock()
	x.files = paths
	s.mu.Unlock()
	return out, nil
}

// ZmodemReadFile returns up to length bytes from offset of the index'th file
// ZmodemChooseFiles returned; fewer at its end, none past it.
func (s *ZmodemService) ZmodemReadFile(ptyID string, index int, offset int64, length int) ([]byte, error) {
	x, err := s.xfer(ptyID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	files := x.files
	s.mu.Unlock()
	if index < 0 || index >= len(files) || offset < 0 || length < 0 {
		return nil, errors.New("no such chosen file")
	}
	f, err := os.Open(files[index])
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b := make([]byte, min(length, zmodemReadMax))
	n, err := f.ReadAt(b, offset)
	if n < len(b) && err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return b[:n], nil
}

// ZmodemCancel stops the terminal's transfer at the server and returns the
// terminal to text.
func (s *ZmodemService) ZmodemCancel(ptyID string) error {
	z, err := s.terms.Zmodem(ptyID)
	if err != nil {
		return err
	}
	z.Abort()
	return nil
}

// xfer is the state of the terminal's transfer in progress, dropped when
// that transfer ends.
func (s *ZmodemService) xfer(ptyID string) (*zmodemXfer, error) {
	z, err := s.terms.Zmodem(ptyID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if x := s.xfers[ptyID]; x != nil && x.z == z {
		return x, nil
	}
	x := &zmodemXfer{z: z}
	s.xfers[ptyID] = x
	go func() {
		<-z.Done()
		s.mu.Lock()
		if s.xfers[ptyID] == x {
			delete(s.xfers, ptyID)
		}
		s.mu.Unlock()
	}()
	return x, nil
}

// zmodemConn is the part of *application.StreamConn the handler uses.
type zmodemConn interface {
	Receive() ([]byte, error)
	Send([]byte) error
	Close() error
	Context() context.Context
}

// ServeZmodemStream is the ZmodemStreamName handler.
func ServeZmodemStream(s *ZmodemService) application.StreamHandler {
	return func(c *application.StreamConn) { s.serve(c) }
}

func (s *ZmodemService) serve(c zmodemConn) {
	defer c.Close()
	id, err := c.Receive()
	if err != nil {
		return
	}
	x, err := s.xfer(string(id))
	if err != nil {
		return
	}
	z, ctx := x.z, c.Context()
	var ending atomic.Bool
	pumped := make(chan struct{})
	go func() {
		defer close(pumped)
		for {
			b, err := z.Read(ctx)
			if err != nil || c.Send(append([]byte{zmRemote}, b...)) != nil {
				if !ending.Load() {
					c.Close() // ended by the idle guard, ZmodemCancel or the terminal closing
				}
				return
			}
		}
	}()

	var (
		w       transfer.Writer // the file being saved
		name    string
		failed  error // a write to it failed
		ended   bool
		replyTo = func(r zmodemReply) error {
			b, _ := json.Marshal(r)
			return c.Send(append([]byte{zmReply}, b...))
		}
	)
	defer func() {
		if w != nil {
			w.Abort()
		}
		if !ended {
			z.Abort()
		}
	}()
	for {
		f, err := c.Receive()
		if err != nil {
			return
		}
		if len(f) == 0 {
			continue
		}
		switch f[0] {
		case zmRemote:
			if z.Write(f[1:]) != nil {
				return
			}
		case zmSave:
			if w != nil { // the page moved on without zmDone
				w.Abort()
				w = nil
			}
			var r zmodemReply
			var req zmodemSave
			if err := json.Unmarshal(f[1:], &req); err != nil {
				r = zmodemReply{Error: err.Error()}
			} else {
				w, r = s.create(ctx, x, req)
			}
			name, failed = r.Name, nil
			if replyTo(r) != nil {
				return
			}
		case zmData:
			if w != nil && failed == nil {
				if _, err := w.Write(f[1:]); err != nil {
					failed = err
				}
			}
		case zmDone:
			r := zmodemReply{Name: name}
			switch {
			case w == nil:
				r = zmodemReply{Error: "no file is being saved"}
			case failed != nil:
				w.Abort()
				r = zmodemReply{Error: failed.Error()}
			default:
				if err := w.Commit(); err != nil {
					r = zmodemReply{Error: err.Error()}
				}
			}
			w, failed = nil, nil
			if replyTo(r) != nil {
				return
			}
		case zmEnd:
			ending.Store(true)
			ended = true
			rest := z.End()
			<-pumped // so rest follows the frames already taken
			if len(rest) > 0 {
				c.Send(append([]byte{zmRemote}, rest...))
			}
			return
		}
	}
}

// create opens the file a zmSave names, inside the chosen folder.
func (s *ZmodemService) create(ctx context.Context, x *zmodemXfer, req zmodemSave) (transfer.Writer, zmodemReply) {
	s.mu.Lock()
	dir := x.folder
	s.mu.Unlock()
	if dir == "" {
		return nil, zmodemReply{Error: "choose a folder to save into first"}
	}
	name := req.Name
	// A plain file name only, so a hostile sz cannot write outside the folder.
	if strings.ContainsAny(name, `/\`) || name == "." || !filepath.IsLocal(name) {
		return nil, zmodemReply{Error: fmt.Sprintf("refused the file name %q: the server may only send plain file names", name)}
	}
	if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
		switch req.Conflict {
		case "overwrite":
		case "rename":
			if name, err = freeLocalName(dir, name); err != nil {
				return nil, zmodemReply{Error: err.Error()}
			}
		default:
			return nil, zmodemReply{Name: name, Exists: true}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, zmodemReply{Error: err.Error()}
	}
	w, err := transfer.Local{}.Create(ctx, filepath.ToSlash(dir), name, req.Size)
	if err != nil {
		return nil, zmodemReply{Error: err.Error()}
	}
	return w, zmodemReply{Name: name}
}

// freeLocalName returns name as "name (1).ext", or the first higher number
// free in dir, as the transfer engine renames.
func freeLocalName(dir, name string) (string, error) {
	ext := filepath.Ext(name)
	if ext == name { // a dotfile such as .bashrc has no extension
		ext = ""
	}
	stem := strings.TrimSuffix(name, ext)
	for n := 1; ; n++ {
		cand := fmt.Sprintf("%s (%d)%s", stem, n, ext)
		_, err := os.Lstat(filepath.Join(dir, cand))
		if errors.Is(err, fs.ErrNotExist) {
			return cand, nil
		}
		if err != nil {
			return "", err
		}
	}
}
