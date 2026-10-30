//go:build !windows

package openconnect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"hostbay/internal/vpn/proto"
	"hostbay/internal/vpn/tunfd"
)

var (
	// openconnectPath and selfPath find openconnect and hostbay-vpn; tests
	// point them at the test binary.
	openconnectPath = findOpenConnect
	selfPath        = os.Executable
)

// handoffEnv names the descriptor of the socket the tun-fd and browser
// children write to. openconnect passes its environment and descriptors on
// to both.
const handoffEnv = "HOSTBAY_VPN_HANDOFF"

// handoff is one message from a child: the tunnel's settings, sent with
// its packet socket, or the browser sign-in URL.
type handoff struct {
	Env map[string]string `json:"env,omitempty"`
	URL string            `json:"url,omitempty"`
}

// Engine is one openconnect connection. New prepares it, Run brings it up,
// Dial reaches hosts through it, and Answer answers its one-time-code
// prompt.
type Engine struct {
	launch

	mu     sync.Mutex
	stdin  io.Writer // openconnect's, while it runs
	report func(proto.Msg)

	stack atomic.Pointer[tunfd.Stack] // set while connected
}

// New checks a start message for an OpenConnect type. Config is a JSON
// object with gateway, user, serverCert and browser; secrets["password"] is
// the password.
func New(typ, conf string, secrets map[string]string) (*Engine, error) {
	l, err := prepare(typ, conf, secrets)
	if err != nil {
		return nil, err
	}
	return &Engine{launch: l}, nil
}

