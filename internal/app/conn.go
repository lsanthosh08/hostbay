package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"golang.org/x/crypto/ssh"

	"hostbay/internal/admin"
	"hostbay/internal/agentexec"
	"hostbay/internal/sshx"
	"hostbay/internal/store"
	"hostbay/internal/terminal"
)

// maxHops bounds a jump-host chain so a loop in the host data cannot spin.
const maxHops = 8

// ConnService opens SSH connections, answers host-key prompts, runs commands
// and drives terminals.
type ConnService struct {
	trust *HostKeyTrust
	creds *HostCredentials
	mgr   *sshx.Manager
	terms *terminal.Registry
	tools *terminalTools
	emit  func(name string, data any)

	mu      sync.Mutex
	watched map[string]*sshx.Client // session ID -> connection its conn-state follows
	hostOf  map[string]string       // session ID -> the saved host it connected to
	noMAC   map[string]bool         // host IDs whose MAC detection found nothing usable
	closers []func(sessionID string)

	// What the AI agent's gate needs to know of each terminal (KTD34),
	// from its owner and never from the page or from terminal text.
	ptys        map[string]ptyMeta // terminal ID -> what opened it
	agentKey    func(ptyID string) // the user typed into a terminal
	agentClosed func(ptyID string) // a terminal ended
}

func NewConnService(trust *HostKeyTrust, creds *HostCredentials, mgr *sshx.Manager) *ConnService {
	s := &ConnService{trust: trust, creds: creds, mgr: mgr, emit: emitEvent, watched: map[string]*sshx.Client{}, hostOf: map[string]string{}, noMAC: map[string]bool{},
		ptys: map[string]ptyMeta{}}
	s.terms = terminal.NewRegistry(
		func(id, data string) { s.emit(PtyDataEvent(id), data) },
		func(id, reason string) {
			s.tools.ended(id)
			s.ptyEnded(id)
			s.emit(PtyClosedEvent(id), reason)
		},
	)
	s.tools = newTerminalTools(creds, s.terms, func(name string, data any) { s.emit(name, data) })
	return s
}

// emitEvent sends an event to the UI; it does nothing before the app runs.
func emitEvent(name string, data any) {
	if a := application.Get(); a != nil {
		a.Event.Emit(name, data)
	}
}

// Connect opens (or joins) the connection to a saved host through its jump
// chain and returns a session ID for the other calls.
func (s *ConnService) Connect(ctx context.Context, hostID string) (string, error) {
	t, err := s.target(hostID)
	if err != nil {
		return "", err
	}
	id, err := s.acquire(ctx, t)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.hostOf[id] = hostID
	s.mu.Unlock()
	s.afterConnect(ctx, hostID, id)
	return id, nil
}

// detectMACCommand prints the MAC of the interface on the default route, as
// SSH Manager did, for Wake-on-LAN (R10).
const detectMACCommand = "ip -o link show $(ip route get 1.1.1.1 2>/dev/null | " +
	"grep -o 'dev [^ ]*' | awk '{print $2}') 2>/dev/null | " +
	"grep -o 'link/ether [0-9a-f:]*' | awk '{print $2}' | head -1"

var macPattern = regexp.MustCompile(`^([0-9a-f]{2}:){5}[0-9a-f]{2}$`)

// afterConnect stamps the host as just connected and, when it has no MAC
// yet, records the one it reports. Both are done here rather than by the
// UI so a save from a stale copy of the host cannot wipe the detected MAC.
// Failures are ignored: neither is needed to use the connection.
func (s *ConnService) afterConnect(ctx context.Context, hostID, sessionID string) {
	d, err := s.creds.st.Data()
	if err != nil {
		return
	}
	h := d.Host(hostID)
	if h == nil {
		return
	}
	s.mu.Lock()
	skip := s.noMAC[hostID]
	s.mu.Unlock()
	var mac string
	if l := s.mgr.Lease(sessionID); (h.Mac == nil || *h.Mac == "") && !skip && l != nil && l.Client() != nil {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		r, err := admin.Exec(ctx, l.Client().Client, detectMACCommand)
		cancel()
		if m := strings.ToLower(strings.TrimSpace(r.Stdout)); err == nil && macPattern.MatchString(m) {
			mac = m
		} else {
			s.mu.Lock()
			s.noMAC[hostID] = true
			s.mu.Unlock()
		}
	}
	_ = s.creds.st.Edit(func(d *store.Data) error {
		if h := d.Host(hostID); mac != "" && h != nil && (h.Mac == nil || *h.Mac == "") {
			if err := d.EditHost(hostID, func(h *store.Host) { h.Mac = &mac }); err != nil {
				return err
			}
		}
		return stampConnected(d, hostID)
	})
}

