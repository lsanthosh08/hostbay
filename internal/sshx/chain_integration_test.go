//go:build integration

package sshx

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"hostbay/internal/apperr"
)

// sshdScript turns a stock Alpine container into an OpenSSH server that
// accepts $PUB for user u and allows TCP forwarding (so it can be a bastion).
const sshdScript = `apk add --no-cache openssh-server >/dev/null &&
ssh-keygen -A >/dev/null && adduser -D u && echo u:unused | chpasswd &&
mkdir -p /home/u/.ssh && echo "$PUB" > /home/u/.ssh/authorized_keys &&
chown -R u:u /home/u/.ssh && chmod 700 /home/u/.ssh && chmod 600 /home/u/.ssh/authorized_keys &&
touch /run/ready && exec /usr/sbin/sshd -D -e -o AllowTcpForwarding=yes -o PasswordAuthentication=no`

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestDockerTwoHopChain runs the chain against real OpenSSH: a bastion
// published on localhost and a target reachable only from the bastion.
func TestDockerTwoHopChain(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not reachable")
	}

	var b [4]byte
	rand.Read(b[:])
	id := hex.EncodeToString(b[:])
	network, bastion, target := "hostbay-it-"+id, "hostbay-bastion-"+id, "hostbay-target-"+id

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	block, _ := ssh.MarshalPrivateKey(priv, "it")
	pub, _ := ssh.NewPublicKey(priv.Public())
	authorized := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))

	docker(t, "network", "create", network)
	t.Cleanup(func() { exec.Command("docker", "network", "rm", network).Run() })
	for _, name := range []string{bastion, target} {
		args := []string{"run", "-d", "--rm", "--network", network, "--name", name, "-e", "PUB=" + authorized}
		if name == bastion {
			args = append(args, "-p", "127.0.0.1::22")
		}
		docker(t, append(args, "alpine:3.20", "sh", "-c", sshdScript)...)
		t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })
	}
	for _, name := range []string{bastion, target} {
		deadline := time.Now().Add(2 * time.Minute)
		for exec.Command("docker", "exec", name, "test", "-f", "/run/ready").Run() != nil {
			if time.Now().After(deadline) {
				t.Fatalf("%s never became ready", name)
			}
			time.Sleep(time.Second)
		}
	}

	_, portStr, err := net.SplitHostPort(docker(t, "port", bastion, "22"))
	if err != nil {
		t.Fatal(err)
	}
	bport, _ := strconv.Atoi(portStr)
	hostKey := func(name string) ssh.PublicKey {
		k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(docker(t, "exec", name, "cat", "/etc/ssh/ssh_host_ed25519_key.pub")))
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	auth := Auth{Key: pem.EncodeToMemory(block)}
	tgt := Target{Key: "it", Hops: []Hop{
		{Host: "127.0.0.1", Port: bport, User: "u", Auth: auth},
		{Host: target, Port: 22, User: "u", Auth: auth},
	}}
	known := []string{khLine("127.0.0.1", bport, hostKey(bastion)), khLine(target, 22, hostKey(target))}
	d := &Dialer{KnownHostsPath: writeKnownHosts(t, known...)}
	ctx := context.Background()

	var c *Client
	for try := 0; ; try++ { // sshd may still be starting after /run/ready
		if c, err = d.Dial(ctx, tgt); err == nil || try == 30 {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		t.Fatal(err)
	}
	if got := run(t, c, "echo through-the-chain"); got != "through-the-chain\n" {
		t.Fatalf("output = %q", got)
	}
	c.Close()

	// AE9: a changed bastion key is refused against hop 0.
	_, stale := seedKey(t, 30)
	d.KnownHostsPath = writeKnownHosts(t, khLine("127.0.0.1", bport, stale), known[1])
	_, err = d.Dial(ctx, tgt)
	var hk *HostKeyError
	if !errors.As(err, &hk) || hk.Hop != 0 || apperr.CodeOf(err) != apperr.HostKeyChanged {
		t.Fatalf("changed bastion key: err = %v", err)
	}

	// AE3: an unknown target key is reported against hop 1.
	d.KnownHostsPath = writeKnownHosts(t, known[0])
	if _, err = d.Dial(ctx, tgt); !errors.As(err, &hk) || hk.Hop != 1 || hk.Changed() {
		t.Fatalf("unknown target key: err = %v", err)
	}
}