// Run starts openconnect and reports connecting, need (a one-time code or
// a browser sign-in), connected once the tunnel's socket arrives, and then
// dropped when openconnect ends, or failed if it ends before that. A
// gateway certificate that is not trusted, or not the pinned one, fails
// with CodeCertUnknown or CodeCertChanged and the certificate's pin (R21).
// Ending ctx stops openconnect, which signs out.
func (e *Engine) Run(ctx context.Context, report func(proto.Msg)) {
	report(proto.Msg{T: proto.State, State: proto.Connecting})
	fail := func(m proto.Msg) {
		m.T, m.State = proto.State, proto.Failed
		report(m)
	}
	bin, err := openconnectPath()
	if err != nil {
		fail(proto.Msg{Reason: err.Error()})
		return
	}
	self, err := selfPath()
	if err != nil {
		fail(proto.Msg{Reason: "Cannot find hostbay-vpn's own path: " + err.Error()})
		return
	}
	out, _ := exec.CommandContext(ctx, bin, "--version").CombinedOutput()
	if err := checkVersion(bin, string(out)); err != nil {
		fail(proto.Msg{Reason: err.Error()})
		return
	}

	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		fail(proto.Msg{Reason: "Cannot make the tunnel handoff socket: " + err.Error()})
		return
	}
	syscall.CloseOnExec(fds[0])
	syscall.CloseOnExec(fds[1])
	theirs := os.NewFile(uintptr(fds[1]), "handoff")
	ours := os.NewFile(uintptr(fds[0]), "handoff")
	hc, err := net.FileConn(ours)
	ours.Close()
	if err != nil {
		theirs.Close()
		fail(proto.Msg{Reason: "Cannot use the tunnel handoff socket: " + err.Error()})
		return
	}
	defer hc.Close()
	outR, outW, err := os.Pipe()
	if err != nil {
		theirs.Close()
		fail(proto.Msg{Reason: err.Error()})
		return
	}
	defer outR.Close()

	cmd := exec.CommandContext(ctx, bin, e.args(self)...)
	cmd.Env = append(os.Environ(), handoffEnv+"=3")
	cmd.ExtraFiles = []*os.File{theirs}
	cmd.Stdout, cmd.Stderr = outW, outW                                      // never hostbay-vpn's stdout, which is the protocol
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) } // openconnect signs out on SIGTERM
	cmd.WaitDelay = 5 * time.Second
	stdin, err := cmd.StdinPipe()
	if err == nil {
		err = cmd.Start()
	}
	theirs.Close()
	outW.Close()
	if err != nil {
		fail(proto.Msg{Reason: "Cannot start OpenConnect: " + err.Error()})
		return
	}
	if e.password != "" {
		io.WriteString(stdin, e.password+"\n") // read while openconnect parses its options
	}
	e.mu.Lock()
	e.stdin, e.report = stdin, report
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.stdin = nil
		e.mu.Unlock()
	}()

	var connected, failed atomic.Bool
	handoffs := make(chan struct{})
	go func() {
		defer close(handoffs)
		receive(hc.(*net.UnixConn), func(h handoff, f *os.File) {
			switch {
			case h.URL != "":
				report(proto.Msg{T: proto.Need, Need: proto.NeedBrowser, URL: h.URL})
			case f != nil && !connected.Load():
				n, err := parseNet(h.Env)
				var s *tunfd.Stack
				if err == nil {
					s, err = tunfd.New(f, n.addrs, n.dns, n.search, n.mtu)
				} else {
					f.Close()
				}
				if err != nil {
					// openconnect keeps running with no one reading the tunnel; stop it.
					failed.Store(true)
					cmd.Cancel()
					report(proto.Msg{T: proto.State, State: proto.Failed, Reason: err.Error()})
					return
				}
				e.stack.Store(s)
				connected.Store(true)
				report(proto.Msg{T: proto.State, State: proto.Connected, Address: n.address(), Routes: n.routes})
			case f != nil:
				f.Close()
			}
		})
	}()
	lines := make(chan output, 1)
	go func() { lines <- e.watch(outR, report) }()

	werr := cmd.Wait()
	hc.Close()
	<-handoffs
	if s := e.stack.Swap(nil); s != nil {
		s.Close()
	}
	var o output
	select {
	case o = <-lines:
	case <-time.After(time.Second): // a child still holds the output pipe
	}
	if ctx.Err() != nil || failed.Load() {
		return
	}
	reason := o.last
	if reason == "" {
		reason = fmt.Sprintf("OpenConnect ended (%v).", werr)
	}
	switch {
	case connected.Load():
		report(proto.Msg{T: proto.State, State: proto.Dropped, Reason: reason})
	case o.cert != "" && e.cfg.ServerCert != "":
		fail(proto.Msg{Code: proto.CodeCertChanged, ServerCert: o.cert, Reason: fmt.Sprintf(
			"The VPN gateway's certificate changed: Hostbay pinned %s, the gateway presented %s. Hostbay did not sign in.",
			e.cfg.ServerCert, o.cert)})
	case o.cert != "":
		fail(proto.Msg{Code: proto.CodeCertUnknown, ServerCert: o.cert, Reason: fmt.Sprintf(
			"This computer does not trust the VPN gateway's certificate, %s. Hostbay did not sign in; trust the certificate to connect.",
			o.cert)})
	default:
		fail(proto.Msg{Reason: reason})
	}
}

// output is what watch learned from openconnect's output.
type output struct {
	last string // the last line, for a reason
	cert string // the pin of a certificate openconnect refused
}

// watch reads openconnect's output until it ends. A line naming
// --servercert carries the refused certificate's pin. Text left waiting
// without a newline and ending in a colon is a prompt, which only comes
// after --passwd-on-stdin's line is used: a one-time code.
func (e *Engine) watch(r io.Reader, report func(proto.Msg)) output {
	var o output
	var pending []byte
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		pending = append(pending, buf[:n]...)
		for {
			i := bytes.IndexByte(pending, '\n')
			if i < 0 {
				break
			}
			line := strings.TrimSpace(string(pending[:i]))
			pending = pending[i+1:]
			if strings.Contains(line, "--servercert") {
				if pin := pinRE.FindString(line); pin != "" {
					o.cert = pin
				}
			}
			if line != "" {
				o.last = line
			}
		}
		// shortcut: every prompt reads as a one-time code; a profile without a user name would be asked "Username:" here.
		if p := strings.TrimSpace(string(pending)); strings.HasSuffix(p, ":") {
			pending = pending[:0]
			report(proto.Msg{T: proto.Need, Need: proto.NeedOTP})
		}
		if err != nil {
			return o
		}
	}
}