// stampConnected records that this computer just connected to the host.
func stampConnected(d *store.Data, hostID string) error {
	return d.SetLastConnected(hostID, strconv.FormatInt(time.Now().UnixMilli(), 10))
}

// touch stamps hostID connected; a failed write is ignored, since it is not
// needed to use the session.
func (c *HostCredentials) touch(hostID string) {
	_ = c.st.Edit(func(d *store.Data) error { return stampConnected(d, hostID) })
}

// ConnectAdhoc connects to a host that is not saved, authenticating with
// identityFile.
// It goes through the connected VPN profile that reaches hostname (R17).
func (s *ConnService) ConnectAdhoc(ctx context.Context, hostname string, port int, user, identityFile string) (string, error) {
	d, err := s.creds.st.Data()
	if err != nil {
		return "", err
	}
	p, err := s.creds.routed(d, hostname)
	if err != nil {
		return "", err
	}
	return s.acquire(ctx, sshx.Target{
		Key:  "adhoc:" + user + "@" + hostname + ":" + strconv.Itoa(port) + ":" + identityFile + vpnKey(d, p),
		Hops: []sshx.Hop{{Host: hostname, Port: port, User: user, Auth: sshx.Auth{KeyPath: identityFile}, Proxy: p}},
	})
}

// acquire opens the shared connection and returns its lease ID, watched
// for conn-state events.
func (s *ConnService) acquire(ctx context.Context, t sshx.Target) (string, error) {
	l, err := s.acquireLease(ctx, t)
	if err != nil {
		return "", err
	}
	if cl := l.Client(); cl != nil {
		s.watch(l.ID(), cl)
	}
	return l.ID(), nil
}

// acquireLease is the UI's acquire: it connects the target's VPN profile
// first when it is down (R15).
func (s *ConnService) acquireLease(ctx context.Context, t sshx.Target) (*sshx.Lease, error) {
	if err := s.creds.vpnUp(ctx, t.Hops[0].Proxy); err != nil {
		return nil, err
	}
	l, err := s.mgr.Acquire(ctx, t)
	if err != nil {
		return nil, s.hostKeyPrompt(err)
	}
	return l, nil
}

// hostKeyPrompt turns an unknown host key into a trust prompt the UI
// answers by request ID, and gives a changed key's refusal a removeId for
// RemoveHostKey; every other error goes back as it is.
func (s *ConnService) hostKeyPrompt(err error) error {
	var hk *sshx.HostKeyError
	if !errors.As(err, &hk) {
		return err
	}
	if hk.Changed() {
		return hk.AppError().With("removeId", s.trust.RemovalRequest(hk.Host, hk.Port))
	}
	return s.trust.Request(hk.Host, hk.Port, hk.KeyType, hk.Key, hk.Fingerprint).With("hop", strconv.Itoa(hk.Hop))
}

// target resolves a saved host and its jump hosts into hops.
func (s *ConnService) target(hostID string) (sshx.Target, error) { return s.targetVia(hostID, "") }

// targetVia is target for a chain that also carries a non-SSH host behind
// it, which names VPN profile vpnID, or "".
func (s *ConnService) targetVia(hostID, vpnID string) (sshx.Target, error) {
	d, err := s.creds.st.Data()
	if err != nil {
		return sshx.Target{}, err
	}
	c, err := s.creds.lookupIn(d, hostID)
	if err != nil {
		return sshx.Target{}, err
	}
	return s.chain(d, c, vpnID)
}

