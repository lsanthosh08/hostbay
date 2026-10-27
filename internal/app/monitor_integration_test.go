//go:build integration

package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"hostbay/internal/monitor"
)

// Against a real OpenSSH server on Alpine (BusyBox ps and netstat), the
// monitor shows live figures over the verified connection and stops
// sampling while its tab is hidden.
func TestDockerMonitorShowsLiveFiguresAndPausesWhenHidden(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not reachable")
	}
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	var b [4]byte
	rand.Read(b[:])
	name := "hostbay-monitor-" + hex.EncodeToString(b[:])
	run("run", "-d", "--rm", "--name", name, "-p", "127.0.0.1::22", "alpine:3.20", "sh", "-c",
		`apk add --no-cache openssh-server >/dev/null && ssh-keygen -A >/dev/null && adduser -D u && echo u:pw | chpasswd &&
touch /run/ready && exec /usr/sbin/sshd -D -e -o PasswordAuthentication=yes`)
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })
	deadline := time.Now().Add(2 * time.Minute)
	for exec.Command("docker", "exec", name, "test", "-f", "/run/ready").Run() != nil {
		if time.Now().After(deadline) {
			t.Fatal("sshd never became ready")
		}
		time.Sleep(time.Second)
	}
	_, portStr, _ := net.SplitHostPort(run("port", name, "22"))
	port, _ := strconv.Atoi(portStr)
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(run("exec", name, "cat", "/etc/ssh/ssh_host_ed25519_key.pub")))
	if err != nil {
		t.Fatal(err)
	}
	conns, hs, _ := connWithKnownHosts(t, knownhosts.Line([]string{knownhosts.Normalize(net.JoinHostPort("127.0.0.1", portStr))}, key))
	conns.emit = func(string, any) {}
	auth, pw := "password", "pw"
	h, err := hs.SaveHost(Host{Alias: "alpine", Hostname: "127.0.0.1", Port: port, User: "u", AuthType: &auth, Password: &pw})
	if err != nil {
		t.Fatal(err)
	}
	var sess string
	for try := 0; ; try++ { // sshd may still be starting after /run/ready
		if sess, err = conns.Connect(context.Background(), h.ID); err == nil || try == 30 {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		t.Fatal(err)
	}

	svc := NewMonitorService(conns)
	t.Cleanup(func() { svc.ServiceShutdown() })
	var mu sync.Mutex
	var samples []*monitor.Sample
	svc.emit = func(_ string, data any) {
		if e := data.(monitor.Event); e.Sample != nil {
			mu.Lock()
			samples = append(samples, e.Sample)
			mu.Unlock()
		}
	}
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(samples) }
	waitFor := func(n int) *monitor.Sample {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for count() < n {
			if time.Now().After(deadline) {
				t.Fatalf("fewer than %d samples", n)
			}
			time.Sleep(100 * time.Millisecond)
		}
		mu.Lock()
		defer mu.Unlock()
		return samples[len(samples)-1]
	}

	m, err := svc.Start(context.Background(), sess, h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.SetInterval(m.ID, 2); got != 2 {
		t.Fatalf("interval %d", got)
	}
	s := waitFor(2) // the second has rates
	var sshd bool
	for _, p := range s.Ports {
		sshd = sshd || p.Port == 22
	}
	var eth0 bool
	for _, i := range s.Ifaces {
		eth0 = eth0 || (i.Name == "eth0" && len(i.Addrs) > 0)
	}
	if s.MemTotal == 0 || s.Cores == 0 || len(s.Disks) == 0 || len(s.Procs) == 0 || !sshd || !eth0 {
		t.Fatalf("sample %+v", s)
	}

	svc.SetVisible(m.ID, false)
	time.Sleep(time.Second) // a sample already in flight may land
	hidden := count()
	time.Sleep(5 * time.Second)
	if count() != hidden {
		t.Fatalf("sampled while hidden: %d -> %d", hidden, count())
	}
	svc.SetVisible(m.ID, true)
	waitFor(hidden + 1)
}
