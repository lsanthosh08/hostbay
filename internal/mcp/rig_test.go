package mcp

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"hostbay/internal/agentexec"
	"hostbay/internal/mcp/mcptest"
	"hostbay/internal/store"
)

// Sample secrets no response may hold.
const (
	samplePassword = "Pw-sample-7731"
	samplePassKey  = "-----BEGIN OPENSSH PRIVATE KEY-----sample"
)

// fakeHosts stands in for the app's leases: saved SSH hosts h1 and h2.
// Its commands print a saved password, which must never reach a client.
type fakeHosts struct {
	mu   sync.Mutex
	ran  []string
	fail map[string]error
}

func (f *fakeHosts) Label(id string) (string, error) {
	if id == "h1" || id == "h2" {
		return "web-" + id, nil
	}
	return "", fmt.Errorf("%q is not a saved SSH host", id)
}
func (f *fakeHosts) Secrets(string) []string { return []string{samplePassword, samplePassKey} }
func (f *fakeHosts) Run(_ context.Context, host, cmd string, stdout, stderr io.Writer) (int, error) {
	f.mu.Lock()
	f.ran = append(f.ran, host+": "+cmd)
	err := f.fail[host]
	f.mu.Unlock()
	if err != nil {
		return 0, err
	}
	fmt.Fprintln(stdout, strings.TrimPrefix(cmd, "echo "))
	fmt.Fprintln(stderr, "password was "+samplePassword)
	return 0, nil
}
func (f *fakeHosts) List(context.Context, string, string) ([]agentexec.RemoteEntry, error) {
	return []agentexec.RemoteEntry{{Name: "etc", Path: "/etc", IsDir: true}}, nil
}
func (f *fakeHosts) Upload(_ context.Context, _, _ string, r io.Reader) error {
	_, err := io.Copy(io.Discard, r)
	return err
}
func (f *fakeHosts) Download(_ context.Context, _, _ string, w io.Writer) error {
	_, err := io.WriteString(w, "remote bytes")
	return err
}

func (f *fakeHosts) runs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ran...)
}

type rig struct {
	st     *store.Store
	srv    *Server
	hosts  *fakeHosts
	port   int
	token  string
	tokID  string
	url    string
	server string // home, where the endpoint file is
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// newRig starts a server in a throwaway home with one token and two saved
// SSH hosts.
func newRig(t *testing.T) *rig {
	t.Helper()
	home := t.TempDir()
	st, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	err = st.Edit(func(d *store.Data) error {
		d.AddHost(store.Host{Meta: store.Meta{ID: "h1"}, Alias: "web", Hostname: "10.0.0.5", Port: 22, User: "root", Password: "sealed-v1:" + samplePassword, IdentityFile: "/Users/me/.ssh/id_secret"})
		d.AddHost(store.Host{Meta: store.Meta{ID: "h2"}, Alias: "db", Hostname: "10.0.0.6", Port: 22, User: "root"})
		tel := "telnet"
		d.AddHost(store.Host{Meta: store.Meta{ID: "t1"}, Alias: "switch", Hostname: "10.0.0.7", Port: 23, Kind: &tel})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeHosts{fail: map[string]error{}}
	tokens := NewTokens(st)
	tok, secret, err := tokens.Create("claude-code")
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{St: st, Jobs: agentexec.NewHostJobs(agentexec.NewApprovals(), f), Tokens: tokens, Home: home}
	port := freePort(t)
	if err := srv.Start(port); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)
	return &rig{st: st, srv: srv, hosts: f, port: port, token: secret, tokID: tok.ID, url: "http://127.0.0.1:" + strconv.Itoa(port) + Path, server: home}
}

func (r *rig) session(t *testing.T, version string) *sdk.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cs, err := mcptest.HTTP(ctx, r.url, r.token, version)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// approveNext answers the next card in the tray once it shows.
func (r *rig) approveNext(t *testing.T, allow bool) agentexec.Pending {
	t.Helper()
	for range 400 {
		if l := r.srv.Jobs.Approvals.List(); len(l) > 0 {
			if err := r.srv.Jobs.Approvals.Answer(l[0].ID, allow); err != nil {
				t.Fatal(err)
			}
			return l[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no card showed")
	return agentexec.Pending{}
}

// call runs a tool and returns its text, failing the test on a protocol error.
func call(t *testing.T, cs *sdk.ClientSession, name string, args map[string]any) (*sdk.CallToolResult, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return res, b.String()
}

// noSecrets fails if text holds a sample secret.
func noSecrets(t *testing.T, where, text string) {
	t.Helper()
	for _, s := range []string{samplePassword, samplePassKey, "id_secret"} {
		if strings.Contains(text, s) {
			t.Fatalf("%s holds %q: %s", where, s, text)
		}
	}
}