// chain builds the hops to last through its saved jump hosts, outermost
// first. The outermost host's proxy, or the VPN profile any hop names,
// carries the first hop (KTD5); a host behind a jump host may not name a
// proxy, since it would be bypassed, and every hop that names a VPN must
// name the same one, which vpnID, when set, also names. The key covers
// every hop's revision and the proxy's or VPN's, so editing any of them
// opens a fresh connection instead of reusing one dialled with the old
// settings.
func (s *ConnService) chain(d *store.Data, last Credentials, vpnID string) (sshx.Target, error) {
	var hops []sshx.Hop
	var key []string
	seen := map[string]bool{}
	c := last
	for {
		h := c.Host
		if seen[h.ID] || len(hops) == maxHops {
			return sshx.Target{}, errors.New("the jump host chain loops or is too long")
		}
		seen[h.ID] = true
		hops = append([]sshx.Hop{hopFor(c)}, hops...)
		key = append(key, h.ID+"@"+strconv.FormatInt(h.Revision, 10))
		if h.UsesVPN() {
			if vpnID != "" && vpnID != *h.VPNID {
				return sshx.Target{}, errors.New(h.Alias + " names another VPN profile than a host reached through it; a jump chain goes through one VPN")
			}
			vpnID = *h.VPNID
		}
		if !h.Jumped() {
			break
		}
		if h.Proxied() {
			return sshx.Target{}, jumpAndProxyErr(h.Alias)
		}
		var err error
		if c, err = s.creds.lookupIn(d, *h.JumpHostID); err != nil {
			return sshx.Target{}, err
		}
	}
	p, err := s.creds.dialVia(d, &c.Host, vpnID)
	if err != nil {
		return sshx.Target{}, err
	}
	hops[0].Proxy = p
	key[len(key)-1] += proxyKey(d, &c.Host) + vpnKey(d, p)
	t := sshx.Target{Key: strings.Join(key, ">"), Hops: hops}
	if k := last.Host.Keepalive; k != nil {
		t.Keepalive = time.Duration(*k) * time.Second
	}
	return t, nil
}

// jumpAndProxyErr refuses the host alias, which names both a jump host and a
// proxy.
func jumpAndProxyErr(alias string) error {
	return errors.New(alias + " is reached through a jump host and also names a proxy; set the proxy on the jump host")
}

// hopFor is one host's hop: its address, sign-in, legacy algorithms and,
// when it is the last hop, agent and X11 forwarding.
func hopFor(c Credentials) sshx.Hop {
	h := c.Host
	auth := sshx.Auth{Password: c.Password, Key: c.Key}
	if c.Key == nil {
		auth.KeyPath = h.IdentityFile
	}
	switch store.Deref(h.AuthType) {
	case "agent":
		auth = sshx.Auth{Agent: true}
	case "kerberos":
		auth = sshx.Auth{Kerberos: true}
	}
	hop := sshx.Hop{Host: h.Hostname, Port: h.Port, User: h.User, Auth: auth, ForwardAgent: h.Forwards(), X11: h.ForwardsX11()}
	if h.Algorithms != nil {
		hop.Algorithms = sshx.Algorithms(*h.Algorithms)
	}
	return hop
}

// TrustHostKey answers the HostKeyUnknown prompt carrying requestID.
func (s *ConnService) TrustHostKey(requestID string, accept bool) error {
	return s.trust.Answer(requestID, accept)
}

// RemoveHostKey deletes the known_hosts entry behind a HostKeyChanged
// refusal's removeId, so a rebuilt server's new key can be trusted on the
// next connect (R13). typedHost must equal the refused host name and OS user
// verification must pass; otherwise known_hosts is left alone.
func (s *ConnService) RemoveHostKey(ctx context.Context, removeID, typedHost string) error {
	p, ok := s.trust.removal(removeID)
	if !ok {
		return errors.New("this host key refusal has expired; connect again")
	}
	if err := sshx.RemoveKnownHost(ctx, s.mgr.KnownHostsPath(), p.host, p.port, typedHost, s.creds.vault.Gate); err != nil {
		return err
	}
	s.trust.removed(removeID)
	return nil
}

// ListKnownHosts lists the known_hosts entries for Settings, read-only.
func (s *ConnService) ListKnownHosts() ([]sshx.KnownHost, error) {
	return sshx.ListKnownHosts(s.mgr.KnownHostsPath())
}

