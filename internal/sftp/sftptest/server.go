// Package sftptest runs an in-process SSH server for tests of internal/sftp
// and its callers: password login, an SFTP subsystem over the real
// filesystem (or none, to test the SCP fallback), exec requests run by
// /bin/sh with a fake sudo on PATH, and TCP forwarding (direct-tcpip and
// tcpip-forward) so it can serve as a jump host or a tunnel endpoint.
package sftptest

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// fakeSudo checks the password protocol sudo uses (-S reads one line from
// stdin, -n never prompts) against $FAKE_SUDO_PW. Root ignores a file's write
// bit; the fake models that by lifting it on $FAKE_SUDO_UNLOCK for the run.
const fakeSudo = `#!/bin/sh
mode=
while :; do
  case "$1" in
  -S) mode=S; IFS= read -r pw; shift ;;
  -n) mode=n; shift ;;
  -p) shift 2 ;;
  --) shift; break ;;
  *) break ;;
  esac
done
if [ "$mode" = n ]; then echo "sudo: a password is required" >&2; exit 1; fi
if [ "$mode" != S ] || [ "$pw" != "$FAKE_SUDO_PW" ]; then
  echo "Sorry, try again." >&2; echo "sudo: 1 incorrect password attempt" >&2; exit 1
fi
[ -n "$FAKE_SUDO_UNLOCK" ] && chmod u+w "$FAKE_SUDO_UNLOCK"
"$@"; rc=$?
[ -n "$FAKE_SUDO_UNLOCK" ] && chmod u-w "$FAKE_SUDO_UNLOCK"
exit $rc
`

// Server is a running test server. Its login password is also the sudo
// password.
type Server struct {
	Addr     string
	Password string
	HostKey  ssh.PublicKey

	cfg  *ssh.ServerConfig
	bin  string
	root string

	mu       sync.Mutex
	conns    []net.Conn
	commands []string
	opens    int
	noSFTP   bool
	unlock   string
	names    map[string]string // direct-tcpip host name -> address, resolved here
	dialed   []string
}

// New starts a server that accepts user "u" with password and stops it when
// the test ends.
func New(t testing.TB, password string) *Server {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Password: password, HostKey: signer.PublicKey(), bin: t.TempDir(), root: t.TempDir()}
	if err := os.WriteFile(filepath.Join(s.bin, "sudo"), []byte(fakeSudo), 0o755); err != nil {
		t.Fatal(err)
	}
	s.cfg = &ssh.ServerConfig{PasswordCallback: func(_ ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
		if string(pw) != password {
			return nil, errors.New("wrong password")
		}
		return nil, nil
	}}
	s.cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close(); s.DropAll() })
	s.Addr = ln.Addr().String()
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns = append(s.conns, nc)
			s.mu.Unlock()
			go s.serve(nc)
		}
	}()
	return s
}

// Dial logs in as u with the server's password, checking its host key.
func (s *Server) Dial(t testing.TB) *ssh.Client {
	t.Helper()
	c, err := ssh.Dial("tcp", s.Addr, &ssh.ClientConfig{
		User:            "u",
		Auth:            []ssh.AuthMethod{ssh.Password(s.Password)},
		HostKeyCallback: ssh.FixedHostKey(s.HostKey),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// KnownHostsLine is the known_hosts entry for this server.
func (s *Server) KnownHostsLine() string {
	return knownhosts.Line([]string{knownhosts.Normalize(s.Addr)}, s.HostKey)
}

// Protect makes path read-only to the login user while fake sudo can still
// write it, as with a root-owned file.
func (s *Server) Protect(t testing.TB, path string) {
	t.Helper()
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.unlock = path
	s.mu.Unlock()
}

// Commands lists every exec command received, in order.
func (s *Server) Commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.commands...)
}

// RefuseSFTP makes the server refuse the SFTP subsystem, as a server with
// no sftp-server configured does; exec requests still work.
func (s *Server) RefuseSFTP() {
	s.mu.Lock()
	s.noSFTP = true
	s.mu.Unlock()
}

// Command puts an executable script named name first on the PATH that exec
// requests run with, standing in for the real command of that name.
func (s *Server) Command(t testing.TB, name, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.bin, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// SFTPOpens counts SFTP subsystem requests, refused ones included.
func (s *Server) SFTPOpens() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opens
}

// Resolve makes direct-tcpip requests for name dial addr instead, as a name
// only the server's DNS knows.
func (s *Server) Resolve(name, addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.names == nil {
		s.names = map[string]string{}
	}
	s.names[name] = addr
}

// Dialed lists every direct-tcpip destination requested, as host:port.
func (s *Server) Dialed() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.dialed...)
}

// DropAll cuts every client connection.
func (s *Server) DropAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		c.Close()
	}
	s.conns = nil
}

func (s *Server) serve(nc net.Conn) {
	sc, chans, reqs, err := ssh.NewServerConn(nc, s.cfg)
	if err != nil {
		nc.Close()
		return
	}
	go s.globalRequests(sc, reqs)
	for nch := range chans {
		switch nch.ChannelType() {
		case "session":
			ch, creqs, err := nch.Accept()
			if err != nil {
				continue
			}
			go s.session(ch, creqs)
		case "direct-tcpip":
			go s.direct(nch)
		case "direct-streamlocal@openssh.com":
			go streamLocal(nch)
		default:
			nch.Reject(ssh.UnknownChannelType, "unsupported")
		}
	}
}

