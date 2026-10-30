// Package vpn runs app-scoped VPN profiles through the hostbay-vpn helper
// (KTD1): one helper process per connected profile, driven over its stdin
// and stdout with the messages in internal/vpn/proto.
//
// The main binary imports this package, so it imports no VPN library: the
// engines live in cmd/hostbay-vpn.
package vpn

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"hostbay/internal/apperr"
	"hostbay/internal/vpn/proto"
)

var (
	// handshakeTimeout bounds the wait for the helper's hello.
	handshakeTimeout = 10 * time.Second
	// exitGrace is how long a helper may take to exit once its stdin closes.
	exitGrace = 3 * time.Second
)

// HelperName is the helper's file name beside the Hostbay executable.
func HelperName() string {
	if runtime.GOOS == "windows" {
		return "hostbay-vpn.exe"
	}
	return "hostbay-vpn"
}

// HelperPath finds the helper next to the running executable. It never
// searches PATH or the working directory, and nothing the user sets can
// move it.
func HelperPath() (string, error) {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return "", apperr.New(apperr.HelperMissing, "Cannot locate Hostbay's VPN helper: "+err.Error())
	}
	return filepath.Join(filepath.Dir(exe), HelperName()), nil
}

// Helper is one running hostbay-vpn.
type Helper struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser

	wmu       sync.Mutex // one message per write
	closeOnce sync.Once
	closing   chan struct{} // Close was called: its end is not news
	done      chan struct{} // the helper exited and was reaped

	browserMu sync.Mutex     // one openBrowser at a time, so replies pair with requests
	browser   chan proto.Msg // browserPort replies, for BrowserPort and never report
}

// Start launches the helper at path (absolute), checks its version and
// sends start, which carries the profile's secrets and SOCKS login: stdin
// is the only place they go. It returns once start is sent. Every state,
// need and nodeState message after that goes to report, in order, from one
// goroutine, nodeState even after Close: a node writes while it closes. A
// helper that exits without saying why, other than through Close, is
// reported as dropped when it was connected and failed otherwise. ctx
// bounds only the handshake; the helper runs until Close or its own end.
func Start(ctx context.Context, path string, start proto.Msg, report func(proto.Msg)) (*Helper, error) {
	if !filepath.IsAbs(path) {
		return nil, apperr.New(apperr.HelperMissing, "The VPN helper path is not absolute: "+path)
	}
	if _, err := os.Stat(path); err != nil {
		return nil, apperr.New(apperr.HelperMissing, "Hostbay's VPN helper is missing ("+path+"). Reinstall Hostbay.")
	}
	cmd := exec.Command(path) // no arguments: secrets go over stdin only
	cmd.Stderr = os.Stderr    // diagnostics; the helper never prints a secret
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, apperr.New(apperr.HelperMissing, "Cannot start Hostbay's VPN helper: "+err.Error()+". Reinstall Hostbay.")
	}
	h := &Helper{cmd: cmd, stdin: stdin, closing: make(chan struct{}), done: make(chan struct{}), browser: make(chan proto.Msg, 1)}
	r := bufio.NewReader(stdout)
	if err := h.handshake(ctx, r, start); err != nil {
		cmd.Process.Kill()
		stdin.Close()
		cmd.Wait()
		return nil, err
	}
	go h.readLoop(r, report)
	return h, nil
}

func (h *Helper) handshake(ctx context.Context, r *bufio.Reader, start proto.Msg) error {
	// Killing the helper unblocks the read on a timeout or cancel.
	t := time.AfterFunc(handshakeTimeout, func() { h.cmd.Process.Kill() })
	stop := context.AfterFunc(ctx, func() { h.cmd.Process.Kill() })
	m, err := readMsg(r)
	t.Stop()
	stop()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return apperr.New(apperr.HelperMissing, "Hostbay's VPN helper did not start: "+err.Error()+". Reinstall Hostbay.")
	}
	if m.T != proto.Hello || m.Version != proto.Version {
		return apperr.New(apperr.HelperMissing, fmt.Sprintf(
			"Hostbay's VPN helper speaks protocol version %d, but this Hostbay needs %d. Reinstall Hostbay.", m.Version, proto.Version))
	}
	return h.send(start)
}

func readMsg(r *bufio.Reader) (proto.Msg, error) {
	var m proto.Msg
	line, err := r.ReadBytes('\n')
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return m, err
	}
	err = json.Unmarshal(line, &m)
	return m, err
}

// readLoop reports the helper's messages until it exits, then reaps it.
func (h *Helper) readLoop(r *bufio.Reader, report func(proto.Msg)) {
	last, reason := "", ""
	for {
		m, err := readMsg(r)
		if err != nil {
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				h.cmd.Process.Kill()
				reason = "The VPN helper sent a malformed message."
			}
			break
		}
		if m.T == proto.BrowserPort {
			select {
			case h.browser <- m:
			default: // nobody asked
			}
			continue
		}
		if m.T != proto.State && m.T != proto.Need && m.T != proto.NodeState {
			continue
		}
		if m.T == proto.State {
			last = m.State
		}
		report(m)
	}
	go io.Copy(io.Discard, r) // a helper blocked on a write can still exit
	h.stdin.Close()
	t := time.AfterFunc(exitGrace, func() { h.cmd.Process.Kill() })
	err := h.cmd.Wait()
	t.Stop()
	select {
	case <-h.closing:
	default:
		if last != proto.Failed && last != proto.Dropped {
			if reason == "" {
				reason = "The VPN helper stopped unexpectedly."
				if err != nil {
					reason = "The VPN helper stopped unexpectedly (" + err.Error() + ")."
				}
			}
			state := proto.Failed
			if last == proto.Connected {
				state = proto.Dropped
			}
			report(proto.Msg{T: proto.State, State: state, Reason: reason})
		}
	}
	close(h.done)
}

// Answer sends a one-time code the helper asked for.
func (h *Helper) Answer(otp string) error {
	return h.send(proto.Msg{T: proto.Answer, OTP: otp})
}

func (h *Helper) send(m proto.Msg) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	h.wmu.Lock()
	defer h.wmu.Unlock()
	if _, err := h.stdin.Write(append(b, '\n')); err != nil {
		return errors.New("the VPN helper has stopped")
	}
	return nil
}

// Close disconnects: closing stdin makes the helper exit, and one that has
// not after a grace period is killed.
func (h *Helper) Close() {
	h.closeOnce.Do(func() {
		close(h.closing)
		h.stdin.Close()
		go func() {
			select {
			case <-h.done:
			case <-time.After(exitGrace):
				h.cmd.Process.Kill()
			}
		}()
	})
}

// Done is closed once the helper has exited and been reaped.
func (h *Helper) Done() <-chan struct{} { return h.done }