// RemoveKnownHost deletes host:port's known_hosts entries from Settings, as
// RemoveHostKey does after a refusal (R13): typedHost must equal host and OS
// user verification must pass; otherwise known_hosts is left alone.
func (s *ConnService) RemoveKnownHost(ctx context.Context, host string, port int, typedHost string) error {
	return sshx.RemoveKnownHost(ctx, s.mgr.KnownHostsPath(), host, port, typedHost, s.creds.vault.Gate)
}

// Reconnect rebuilds a dropped session's shared connection (KTD5) with fresh
// host-key checks, connecting its VPN profile first when that is down too
// (R15); each view then reopens its own channel. A live connection is
// returned as it is.
func (s *ConnService) Reconnect(ctx context.Context, sessionID string) error {
	l, err := s.lease(sessionID)
	if err != nil {
		return err
	}
	if err := s.creds.vpnUp(ctx, l.Proxy()); err != nil {
		return err
	}
	cl, err := l.Reconnect(ctx)
	if err != nil {
		return err
	}
	s.watch(sessionID, cl)
	return nil
}

// onDisconnect adds f to what Disconnect runs, so work that belongs to a
// session (its Monitor tabs) stops with it.
func (s *ConnService) onDisconnect(f func(sessionID string)) {
	s.mu.Lock()
	s.closers = append(s.closers, f)
	s.mu.Unlock()
}

// Disconnect releases this session's hold on the connection; the last
// release closes it. Unknown IDs are already gone.
func (s *ConnService) Disconnect(sessionID string) error {
	s.terms.CloseOwner(sessionID)
	s.mu.Lock()
	delete(s.watched, sessionID)
	delete(s.hostOf, sessionID)
	closers := s.closers
	s.mu.Unlock()
	for _, f := range closers {
		f(sessionID)
	}
	if l := s.mgr.Lease(sessionID); l != nil {
		l.Release()
	}
	return nil
}

// PpkToPem converts a PuTTY key to OpenSSH PEM, written owner-only.
func (*ConnService) PpkToPem(ppkPath, outPath string) (string, error) {
	data, err := os.ReadFile(ppkPath)
	if err != nil {
		return "", err
	}
	pem, err := sshx.PPKToPEM(data, nil)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(outPath, pem, 0o600); err != nil {
		return "", err
	}
	return outPath, nil
}

// watch emits the session's conn-state: "dropped" when the connection it
// uses ends, and "live" when a later call finds it rebuilt.
func (s *ConnService) watch(sessionID string, cl *sshx.Client) {
	s.mu.Lock()
	prev, seen := s.watched[sessionID]
	s.watched[sessionID] = cl
	s.mu.Unlock()
	if prev == cl {
		return
	}
	if seen {
		s.emit(ConnStateEvent(sessionID), "live")
	}
	go func() {
		cl.Wait()
		s.mu.Lock()
		current := s.watched[sessionID] == cl
		s.mu.Unlock()
		if current {
			s.emit(ConnStateEvent(sessionID), "dropped")
		}
	}()
}

func (s *ConnService) lease(sessionID string) (*sshx.Lease, error) {
	l := s.mgr.Lease(sessionID)
	if l == nil {
		return nil, errors.New("the session has ended; connect again")
	}
	return l, nil
}

// ExecCommand runs one command and returns its stdout, stderr and exit code,
// giving up after admin.ExecTimeout. A dropped connection is rebuilt first.
func (s *ConnService) ExecCommand(ctx context.Context, sessionID, command string) (ExecResult, error) {
	l, err := s.lease(sessionID)
	if err != nil {
		return ExecResult{}, err
	}
	cl, err := l.Live(ctx)
	if err != nil {
		return ExecResult{}, err
	}
	s.watch(sessionID, cl)
	ctx, cancel := context.WithTimeout(ctx, admin.ExecTimeout)
	defer cancel()
	r, err := admin.Exec(ctx, cl.Client, command)
	return ExecResult(r), err
}

// promptlessLease dials a saved host for an approved MCP request (KTD36),
// or joins its live connection. It never registers a trust prompt: an
// unknown or changed key fails as HostKeyUnknown or HostKeyChanged with no
// request ID, and a secret Hostbay lacks fails as SecretNeeded. Nor does it
// connect a VPN profile: one that is down fails as VPNDown naming it, and
// the agent calls connect_vpn (KTD6).
func (s *ConnService) promptlessLease(ctx context.Context, hostID string) (*sshx.Lease, error) {
	t, err := s.target(hostID)
	if err != nil {
		return nil, err
	}
	l, err := s.mgr.Acquire(ctx, t)
	var hk *sshx.HostKeyError
	if errors.As(err, &hk) {
		return nil, hk.AppError()
	}
	return l, err
}

