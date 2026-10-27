package k8s

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// execServer speaks the exec channel protocol of the given subprotocols:
// it records the first resize, echoes stdin on stdout, and on "exit\n"
// sends a Success status and closes.
func execServer(t *testing.T, protocols []string, resized chan<- string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: protocols})
		if err != nil {
			return
		}
		defer ws.CloseNow()
		ctx := r.Context()
		for {
			_, msg, err := ws.Read(ctx)
			if err != nil || len(msg) == 0 {
				return
			}
			switch msg[0] {
			case 4:
				select {
				case resized <- string(msg[1:]):
				default:
				}
			case 0:
				if string(msg[1:]) == "exit\n" {
					ws.Write(ctx, websocket.MessageBinary, append([]byte{3}, `{"status":"Success"}`...))
					ws.Close(websocket.StatusNormalClosure, "")
					return
				}
				ws.Write(ctx, websocket.MessageBinary, append([]byte{1}, msg[1:]...))
			}
		}
	}
}

func TestExecRunsTheArgumentListOverTheChannelProtocol(t *testing.T) {
	for _, protos := range [][]string{{"v5.channel.k8s.io"}, {"v4.channel.k8s.io"}} {
		t.Run(protos[0], func(t *testing.T) {
			resized := make(chan string, 1)
			f, c := newFake(t, execServer(t, protos, resized))
			s, err := c.Exec(context.Background(), "apps", "web-1", "web", []string{"/bin/sh", "-i"}, 120, 40)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			r := f.reqs[0]
			q := r.URL.Query()
			if r.URL.Path != "/api/v1/namespaces/apps/pods/web-1/exec" || strings.Join(q["command"], " ") != "/bin/sh -i" ||
				q.Get("container") != "web" || q.Get("tty") != "true" || q.Get("stdin") != "true" || r.Header.Get("Authorization") != "Bearer tok-123" {
				t.Fatalf("asked %s?%s", r.URL.Path, r.URL.RawQuery)
			}
			select {
			case size := <-resized:
				var sz struct{ Width, Height int }
				if json.Unmarshal([]byte(size), &sz); sz.Width != 120 || sz.Height != 40 {
					t.Fatalf("resize = %s", size)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no resize was sent")
			}
			if _, err := s.Write([]byte("echo ok\n")); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, 64)
			n, err := s.Read(buf)
			if err != nil || string(buf[:n]) != "echo ok\n" {
				t.Fatalf("read %q, %v", buf[:n], err)
			}
			s.Write([]byte("exit\n"))
			if rest, _ := io.ReadAll(s); len(rest) != 0 || s.EndReason() != "session closed" {
				t.Fatalf("after exit: %q, %s", rest, s.EndReason())
			}
		})
	}
}

func TestExecRefusesBadNamesAndAnEmptyCommand(t *testing.T) {
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	if _, err := c.Exec(ctx, "apps", "a;rm -rf /", "web", []string{"sh"}, 80, 24); err == nil {
		t.Error("the pod name a;rm -rf / was accepted")
	}
	if _, err := c.Exec(ctx, "apps", "web-1", "web", nil, 80, 24); err == nil {
		t.Error("an empty command was accepted")
	}
	if len(f.seen()) != 0 {
		t.Fatalf("requests reached the server: %v", f.seen())
	}
}