// direct dials the requested address (RFC 4254 7.2) and pipes it to the
// channel.
func (s *Server) direct(nch ssh.NewChannel) {
	var p struct {
		Host  string
		Port  uint32
		OHost string
		OPort uint32
	}
	if err := ssh.Unmarshal(nch.ExtraData(), &p); err != nil {
		nch.Reject(ssh.ConnectionFailed, "bad payload")
		return
	}
	addr := net.JoinHostPort(p.Host, strconv.Itoa(int(p.Port)))
	s.mu.Lock()
	s.dialed = append(s.dialed, addr)
	if a, ok := s.names[p.Host]; ok {
		addr = a
	}
	s.mu.Unlock()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		nch.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	ch, reqs, err := nch.Accept()
	if err != nil {
		c.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	pipe(ch, c)
}

// streamLocal connects to the requested Unix socket, as OpenSSH's
// direct-streamlocal does, and pipes it to the channel.
func streamLocal(nch ssh.NewChannel) {
	var p struct {
		Path      string
		Reserved  string
		Reserved2 uint32
	}
	if err := ssh.Unmarshal(nch.ExtraData(), &p); err != nil {
		nch.Reject(ssh.ConnectionFailed, "bad payload")
		return
	}
	c, err := net.Dial("unix", p.Path)
	if err != nil {
		nch.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	ch, reqs, err := nch.Accept()
	if err != nil {
		c.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	pipe(ch, c)
}

// globalRequests serves tcpip-forward and cancel-tcpip-forward (RFC 4254
// 7.1) with real listeners; they close when the connection ends.
func (s *Server) globalRequests(sc *ssh.ServerConn, reqs <-chan *ssh.Request) {
	var mu sync.Mutex
	listeners := map[string]net.Listener{}
	defer func() {
		mu.Lock()
		for _, ln := range listeners {
			ln.Close()
		}
		mu.Unlock()
	}()
	for req := range reqs {
		var p struct {
			Addr string
			Port uint32
		}
		if (req.Type != "tcpip-forward" && req.Type != "cancel-tcpip-forward") || ssh.Unmarshal(req.Payload, &p) != nil {
			req.Reply(false, nil)
			continue
		}
		key := net.JoinHostPort(p.Addr, strconv.Itoa(int(p.Port)))
		if req.Type == "cancel-tcpip-forward" {
			mu.Lock()
			ln, ok := listeners[key]
			delete(listeners, key)
			mu.Unlock()
			if ok {
				ln.Close()
			}
			req.Reply(ok, nil)
			continue
		}
		ln, err := net.Listen("tcp", key)
		if err != nil {
			req.Reply(false, nil)
			continue
		}
		port := uint32(ln.Addr().(*net.TCPAddr).Port)
		mu.Lock()
		listeners[net.JoinHostPort(p.Addr, strconv.Itoa(int(port)))] = ln
		mu.Unlock()
		req.Reply(true, ssh.Marshal(struct{ Port uint32 }{port}))
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				ra := c.RemoteAddr().(*net.TCPAddr)
				go func() {
					ch, creqs, err := sc.OpenChannel("forwarded-tcpip", ssh.Marshal(struct {
						Addr  string
						Port  uint32
						OAddr string
						OPort uint32
					}{p.Addr, port, ra.IP.String(), uint32(ra.Port)}))
					if err != nil {
						c.Close()
						return
					}
					go ssh.DiscardRequests(creqs)
					pipe(ch, c)
				}()
			}
		}()
	}
}

// pipe copies both ways until either side ends, then closes both.
func pipe(a, b io.ReadWriteCloser) {
	done := make(chan struct{}, 2)
	cp := func(dst io.Writer, src io.Reader) { io.Copy(dst, src); done <- struct{}{} }
	go cp(a, b)
	go cp(b, a)
	<-done
	a.Close()
	b.Close()
	<-done
}

func (s *Server) session(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	for req := range reqs {
		switch {
		case req.Type == "subsystem" && len(req.Payload) > 4 && string(req.Payload[4:]) == "sftp":
			s.mu.Lock()
			s.opens++
			refuse := s.noSFTP
			s.mu.Unlock()
			if refuse {
				req.Reply(false, nil)
				continue
			}
			req.Reply(true, nil)
			srv, err := sftp.NewServer(ch)
			if err == nil {
				srv.Serve()
			}
			return
		case req.Type == "exec" && len(req.Payload) > 4:
			cmdline := string(req.Payload[4:])
			req.Reply(true, nil)
			s.mu.Lock()
			s.commands = append(s.commands, cmdline)
			unlock := s.unlock
			s.mu.Unlock()
			cmd := exec.Command("/bin/sh", "-c", cmdline)
			cmd.Dir = s.root
			cmd.Env = append(os.Environ(), "PATH="+s.bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"FAKE_SUDO_PW="+s.Password, "FAKE_SUDO_UNLOCK="+unlock)
			cmd.Stdin, cmd.Stdout, cmd.Stderr = ch, ch, ch.Stderr()
			code := 0
			if err := cmd.Run(); err != nil {
				code = 127
				var ee *exec.ExitError
				if errors.As(err, &ee) {
					code = ee.ExitCode()
				}
			}
			var status [4]byte
			binary.BigEndian.PutUint32(status[:], uint32(code))
			ch.SendRequest("exit-status", false, status[:])
			return
		default:
			req.Reply(false, nil)
		}
	}
}