// leaseExec runs command on l's connection with no terminal, streaming its
// output, until it exits or ctx ends: agentexec's MCP runner. An exit by
// signal or with no status is -1.
func leaseExec(ctx context.Context, l *sshx.Lease, command string, stdout, stderr io.Writer) (int, error) {
	cl, err := l.Live(ctx)
	if err != nil {
		return 0, err
	}
	sess, err := cl.Client.NewSession()
	if err != nil {
		return 0, err
	}
	defer sess.Close()
	sess.Stdout, sess.Stderr = stdout, stderr
	stop := context.AfterFunc(ctx, func() { sess.Close() })
	err = sess.Run(command)
	if !stop() {
		return 0, ctx.Err()
	}
	var exit *ssh.ExitError
	var missing *ssh.ExitMissingError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &exit) && exit.Signal() == "":
		return exit.ExitStatus(), nil
	case errors.As(err, &exit), errors.As(err, &missing):
		return -1, nil
	}
	return 0, err
}

// PtyOpen opens a shell on the session's connection and returns the
// terminal's ID. Output is held until PtyAttach. Reopening after an exit
// only adds a channel; after a drop it rebuilds the shared connection.
func (s *ConnService) PtyOpen(ctx context.Context, sessionID string, cols, rows int) (string, error) {
	l, err := s.lease(sessionID)
	if err != nil {
		return "", err
	}
	t, err := terminal.OpenSSH(ctx, l, cols, rows)
	if err != nil {
		return "", err
	}
	if cl := l.Client(); cl != nil {
		s.watch(sessionID, cl)
	}
	s.mu.Lock()
	hostID := s.hostOf[sessionID]
	s.mu.Unlock()
	return s.track(s.openTerm(sessionID, hostID, "ssh", t), ptyMeta{session: sessionID, hostID: hostID, kind: "ssh"}), nil
}

// openTerm opens t and starts its tools (KTD39) before any output is read:
// its host's log and post-login rules, on every new shell, a reconnect's
// included. hostID is "" for a terminal that is not a saved host's; label
// then names its log and history.
func (s *ConnService) openTerm(owner, hostID, label string, t terminal.Transport) string {
	return s.terms.OpenWith(owner, t, func(id string) { s.tools.start(id, hostID, label) })
}

// PtyOpenTelnet opens a Telnet terminal to a saved host (R18), through its
// jump host or proxy. owner groups it like a session ID, so
// Disconnect(owner) closes it. Telnet is unencrypted: everything typed,
// passwords included, crosses in the clear.
func (s *ConnService) PtyOpenTelnet(ctx context.Context, owner, hostID string, cols, rows int) (string, error) {
	h, err := s.host(hostID)
	if err != nil {
		return "", err
	}
	port := h.Port
	if port == 0 {
		port = 23
	}
	conn, err := s.dialHost(ctx, h, port)
	if err != nil {
		return "", err
	}
	s.afterConnect(ctx, hostID, owner)
	return s.track(s.openTerm(owner, hostID, "telnet", terminal.OpenTelnet(conn, cols, rows)), ptyMeta{hostID: hostID, kind: "telnet"}), nil
}

// PtyOpenTCP opens a raw TCP terminal to a saved "tcp" host (R48), through
// its jump host or proxy, echoing typed text locally when the host says
// so. owner groups it like a session ID, so Disconnect(owner) closes it.
// Nothing is encrypted.
func (s *ConnService) PtyOpenTCP(ctx context.Context, owner, hostID string) (string, error) {
	h, err := s.host(hostID)
	if err != nil {
		return "", err
	}
	if h.Port < 1 || h.Port > 65535 {
		return "", errors.New("this host has no port to connect to")
	}
	conn, err := s.dialHost(ctx, h, h.Port)
	if err != nil {
		return "", err
	}
	s.afterConnect(ctx, hostID, owner)
	return s.track(s.openTerm(owner, hostID, "tcp", terminal.OpenTCP(conn, h.LocalEcho != nil && *h.LocalEcho)), ptyMeta{hostID: hostID, kind: "tcp"}), nil
}

