//go:build integration

package app

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"hostbay/internal/agentexec"
	"hostbay/internal/dockertest"
	"hostbay/internal/mcp"
	"hostbay/internal/mcp/mcptest"
	"hostbay/internal/secrets"
	"hostbay/internal/sshx"
	"hostbay/internal/tlstrust"
)

const mcpSSHD = `apk add --no-cache openssh-server >/dev/null && ssh-keygen -A >/dev/null &&
adduser -D u && echo "u:$PW" | chpasswd && touch /run/ready &&
exec /usr/sbin/sshd -D -e -o PasswordAuthentication=yes`

// Against real OpenSSH, an approved `uname -a` runs on the saved host's
// lease, from a client over HTTP and from a stdio client through the relay.
func TestDockerMCPUname(t *testing.T) {
	dockertest.Require(t)
	name := dockertest.Name("hostbay-mcp")
	pw := "Mcp-It-Pass-9921"
	dockertest.Start(t, name, "-e", "PW="+pw, "-p", "127.0.0.1::22", "alpine:3.20", "sh", "-c", mcpSSHD)
	dockertest.WaitExec(t, name, 2*time.Minute, "test", "-f", "/run/ready")
	port := dockertest.Port(t, name, "22")
	key := strings.Fields(dockertest.Docker(t, "exec", name, "cat", "/etc/ssh/ssh_host_ed25519_key.pub"))
	kh := filepath.Join(t.TempDir(), "known_hosts")
	os.WriteFile(kh, []byte("[127.0.0.1]:"+strconv.Itoa(port)+" "+key[0]+" "+key[1]+"\n"), 0o600)

	hs, creds, _ := newHostService(t, fakeVerifier(secrets.Passed))
	conns := NewConnService(NewHostKeyTrust(sshx.NewKnownHostAppender(kh)), creds, sshx.NewManager(&sshx.Dialer{KnownHostsPath: kh, Timeout: 10 * time.Second}))
	auth := "password"
	h, err := hs.SaveHost(Host{Alias: "docker", Hostname: "127.0.0.1", Port: port, User: "u", AuthType: &auth, Password: &pw})
	if err != nil {
		t.Fatal(err)
	}
	ai := NewAIService(creds.st, creds.vault, creds, conns, tlstrust.New(t.TempDir()))
	ai.emit = func(string, any) {}
	s := NewMCPService(creds.st, conns, creds, ai)
	s.confirm = func(context.Context, string, string) bool { return true }
	s.gate = func(context.Context, string) error { return nil }
	var token string
	s.copy = func(v string) bool { token = v; return true }
	if err := s.SetEnabled(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	defer s.ServiceShutdown()
	if _, err := s.CreateToken(t.Context(), "it"); err != nil {
		t.Fatal(err)
	}
	approve := func() {
		for range 1000 {
			if l := s.srv.Jobs.Approvals.List(); len(l) > 0 {
				s.srv.Jobs.Approvals.Answer(l[0].ID, true)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Error("no card showed")
	}

	st, _ := s.Status()
	httpClient, err := mcptest.HTTP(t.Context(), st.Endpoint, token, mcptest.V20260728)
	if err != nil {
		t.Fatal(err)
	}
	defer httpClient.Close()

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() { mcp.Relay(context.Background(), creds.st.Home(), token, inR, outW, io.Discard); outW.Close() }()
	stdioClient, err := mcptest.IO(t.Context(), outR, inW, mcptest.V20251125)
	if err != nil {
		t.Fatal(err)
	}
	defer stdioClient.Close()

	for name, cs := range map[string]*sdk.ClientSession{"http": httpClient, "stdio": stdioClient} {
		go approve()
		_, text := mcpCall(t, cs, agentexec.ToolRun, map[string]any{"hostId": h.ID, "command": "uname -a"})
		if !strings.Contains(text, "Linux") || !strings.Contains(text, `"exitCode":0`) || strings.Contains(text, pw) {
			t.Fatalf("%s: %s", name, text)
		}
	}
}
