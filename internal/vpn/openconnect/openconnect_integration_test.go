//go:build integration && !windows

package openconnect

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"hostbay/internal/dockertest"
	"hostbay/internal/vpn/proto"
)

const ocservImage = `FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends ocserv openssl python3 && rm -rf /var/lib/apt/lists/*
RUN printf 'alice:*:%s\n' "$(openssl passwd -6 'Hunter2-VPN!')" > /etc/ocserv/passwd
RUN printf '%s\n' \
  'auth = "plain[passwd=/etc/ocserv/passwd]"' 'tcp-port = 443' 'run-as-user = nobody' 'run-as-group = nogroup' \
  'socket-file = /run/ocserv.socket' 'server-cert = /certs/cert.pem' 'server-key = /certs/key.pem' \
  'isolate-workers = false' 'max-clients = 8' 'max-same-clients = 4' 'keepalive = 300' 'dpd = 60' \
  'try-mtu-discovery = false' 'auth-timeout = 240' 'cookie-timeout = 300' 'deny-roaming = false' \
  'rekey-time = 172800' 'rekey-method = ssl' 'use-occtl = false' 'pid-file = /run/ocserv.pid' 'device = vpns' \
  'predictable-ips = true' 'ipv4-network = 192.168.99.0' 'ipv4-netmask = 255.255.255.0' \
  'route = 192.168.99.0/255.255.255.0' 'ping-leases = false' 'cisco-client-compat = true' > /etc/ocserv/it.conf
CMD ["sh", "-c", "python3 -m http.server 8080 & exec ocserv -f -c /etc/ocserv/it.conf"]
`

// ocserv runs an ocserv gateway with a self-signed certificate and an HTTP
// server inside, and returns its container name, address and the
// certificate's pin.
func ocserv(t *testing.T) (name, gateway, pin string) {
	t.Helper()
	dockertest.Require(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "vpn.example.test"},
		DNSNames: []string{"vpn.example.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	pkcs8, _ := x509.MarshalPKCS8PrivateKey(key)
	dir := t.TempDir()
	os.Chmod(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "cert.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	os.WriteFile(filepath.Join(dir, "key.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}), 0o644)

	image := "hostbay-it-ocserv"
	build := exec.Command("docker", "build", "-q", "-t", image, "-")
	build.Stdin = strings.NewReader(ocservImage)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the ocserv image: %v\n%s", err, out)
	}
	name = dockertest.Name("hostbay-it-ocserv")
	dockertest.Start(t, name, "--cap-add", "NET_ADMIN", "--device", "/dev/net/tun", "-v", dir+":/certs:ro", "-p", "127.0.0.1::443", image)
	port := dockertest.Port(t, name, "443")
	gateway = "127.0.0.1:" + strconv.Itoa(port)
	deadline := time.Now().Add(60 * time.Second)
	for {
		if c, err := net.DialTimeout("tcp", gateway, time.Second); err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ocserv never listened:\n%s", dockertest.Docker(t, "logs", name))
		}
		time.Sleep(time.Second)
	}
	return name, gateway, "pin-sha256:" + base64.StdEncoding.EncodeToString(sum[:])
}

// A real openconnect signs in to ocserv with a password, hands its tunnel
// to the engine through the tun-fd child, and a dial through the engine
// reaches the HTTP server behind the gateway. An unknown and a changed
// certificate both stop with the gateway's real pin before the gateway sees
// a sign-in (AE8): its log names alice only after the password run.
func TestOcservPasswordLogin(t *testing.T) {
	bin, err := exec.LookPath("openconnect")
	if err != nil {
		t.Skip("openconnect not installed")
	}
	out, _ := exec.Command(bin, "--version").CombinedOutput()
	if err := checkVersion(bin, string(out)); err != nil {
		t.Skip(err)
	}
	container, gateway, pin := ocserv(t)
	t.Setenv(fakeEnv, "") // the real openconnect, not the fake
	use := func(t *testing.T, conf string) (*Engine, <-chan proto.Msg) {
		exe, _ := os.Executable()
		oldPath, oldSelf := openconnectPath, selfPath
		openconnectPath = func() (string, error) { return bin, nil }
		selfPath = func() (string, error) { return exe, nil }
		t.Cleanup(func() { openconnectPath, selfPath = oldPath, oldSelf })
		e, err := New(proto.TypeAnyConnect, conf, map[string]string{"password": testPass})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		msgs := make(chan proto.Msg, 32)
		done := make(chan struct{})
		go func() { e.Run(ctx, func(m proto.Msg) { msgs <- m }); close(done) }()
		t.Cleanup(func() { cancel(); <-done })
		return e, msgs
	}
	until := func(t *testing.T, msgs <-chan proto.Msg, state string) proto.Msg {
		t.Helper()
		for {
			m := next(t, msgs)
			if m.State == state {
				return m
			}
			if m.State == proto.Failed || m.State == proto.Dropped {
				t.Fatalf("got %+v, want %s", m, state)
			}
		}
	}

	signedIn := func() bool { return strings.Contains(dockertest.Docker(t, "logs", container), "alice") }
	for _, c := range []struct{ name, pin, code string }{
		{"unknown certificate", "", proto.CodeCertUnknown},
		{"changed certificate", pinA, proto.CodeCertChanged},
	} {
		t.Run(c.name, func(t *testing.T) {
			conf := `{"gateway":"` + gateway + `","user":"alice"}`
			if c.pin != "" {
				conf = `{"gateway":"` + gateway + `","user":"alice","serverCert":"` + c.pin + `"}`
			}
			_, msgs := use(t, conf)
			for {
				m := next(t, msgs)
				if m.State == proto.Connected {
					t.Fatalf("connected to a gateway it should refuse")
				}
				if m.State == proto.Failed {
					if m.Code != c.code || m.ServerCert != pin {
						t.Fatalf("got %+v, want %s with pin %s", m, c.code, pin)
					}
					if signedIn() {
						t.Fatal("the gateway saw a sign-in")
					}
					return
				}
			}
		})
	}
	t.Run("password", func(t *testing.T) {
		e, msgs := use(t, `{"gateway":"`+gateway+`","user":"alice","serverCert":"`+pin+`"}`)
		m := until(t, msgs, proto.Connected)
		if len(m.Address) != 1 || !strings.HasPrefix(m.Address[0], "192.168.99.") {
			t.Fatalf("connected with %+v", m)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		c, err := e.Dial(ctx, "tcp", "192.168.99.1:8080")
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(20 * time.Second))
		io.WriteString(c, "GET / HTTP/1.0\r\n\r\n")
		b, _ := io.ReadAll(c)
		if !strings.HasPrefix(string(b), "HTTP/1.0 200") {
			t.Fatalf("got %q", b)
		}
		if !signedIn() {
			t.Fatal("the gateway's log does not show the sign-in, so the checks above prove nothing")
		}
	})
}