// PtyOpenMosh opens a Mosh terminal (R19): mosh-server starts over the
// session's host-key-verified SSH connection, then the terminal runs over
// UDP. A host behind a jump host is refused: mosh's UDP cannot follow the
// chain.
func (s *ConnService) PtyOpenMosh(ctx context.Context, sessionID, hostID string, cols, rows int) (string, error) {
	h, err := s.host(hostID)
	if err != nil {
		return "", err
	}
	if h.Jumped() {
		return "", errors.New("mosh cannot reach a host through a jump host, because its UDP traffic " +
			"cannot pass through SSH; connect to this host with SSH instead")
	}
	if h.Proxied() {
		return "", errors.New("mosh cannot use a proxy, because its UDP traffic would bypass it; " +
			"connect to this host with SSH instead")
	}
	l, err := s.lease(sessionID)
	if err != nil {
		return "", err
	}
	t, err := terminal.OpenMosh(ctx, l, cols, rows)
	if err != nil {
		return "", err
	}
	return s.track(s.openTerm(sessionID, hostID, "mosh", t), ptyMeta{session: sessionID, hostID: hostID, kind: "mosh"}), nil
}

// ListSerialPorts names this computer's serial ports, for PtyOpenSerial.
func (*ConnService) ListSerialPorts() ([]string, error) { return terminal.ListSerialPorts() }

// PtyOpenSerial opens a serial terminal (R20) on a port ListSerialPorts
// names. parity is none, odd, even, mark or space; stopBits is 1, 1.5 or 2.
// owner groups it like a session ID, so Disconnect(owner) closes it.
func (s *ConnService) PtyOpenSerial(owner, port string, baud, dataBits int, parity, stopBits string, localEcho bool) (string, error) {
	t, err := terminal.OpenSerial(terminal.SerialConfig{
		Port: port, Baud: baud, DataBits: dataBits, Parity: parity, StopBits: stopBits, LocalEcho: localEcho,
	})
	if err != nil {
		return "", err
	}
	return s.track(s.openTerm(owner, "", "serial-"+filepath.Base(port), t), ptyMeta{hostID: "serial:" + port, label: "Serial " + port, kind: "serial"}), nil
}

// ListLocalShells lists the shells PtyOpenLocal can start: the login shell
// on macOS and Linux; PowerShell, CMD and WSL distributions on Windows.
func (*ConnService) ListLocalShells() []terminal.LocalShell { return terminal.ListLocalShells() }

// PtyOpenLocal starts a local shell (R21) by its ListLocalShells ID. owner
// groups it like a session ID, so Disconnect(owner) closes it.
func (s *ConnService) PtyOpenLocal(owner, shellID string, cols, rows int) (string, error) {
	t, err := terminal.OpenLocal(shellID, cols, rows)
	if err != nil {
		return "", err
	}
	label, shell := localShell(shellID)
	return s.track(s.openTerm(owner, "", "local-"+shellID, t), ptyMeta{hostID: "local", label: label, kind: "local", shell: shell, probed: true}), nil
}

func (s *ConnService) host(id string) (*store.Host, error) {
	d, err := s.creds.st.Data()
	if err != nil {
		return nil, err
	}
	h := d.Host(id)
	if h == nil {
		return nil, errors.New("host not found")
	}
	return h, nil
}

// PtyAttach starts the terminal's output events; call it once subscribed.
func (s *ConnService) PtyAttach(ptyID string) error { return s.terms.Attach(ptyID) }

// PtyWrite types the user's keys into a terminal. The agent writes
// through agentexec instead, so these always count as keystrokes.
func (s *ConnService) PtyWrite(ptyID, data string) error {
	s.tools.typed(ptyID)
	s.mu.Lock()
	key := s.agentKey
	s.mu.Unlock()
	if key != nil {
		key(ptyID)
	}
	return s.terms.Write(ptyID, []byte(data))
}

func (s *ConnService) PtyResize(ptyID string, cols, rows int) error {
	return s.terms.Resize(ptyID, cols, rows)
}

// PtyClose ends the terminal. Unknown IDs are already closed.
func (s *ConnService) PtyClose(ptyID string) error {
	s.terms.Close(ptyID)
	return nil
}

