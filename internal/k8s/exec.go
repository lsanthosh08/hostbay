package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Channel numbers of the exec stream protocol (v4.channel.k8s.io; v5 adds
// a close channel Hostbay does not need).
const (
	chStdin  = 0
	chStdout = 1
	chStderr = 2
	chError  = 3
	chResize = 4
)

// Exec runs argv in a pod's container on a terminal, over the WebSocket
// channel protocol (v5.channel.k8s.io, else v4). argv is an argument list
// run as is, never through a shell, so nothing the page sends is parsed as
// a command line. The session is a terminal transport.
func (c *Client) Exec(ctx context.Context, ns, pod, container string, argv []string, cols, rows int) (*ExecSession, error) {
	k, err := target("pods", ns, pod)
	if err != nil {
		return nil, err
	}
	if err := checkLabel("container", container); err != nil {
		return nil, err
	}
	if len(argv) == 0 || len(argv) > 64 || argv[0] == "" {
		return nil, errors.New("choose the command to run")
	}
	for _, a := range argv {
		if strings.ContainsRune(a, 0) || len(a) > 4096 {
			return nil, errors.New("the command has an argument that cannot be sent")
		}
	}
	q := url.Values{"container": {container}, "stdin": {"true"}, "stdout": {"true"}, "tty": {"true"}, "command": argv}
	h := http.Header{}
	if c.token != "" {
		h.Set("Authorization", "Bearer "+c.token)
	}
	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ws, resp, err := websocket.Dial(dctx, c.endpoint(k.path(ns, pod)+"/exec", q).String(), &websocket.DialOptions{
		HTTPClient: c.hc, HTTPHeader: h, Subprotocols: []string{"v5.channel.k8s.io", "v4.channel.k8s.io"},
	})
	if err != nil {
		if resp != nil && resp.StatusCode >= 300 {
			return nil, statusError(resp)
		}
		return nil, err
	}
	if p := ws.Subprotocol(); p != "v5.channel.k8s.io" && p != "v4.channel.k8s.io" {
		ws.Close(websocket.StatusProtocolError, "")
		return nil, fmt.Errorf("the cluster offered exec protocol %q, which Hostbay does not speak", p)
	}
	ws.SetReadLimit(1 << 20)
	sctx, stop := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	s := &ExecSession{ws: ws, ctx: sctx, stop: stop, out: pr}
	go s.read(pw)
	if err := s.Resize(cols, rows); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// ExecSession is a pod terminal.
type ExecSession struct {
	ws   *websocket.Conn
	ctx  context.Context
	stop context.CancelFunc
	out  *io.PipeReader

	mu     sync.Mutex
	reason string
}

// read copies stdout and stderr to pw until the stream ends. A failure
// status on the error channel is shown in the terminal and becomes the end
// reason.
func (s *ExecSession) read(pw *io.PipeWriter) {
	defer pw.Close()
	for {
		_, msg, err := s.ws.Read(s.ctx)
		if err != nil {
			if websocket.CloseStatus(err) == -1 && s.ctx.Err() == nil {
				s.setReason("connection lost")
			}
			return
		}
		if len(msg) == 0 {
			continue
		}
		switch msg[0] {
		case chStdout, chStderr:
			if _, err := pw.Write(msg[1:]); err != nil {
				return
			}
		case chError:
			var st struct {
				Status  string `json:"status"`
				Message string `json:"message"`
			}
			if json.Unmarshal(msg[1:], &st) == nil && st.Status != "Success" && st.Message != "" {
				s.setReason(st.Message)
				pw.Write([]byte("\r\n" + st.Message + "\r\n"))
			}
		}
	}
}

func (s *ExecSession) setReason(r string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reason == "" {
		s.reason = r
	}
}

func (s *ExecSession) Read(b []byte) (int, error) { return s.out.Read(b) }

func (s *ExecSession) Write(b []byte) (int, error) {
	if err := s.ws.Write(s.ctx, websocket.MessageBinary, append([]byte{chStdin}, b...)); err != nil {
		return 0, err
	}
	return len(b), nil
}

// Resize tells the container's terminal its size.
func (s *ExecSession) Resize(cols, rows int) error {
	b, _ := json.Marshal(struct{ Width, Height int }{cols, rows})
	return s.ws.Write(s.ctx, websocket.MessageBinary, append([]byte{chResize}, b...))
}

func (s *ExecSession) Close() error {
	s.ws.Close(websocket.StatusNormalClosure, "")
	s.stop()
	return s.out.Close()
}

// EndReason is why the terminal ended: the command's failure, a lost
// connection, or "session closed".
func (s *ExecSession) EndReason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reason == "" {
		return "session closed"
	}
	return s.reason
}
