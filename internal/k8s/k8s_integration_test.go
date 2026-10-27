//go:build integration

package k8s

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"hostbay/internal/dockertest"
	"hostbay/internal/importer/kubeconfig"
	"hostbay/internal/sshx"
	"hostbay/internal/store"
	"hostbay/internal/tlstrust"
)

// sshdScript makes a stock Alpine container an OpenSSH server that accepts
// $PUB for user u and forwards TCP, so it can be the bastion.
const sshdScript = `apk add --no-cache openssh-server >/dev/null &&
ssh-keygen -A >/dev/null && adduser -D u && echo u:unused | chpasswd &&
mkdir -p /home/u/.ssh && echo "$PUB" > /home/u/.ssh/authorized_keys &&
chown -R u:u /home/u/.ssh && chmod 700 /home/u/.ssh && chmod 600 /home/u/.ssh/authorized_keys &&
touch /run/ready && exec /usr/sbin/sshd -D -e -o AllowTcpForwarding=yes -o PasswordAuthentication=no`

const workload = `apiVersion: v1
kind: ConfigMap
metadata:
  name: settings
data:
  mode: fast
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  replicas: 1
  selector:
    matchLabels: {app: web}
  template:
    metadata:
      labels: {app: web}
    spec:
      containers:
      - name: web
        image: busybox:1.36
        command: ["sh", "-c", "echo hello from web; sleep 3600"]
`

// A k3s cluster whose API server is reachable only from the bastion lists
// pods, follows a pod's log, runs echo ok through exec, applies after a
// dry run and scales a deployment, every connection through the bastion.
func TestClusterThroughTheBastion(t *testing.T) {
	dockertest.Require(t)
	t.Setenv("HOME", t.TempDir())
	network := dockertest.Network(t)
	bastion, k3s := dockertest.Name("hostbay-bastion"), dockertest.Name("hostbay-k3s")

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	block, _ := ssh.MarshalPrivateKey(priv, "it")
	pub, _ := ssh.NewPublicKey(priv.Public())
	dockertest.Start(t, bastion, "--network", network, "-e", "PUB="+strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))), "-p", "127.0.0.1::22", "alpine:3.20", "sh", "-c", sshdScript)
	// No published port: only the bastion reaches the API server.
	dockertest.Start(t, k3s, "--network", network, "--privileged", "--hostname", k3s, "rancher/k3s:v1.31.4-k3s1", "server", "--disable", "traefik", "--tls-san", k3s)
	dockertest.WaitExec(t, bastion, 2*time.Minute, "test", "-f", "/run/ready")
	dockertest.WaitExec(t, k3s, 3*time.Minute, "kubectl", "get", "--raw", "/readyz")

	// The cluster's own kubeconfig, imported as Hostbay imports one.
	cfgPath := filepath.Join(t.TempDir(), "config")
	raw := strings.ReplaceAll(dockertest.Docker(t, "exec", k3s, "cat", "/etc/rancher/k3s/k3s.yaml"), "https://127.0.0.1:6443", "https://"+k3s+":6443")
	if err := os.WriteFile(cfgPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := kubeconfig.Parse(cfgPath, "")
	if err != nil || len(res.Bundle.Hosts) != 1 {
		t.Fatalf("import: %+v, %v", res, err)
	}
	h := res.Bundle.Hosts[0]
	keyPEM, _ := base64.StdEncoding.DecodeString(res.Bundle.Keys[strings.TrimPrefix(h.IdentityFile, store.KeyRefPrefix)])
	pair, err := tls.X509KeyPair(keyPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg, err := tlstrust.Anchored([]byte(store.Deref(h.CAData)), k3s)
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg.Certificates = []tls.Certificate{pair}

	bport := dockertest.Port(t, bastion, "22")
	hk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(dockertest.Docker(t, "exec", bastion, "cat", "/etc/ssh/ssh_host_ed25519_key.pub")))
	if err != nil {
		t.Fatal(err)
	}
	kh := filepath.Join(t.TempDir(), "known_hosts")
	os.WriteFile(kh, []byte(knownhosts.Line([]string{knownhosts.Normalize(net.JoinHostPort("127.0.0.1", strconv.Itoa(bport)))}, hk)+"\n"), 0o600)
	ctx := context.Background()
	var jump *sshx.Client
	for try := 0; ; try++ { // sshd may still be starting after /run/ready
		jump, err = (&sshx.Dialer{KnownHostsPath: kh}).Dial(ctx, sshx.Target{Key: "it", Hops: []sshx.Hop{{Host: "127.0.0.1", Port: bport, User: "u", Auth: sshx.Auth{Key: pem.EncodeToMemory(block)}}}})
		if err == nil || try == 30 {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer jump.Close()

	want := net.JoinHostPort(k3s, "6443")
	var dials atomic.Int32
	c, err := New(Config{URL: store.Deref(h.URL), TLS: tlsCfg, Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != want {
			return nil, fmt.Errorf("refused %s", addr)
		}
		dials.Add(1)
		return jump.DialContext(ctx, "tcp", addr)
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Version(ctx); err != nil {
		t.Fatal(err)
	}

	if res, err := c.Apply(ctx, "default", workload, true); err != nil || len(res) != 2 || res[1].Outcome != "created" {
		t.Fatalf("dry run: %+v, %v", res, err)
	}
	if _, err := c.Get(ctx, "configmaps", "default", "settings"); !IsStatus(err, 404) {
		t.Fatalf("the dry run created the config map: %v", err)
	}
	if _, err := c.Apply(ctx, "default", workload, false); err != nil {
		t.Fatal(err)
	}

	var pod Item
	deadline := time.Now().Add(5 * time.Minute)
	for pod.Status != "Running" {
		if time.Now().After(deadline) {
			t.Fatalf("no running web pod: %+v", pod)
		}
		time.Sleep(2 * time.Second)
		p, err := c.List(ctx, "pods", "default", 0, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range p.Items {
			if strings.HasPrefix(it.Name, "web-") {
				pod = it
			}
		}
	}

	logs, err := c.Logs(ctx, "default", pod.Name, "web", 10)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, _ := io.ReadAtLeast(logs, buf, len("hello from web"))
	logs.Close()
	if !strings.Contains(string(buf[:n]), "hello from web") {
		t.Fatalf("logs = %q", buf[:n])
	}

	s, err := c.Exec(ctx, "default", pod.Name, "web", []string{"echo", "ok"}, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(s)
	s.Close()
	if !strings.Contains(string(out), "ok") {
		t.Fatalf("exec printed %q", out)
	}

	if err := c.Scale(ctx, "deployments", "default", "web", 2); err != nil {
		t.Fatal(err)
	}
	if text, err := c.Get(ctx, "deployments", "default", "web"); err != nil || !strings.Contains(text, `"replicas": 2`) {
		t.Fatalf("after scale: %v", err)
	}
	if _, err := c.Metrics(ctx, "default"); err != nil {
		t.Fatal(err)
	}
	if dials.Load() == 0 {
		t.Fatal("nothing went through the bastion")
	}
}
