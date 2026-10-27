//go:build integration

package database

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"hostbay/internal/dockertest"
	"hostbay/internal/sshx"
)

// chainClient starts a two-hop SSH chain on network, the bastion
// published on localhost and the hop reachable only from it, and returns
// a client at the hop's end.
func chainClient(t *testing.T, network string) *sshx.Client {
	t.Helper()
	bastion, hop := dockertest.Name("hostbay-bastion"), dockertest.Name("hostbay-hop")
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	block, _ := ssh.MarshalPrivateKey(priv, "it")
	pub, _ := ssh.NewPublicKey(priv.Public())
	authorized := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))
	dockertest.Start(t, bastion, "--network", network, "-e", "PUB="+authorized, "-p", "127.0.0.1::22", "alpine:3.20", "sh", "-c", sshdScript)
	dockertest.Start(t, hop, "--network", network, "-e", "PUB="+authorized, "alpine:3.20", "sh", "-c", sshdScript)
	for _, name := range []string{bastion, hop} {
		dockertest.WaitExec(t, name, 2*time.Minute, "test", "-f", "/run/ready")
	}
	bport := dockertest.Port(t, bastion, "22")
	var known []string
	for name, addr := range map[string]string{bastion: net.JoinHostPort("127.0.0.1", strconv.Itoa(bport)), hop: net.JoinHostPort(hop, "22")} {
		k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(dockertest.Docker(t, "exec", name, "cat", "/etc/ssh/ssh_host_ed25519_key.pub")))
		if err != nil {
			t.Fatal(err)
		}
		known = append(known, knownhosts.Line([]string{knownhosts.Normalize(addr)}, k))
	}
	kh := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(kh, []byte(strings.Join(known, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	auth := sshx.Auth{Key: pem.EncodeToMemory(block)}
	d := &sshx.Dialer{KnownHostsPath: kh}
	var c *sshx.Client
	var err error
	for try := 0; ; try++ { // sshd may still be starting after /run/ready
		c, err = d.Dial(context.Background(), sshx.Target{Key: "it", Hops: []sshx.Hop{
			{Host: "127.0.0.1", Port: bport, User: "u", Auth: auth},
			{Host: hop, Port: 22, User: "u", Auth: auth},
		}})
		if err == nil || try == 30 {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// chainDial dials only want, through c, and records any other address a
// driver asked for, as the app's allowlisted dial function refuses it.
type chainDial struct {
	c    *sshx.Client
	want string

	mu      sync.Mutex
	dials   int
	refused []string
}

func (d *chainDial) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if addr != d.want {
		d.refused = append(d.refused, addr)
		return nil, fmt.Errorf("refused %s", addr)
	}
	d.dials++
	return d.c.DialContext(ctx, "tcp", addr)
}

func (d *chainDial) check(t *testing.T) {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dials == 0 || len(d.refused) > 0 {
		t.Fatalf("%d dials through the chain; refused %q", d.dials, d.refused)
	}
}

// allRows runs text and reads every page of its first result set.
func allRows(t *testing.T, e Engine, text string) [][]Cell {
	t.Helper()
	ctx := context.Background()
	r := NewRegistry(func(State) {})
	id := r.Start(ctx, "tab", func(ctx context.Context) (Results, error) { return e.Query(ctx, text) })
	var rows [][]Cell
	for {
		p, err := r.Next(ctx, id)
		if err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		if p.Result == 0 {
			rows = append(rows, p.Rows...)
		}
		if p.Done {
			return rows
		}
	}
}

// A Redis server reachable only from the far end of the chain lists keys
// by SCAN, runs commands and edits a value of each type through it.
func TestRedisThroughATwoHopChain(t *testing.T) {
	dockertest.Require(t)
	network := dockertest.Network(t)
	c := chainClient(t, network)
	name := dockertest.Name("hostbay-redis")
	const pw = "it-Redis-Pass-5!" // a throwaway container's password
	dockertest.Start(t, name, "--network", network, "redis:7-alpine", "redis-server", "--requirepass", pw)
	dockertest.WaitExec(t, name, time.Minute, "redis-cli", "-a", pw, "--no-auth-warning", "ping")
	dial := &chainDial{c: c, want: net.JoinHostPort(name, "6379")}
	ctx := context.Background()
	e, err := OpenRedis(ctx, RedisConfig{Host: name, Port: 6379, Password: pw, DB: 2, TLS: "none", Dial: dial.dial})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	var seed strings.Builder
	for i := range 250 {
		fmt.Fprintf(&seed, "SET k%03d v%d\n", i, i)
	}
	seed.WriteString("HSET h f1 a f2 b\nRPUSH l x y z\nSADD s m1 m2\nZADD z 1 one 2 two\n")
	if _, err := e.Query(ctx, seed.String()); err != nil {
		t.Fatal(err)
	}
	first, more, err := e.Children(ctx, nil, 0)
	if err != nil || len(first) != TreePage || !more {
		t.Fatalf("first page: %d keys, more %v, %v", len(first), more, err)
	}
	rest, more, err := e.Children(ctx, nil, TreePage)
	if err != nil || len(first)+len(rest) != 254 || more {
		t.Fatalf("second page: %d keys, more %v, %v", len(rest), more, err)
	}
	if rows := allRows(t, e, "GET k007"); len(rows) != 1 || rows[0][1].Text != "v7" {
		t.Fatalf("GET = %+v", rows)
	}

	ed := e.(RowEditor)
	s := func(v string) *string { return &v }
	for _, tc := range []struct {
		key, col, row, setCol, val, check, want string
	}{
		{"k007", "key", "k007", "value", "seven", "GET k007", "seven"},
		{"h", "field", "f1", "value", "A", "HGET h f1", "A"},
		{"l", "index", "1", "value", "Y", "LINDEX l 1", "Y"},
		{"s", "member", "m1", "member", "m3", "SISMEMBER s m3", "1"},
		{"z", "member", "two", "score", "5", "ZSCORE z two", "5"},
	} {
		tb, err := ed.EditableTable(ctx, []string{tc.key})
		if err != nil || tb == nil || tb.Key[0] != tc.col {
			t.Fatalf("%s: table %+v, %v", tc.key, tb, err)
		}
		if rows := allRows(t, e, ed.SelectTable([]string{tc.key})); len(rows) == 0 {
			t.Fatalf("%s shows no rows", tc.key)
		}
		if err := ed.EditRow(ctx, RowEdit{Path: []string{tc.key}, Key: map[string]*string{tc.col: s(tc.row)}, Set: map[string]*string{tc.setCol: s(tc.val)}}); err != nil {
			t.Fatalf("%s: %v", tc.key, err)
		}
		if rows := allRows(t, e, tc.check); rows[0][len(rows[0])-1].Text != tc.want {
			t.Fatalf("%s: %s = %+v", tc.key, tc.check, rows)
		}
	}
	if err := ed.EditRow(ctx, RowEdit{Path: []string{"h"}, Key: map[string]*string{"field": s("gone")}, Set: map[string]*string{"value": s("x")}}); err == nil {
		t.Fatal("an edit of a deleted hash field was saved")
	}
	if rows := allRows(t, e, "HEXISTS h gone"); rows[0][0].Text != "0" {
		t.Fatal("the edit recreated a deleted hash field")
	}
	if n := dockertest.Docker(t, "exec", name, "redis-cli", "-a", pw, "--no-auth-warning", "-n", "2", "DBSIZE"); n != "254" {
		t.Fatalf("database 2 holds %s keys", n)
	}
	dial.check(t)
}