// Answer sends a one-time code to openconnect's waiting prompt.
func (e *Engine) Answer(code string) {
	code, _, _ = strings.Cut(code, "\n") // one line answers one prompt
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stdin == nil {
		return
	}
	io.WriteString(e.stdin, strings.TrimSuffix(code, "\r")+"\n")
	e.report(proto.Msg{T: proto.State, State: proto.Connecting})
}

// Dial opens a TCP connection, or a connected UDP socket for network "udp",
// through the tunnel, resolving names through the VPN's DNS.
func (e *Engine) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	s := e.stack.Load()
	if s == nil {
		return nil, errors.New("the VPN is not connected")
	}
	return s.Dial(ctx, network, address)
}

// Child runs the modes openconnect starts hostbay-vpn in, with arg its one
// argument: "tun-fd" as the --script-tun script, which sends VPNFD and the
// tunnel's settings to the helper and exits (the helper's copy of the
// socket keeps the tunnel open), or a URL as the --external-browser, which
// sends the sign-in URL for Hostbay to open.
func Child(arg string) error {
	fd, err := strconv.Atoi(os.Getenv(handoffEnv))
	if err != nil {
		return fmt.Errorf("%q runs only under the hostbay-vpn helper", arg)
	}
	f := os.NewFile(uintptr(fd), "handoff")
	c, err := net.FileConn(f)
	f.Close()
	if err != nil {
		return err
	}
	defer c.Close()
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return errors.New("the handoff descriptor is not a Unix socket")
	}
	switch {
	case arg == "tun-fd":
		vpnfd, err := strconv.Atoi(os.Getenv("VPNFD"))
		if err != nil {
			return errors.New("openconnect gave no VPNFD")
		}
		env := map[string]string{}
		for _, kv := range os.Environ() {
			if k, v, _ := strings.Cut(kv, "="); scriptVar(k) {
				env[k] = v
			}
		}
		return send(uc, handoff{Env: env}, vpnfd)
	case strings.HasPrefix(arg, "https://") || strings.HasPrefix(arg, "http://"):
		return send(uc, handoff{URL: arg}, -1)
	}
	return fmt.Errorf("unknown mode %q", arg)
}

// send writes h as one JSON line, with fd attached when it is not -1.
func send(c *net.UnixConn, h handoff, fd int) error {
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	var oob []byte
	if fd >= 0 {
		oob = syscall.UnixRights(fd)
	}
	n, _, err := c.WriteMsgUnix(b, oob, nil)
	if err == nil && n < len(b) {
		_, err = c.Write(b[n:])
	}
	return err
}

// receive calls got for each message until the socket ends. A message with
// Env gets the descriptor that came with it, which got then owns.
func receive(c *net.UnixConn, got func(handoff, *os.File)) {
	var pending []byte
	var files []*os.File
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	buf, oob := make([]byte, 64<<10), make([]byte, syscall.CmsgSpace(16*4))
	for {
		n, oobn, _, _, err := c.ReadMsgUnix(buf, oob)
		if msgs, perr := syscall.ParseSocketControlMessage(oob[:oobn]); perr == nil {
			for _, m := range msgs {
				fds, _ := syscall.ParseUnixRights(&m)
				for _, fd := range fds {
					syscall.CloseOnExec(fd)
					files = append(files, os.NewFile(uintptr(fd), "vpnfd"))
				}
			}
		}
		pending = append(pending, buf[:n]...)
		for {
			i := bytes.IndexByte(pending, '\n')
			if i < 0 {
				break
			}
			var h handoff
			jerr := json.Unmarshal(pending[:i], &h)
			pending = pending[i+1:]
			if jerr != nil {
				continue
			}
			var f *os.File
			if h.Env != nil && len(files) > 0 {
				f, files = files[0], files[1:]
			}
			got(h, f)
		}
		if err != nil || n == 0 && oobn == 0 {
			return
		}
	}
}
