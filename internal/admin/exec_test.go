package admin

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// execServer answers "exec": "split" writes to both streams and exits 3,
// "signal" ends by signal, "hang" never ends, anything else exits 0 silently.
func execServer(t *testing.T) *ssh.Client {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	hk, _ := ssh.NewSignerFromKey(priv)
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(hk)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, chans, reqs, err := ssh.NewServerConn(nc, cfg)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for nch := range chans {
					go serveExec(nch)
				}
			}()
		}
	}()
	c, err := ssh.Dial("tcp", ln.Addr().String(), &ssh.ClientConfig{User: "u", HostKeyCallback: ssh.InsecureIgnoreHostKey()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func serveExec(nch ssh.NewChannel) {
	ch, reqs, err := nch.Accept()
	if err != nil {
		return
	}
	defer ch.Close()
	for req := range reqs {
		if req.Type != "exec" {
			req.Reply(false, nil)
			continue
		}
		var p struct{ Cmd string }
		ssh.Unmarshal(req.Payload, &p)
		req.Reply(true, nil)
		switch p.Cmd {
		case "split":
			io.WriteString(ch, "out\n")
			io.WriteString(ch.Stderr(), "err\n")
			ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{3}))
		case "signal":
			ch.SendRequest("exit-signal", false, ssh.Marshal(struct {
				Signal     string
				CoreDumped bool
				Msg, Lang  string
			}{Signal: "KILL"}))
		case "hang":
			for range reqs {
			}
		default:
			ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
		}
		return
	}
}

func TestExecKeepsStdoutStderrAndExitCodeApart(t *testing.T) {
	c := execServer(t)
	r, err := Exec(context.Background(), c, "split")
	if err != nil {
		t.Fatal(err)
	}
	if r.Stdout != "out\n" || r.Stderr != "err\n" || r.Code == nil || *r.Code != 3 {
		t.Fatalf("result = %+v (code %v)", r, r.Code)
	}

	r, err = Exec(context.Background(), c, "true")
	if err != nil || r.Code == nil || *r.Code != 0 {
		t.Fatalf("success: %+v, %v", r, err)
	}
}

// As in exec.rs, a command killed by a signal has no exit code.
func TestExecKilledBySignalHasNoCode(t *testing.T) {
	r, err := Exec(context.Background(), execServer(t), "signal")
	if err != nil || r.Code != nil {
		t.Fatalf("result = %+v, %v", r, err)
	}
}

func TestExecGivesUpAtTheDeadline(t *testing.T) {
	c := execServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Exec(ctx, c, "hang")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(start))
	}
	if _, err := Exec(context.Background(), c, "true"); err != nil {
		t.Fatalf("connection unusable after a timeout: %v", err)
	}
}
