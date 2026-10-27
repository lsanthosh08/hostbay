package terminal

import (
	"context"
	"errors"
	"io"
	"sync/atomic"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"hostbay/internal/sshx"
)

var errConnLost = errors.New("the connection was lost; reconnect the terminal")

// sshPty is a shell on a PTY over one session channel of a lease's shared
// connection (KTD5).
type sshPty struct {
	lease *sshx.Lease
	cl    *sshx.Client // the connection this channel lives on
	sess  *ssh.Session
	stdin io.Writer
	out   *io.PipeReader
	lost  atomic.Bool
}

// OpenSSH opens a shell on l's connection. A live connection only gains a
// channel, so other tabs' jobs are untouched; a dropped one goes through the
// manager's shared rebuild first (R34).
func OpenSSH(ctx context.Context, l *sshx.Lease, cols, rows int) (Transport, error) {
	return OpenSSHCommand(ctx, l, cols, rows, "")
}

// OpenSSHCommand is OpenSSH running command on the PTY in place of the
// login shell, as a container engine's exec -it does; "" is the shell.
func OpenSSHCommand(ctx context.Context, l *sshx.Lease, cols, rows int, command string) (Transport, error) {
	cl, err := l.Reconnect(ctx)
	if err != nil {
		return nil, err
	}
	sess, err := cl.NewSession()
	if err != nil {
		return nil, err
	}
	if cl.ForwardAgent {
		// Like OpenSSH, a server that refuses forwarding still gets a shell.
		_ = agent.RequestAgentForwarding(sess)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		return nil, err
	}
	// Both streams go to one pipe, as pty.rs merged them. The pipe has no
	// buffer, so a slow reader stalls the channel and SSH flow control.
	pr, pw := io.Pipe()
	sess.Stdout, sess.Stderr = pw, pw
	if err := sess.RequestPty("xterm-256color", rows, cols, ssh.TerminalModes{}); err != nil {
		sess.Close()
		return nil, err
	}
	// Sent only when the host forwards X11; a refusal still gets a shell,
	// as with the agent. The session's cookie stops working when it ends.
	endX11, _ := cl.RequestX11(sess)
	start := sess.Shell
	if command != "" {
		start = func() error { return sess.Start(command) }
	}
	if err := start(); err != nil {
		endX11()
		sess.Close()
		return nil, err
	}
	p := &sshPty{lease: l, cl: cl, sess: sess, stdin: stdin, out: pr}
	go func() {
		// A shell that exits sends its status; a channel cut by a dead
		// connection ends without one.
		var missing *ssh.ExitMissingError
		p.lost.Store(errors.As(sess.Wait(), &missing) || p.replaced())
		endX11()
		pw.Close()
	}()
	return p, nil
}

func (p *sshPty) Read(b []byte) (int, error) { return p.out.Read(b) }

// Write refuses once the lease has moved to a new connection or dropped, so
// keystrokes never vanish into a channel on a dead connection.
func (p *sshPty) Write(b []byte) (int, error) {
	if p.replaced() {
		p.Close()
		return 0, errConnLost
	}
	return p.stdin.Write(b)
}

func (p *sshPty) Resize(cols, rows int) error { return p.sess.WindowChange(rows, cols) }

func (p *sshPty) Close() error {
	p.out.Close() // unblocks the copy into the pipe
	return p.sess.Close()
}

func (p *sshPty) EndReason() string {
	if p.lost.Load() {
		return "connection lost"
	}
	return "session closed"
}

func (p *sshPty) replaced() bool { return p.lease.Client() != p.cl }
