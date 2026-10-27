//go:build !windows

package sshx

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// testAgent serves an in-memory agent holding one fresh key at a socket
// named by SSH_AUTH_SOCK for the test, and returns that key.
func testAgent(t *testing.T) ssh.PublicKey {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	kr := agent.NewKeyring()
	if err := kr.Add(agent.AddedKey{PrivateKey: priv}); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "hbag") // short: socket paths are capped near 104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "a")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { agent.ServeAgent(kr, c); c.Close() }()
		}
	}()
	t.Setenv("SSH_AUTH_SOCK", sock)
	pub, _ := ssh.NewPublicKey(priv.Public())
	return pub
}

func TestAgentSignsTheLogin(t *testing.T) {
	key := testAgent(t)
	s := newTestServer(t, serverOpts{authorized: []ssh.PublicKey{key}})
	d := &Dialer{KnownHostsPath: writeKnownHosts(t, s.knownLine(t))}
	c, err := d.Dial(context.Background(), Target{Key: "k", Hops: []Hop{{Host: s.host, Port: s.port, User: "u", Auth: Auth{Agent: true}}}})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}

func TestAgentAuthWithoutAgentSaysSo(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	s := newTestServer(t, serverOpts{})
	d := &Dialer{KnownHostsPath: writeKnownHosts(t, s.knownLine(t))}
	_, err := d.Dial(context.Background(), Target{Key: "k", Hops: []Hop{{Host: s.host, Port: s.port, User: "u", Auth: Auth{Agent: true}}}})
	if err == nil || !strings.Contains(err.Error(), "agent") || s.conns.Load() != 0 {
		t.Fatalf("err = %v, conns = %d", err, s.conns.Load())
	}
}

// With ForwardAgent, two shells on one connection both reach the local
// agent through the one handler registered at dial time; without it the
// server's agent channel is refused (R50).
func TestAgentForwardingOnlyWhenEnabled(t *testing.T) {
	key := testAgent(t)
	s := newTestServer(t, serverOpts{})
	d := &Dialer{KnownHostsPath: writeKnownHosts(t, s.knownLine(t))}
	list := func(c *Client) string {
		sess, err := c.NewSession()
		if err != nil {
			t.Error(err)
			return ""
		}
		defer sess.Close()
		if err := agent.RequestAgentForwarding(sess); err != nil {
			t.Error(err)
		}
		out, err := sess.Output("ssh-add -l")
		if err != nil {
			t.Error(err)
		}
		return string(out)
	}

	hop := s.hop()
	hop.ForwardAgent = true
	c, err := d.Dial(context.Background(), Target{Key: "k", Hops: []Hop{hop}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			if got := list(c); got != ssh.FingerprintSHA256(key)+"\n" {
				t.Errorf("forwarded agent listed %q", got)
			}
		})
	}
	wg.Wait()

	off, err := d.Dial(context.Background(), Target{Key: "k2", Hops: []Hop{s.hop()}})
	if err != nil {
		t.Fatal(err)
	}
	defer off.Close()
	if got := list(off); !strings.HasPrefix(got, "refused") {
		t.Fatalf("without forwarding the server reached the agent: %q", got)
	}
}