// ptyMeta is what opened a terminal.
type ptyMeta struct {
	session string // the SSH session for ssh and mosh, which the shell probe runs on
	hostID  string
	label   string // for terminals with no saved host
	kind    string
	shell   agentexec.Shell
	probed  bool
}

func (s *ConnService) track(ptyID string, m ptyMeta) string {
	s.mu.Lock()
	s.ptys[ptyID] = m
	s.mu.Unlock()
	return ptyID
}

func (s *ConnService) pty(ptyID string) ptyMeta {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ptys[ptyID]
}

func (s *ConnService) ptyEnded(ptyID string) {
	s.mu.Lock()
	delete(s.ptys, ptyID)
	closed := s.agentClosed
	s.mu.Unlock()
	if closed != nil {
		closed(ptyID)
	}
}

func (s *ConnService) sessionHost(sessionID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hostOf[sessionID]
}

// setAgentHooks tells the agent's gate about keystrokes and closed
// terminals.
func (s *ConnService) setAgentHooks(key, closed func(ptyID string)) {
	s.mu.Lock()
	s.agentKey, s.agentClosed = key, closed
	s.mu.Unlock()
}

// shellProbe asks the SSH server for the user's login shell, which a PTY
// runs: one constant command on the session's lease (KTD34).
const shellProbe = `printf '%s' "$SHELL"`

// shellFamily names the envelope a shell takes from its program name.
func shellFamily(program string) agentexec.Shell {
	name := strings.TrimSuffix(strings.ToLower(filepath.Base(strings.TrimSpace(program))), ".exe")
	switch name {
	case "sh", "bash", "dash", "zsh", "ksh", "mksh", "ash", "yash":
		return agentexec.ShellPOSIX
	case "pwsh", "powershell":
		return agentexec.ShellPowerShell
	}
	return agentexec.ShellNone // fish, cmd, csh, network devices: confirm-only
}

// localShell is a local shell's label and family. A WSL distribution's
// shell is not known from here, so it is confirm-only.
func localShell(shellID string) (string, agentexec.Shell) {
	for _, l := range terminal.ListLocalShells() {
		switch {
		case l.ID != shellID:
		case strings.HasPrefix(shellID, "wsl:"):
			return l.Name, agentexec.ShellNone
		case shellID == "pwsh" || shellID == "powershell" || shellID == "cmd":
			return l.Name + " (this computer)", shellFamily(shellID)
		default:
			return l.Name + " (this computer)", shellFamily(l.Name)
		}
	}
	return "Local shell", agentexec.ShellNone
}

// agentInfo is a terminal as the agent's gate sees it. An SSH or Mosh
// terminal's shell is probed once on its session's connection.
func (s *ConnService) agentInfo(ctx context.Context, ptyID string) (agentexec.TermInfo, error) {
	s.mu.Lock()
	m, ok := s.ptys[ptyID]
	s.mu.Unlock()
	if !ok {
		return agentexec.TermInfo{}, errors.New("that terminal is closed")
	}
	if !m.probed && m.session != "" {
		m.shell = agentexec.ShellNone
		if l := s.mgr.Lease(m.session); l != nil {
			if cl, err := l.Live(ctx); err == nil {
				pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				r, err := admin.Exec(pctx, cl.Client, shellProbe)
				cancel()
				if err == nil && r.Code != nil && *r.Code == 0 {
					m.shell = shellFamily(r.Stdout)
				}
			}
		}
		m.probed = true
		s.mu.Lock()
		if _, ok := s.ptys[ptyID]; ok {
			s.ptys[ptyID] = m
		}
		s.mu.Unlock()
	}
	info := agentexec.TermInfo{HostID: m.hostID, Label: m.label, Kind: m.kind, Shell: m.shell}
	if m.label == "" {
		info.Label = "Unsaved host"
		if s.creds == nil {
			return info, nil
		}
		if h, err := s.host(m.hostID); err == nil {
			info.Label = fmt.Sprintf("%s (%s@%s)", h.Alias, h.User, h.Hostname)
		}
	}
	return info, nil
}

// agentTerminals lists the open terminals the agent can target.
func (s *ConnService) agentTerminals() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.ptys))
	for id := range s.ptys {
		ids = append(ids, id)
	}
	return ids
}
