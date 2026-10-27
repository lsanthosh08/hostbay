//go:build integration && !windows

package sshx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"hostbay/internal/sshx/proxytest"
)

// TestDockerConnectionExtras runs U19's options against real OpenSSH that
// offers only aes128-cbc: reached through a SOCKS5 proxy, signed in with the
// local agent, its agent forwarded only when the hop asks (R49, R50, R52).
func TestDockerConnectionExtras(t *testing.T) {
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not reachable")
	}
	key := testAgent(t)
	authorized := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	var b [4]byte
	rand.Read(b[:])
	name := "hostbay-extras-" + hex.EncodeToString(b[:])
	script := strings.Replace(sshdScript, "openssh-server", "openssh-server openssh-client", 1)
	script = strings.Replace(script, "-o AllowTcpForwarding=yes", "-o AllowTcpForwarding=yes -o AllowAgentForwarding=yes -o Ciphers=aes128-cbc", 1)
	docker(t, "run", "-d", "--rm", "--name", name, "-e", "PUB="+authorized, "-p", "127.0.0.1::22", "alpine:3.20", "sh", "-c", script)
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })
	deadline := time.Now().Add(2 * time.Minute)
	for exec.Command("docker", "exec", name, "test", "-f", "/run/ready").Run() != nil {
		if time.Now().After(deadline) {
			t.Fatal("sshd never became ready")
		}
		time.Sleep(time.Second)
	}
	_, portStr, err := net.SplitHostPort(docker(t, "port", name, "22"))
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portStr)
	hk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(docker(t, "exec", name, "cat", "/etc/ssh/ssh_host_ed25519_key.pub")))
	if err != nil {
		t.Fatal(err)
	}
	p := proxytest.SOCKS5(t, "", "")
	d := &Dialer{KnownHostsPath: writeKnownHosts(t, khLine("127.0.0.1", port, hk))}
	hop := Hop{Host: "127.0.0.1", Port: port, User: "u", Auth: Auth{Agent: true},
		Proxy: &Proxy{Type: "socks5", Host: p.Host, Port: p.Port}}
	dial := func(h Hop) (*Client, error) {
		var c *Client
		var err error
		for try := 0; try < 30; try++ { // sshd may still be starting after /run/ready
			if c, err = d.Dial(context.Background(), Target{Key: "it", Hops: []Hop{h}}); err == nil || strings.Contains(err.Error(), "algorithms") {
				break
			}
			time.Sleep(time.Second)
		}
		return c, err
	}

	if _, err := dial(hop); err == nil || !strings.Contains(err.Error(), "legacy") {
		t.Fatalf("without the legacy cipher: err = %v", err)
	}
	hop.Algorithms = Algorithms{Ciphers: []string{ssh.InsecureCipherAES128CBC}}
	agentList := func(c *Client) (string, error) {
		sess, err := c.NewSession()
		if err != nil {
			return "", err
		}
		defer sess.Close()
		if c.ForwardAgent {
			if err := agentRequest(sess); err != nil {
				return "", err
			}
		}
		out, err := sess.CombinedOutput("ssh-add -l")
		return string(out), err
	}

	off, err := dial(hop)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := agentList(off); err == nil {
		t.Fatalf("without forwarding the server reached the agent: %q", out)
	}
	off.Close()

	hop.ForwardAgent = true
	on, err := dial(hop)
	if err != nil {
		t.Fatal(err)
	}
	defer on.Close()
	if out, err := agentList(on); err != nil || !strings.Contains(out, ssh.FingerprintSHA256(key)) {
		t.Fatalf("forwarded agent: %q, %v", out, err)
	}
	if len(p.Targets()) == 0 {
		t.Fatal("the proxy carried nothing")
	}
}

func agentRequest(s *ssh.Session) error { return agent.RequestAgentForwarding(s) }
