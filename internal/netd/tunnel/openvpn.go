package tunnel

import (
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"hostbay/internal/netd"
	"hostbay/internal/vpn/openvpn"
)

// socketWait bounds the wait for OpenVPN's management socket, which it
// opens before anything else.
var socketWait = 10 * time.Second

// openVPN writes the profile's configuration (openvpn.Build, which refuses
// anything off its allowlist), starts the helper's OpenVPN on it and
// returns where Hostbay reaches its management interface: there Hostbay
// sees its states and gives the password, which never reaches the helper.
func (m *Manager) openVPN(h string, p *openvpn.Profile, ended func(string)) (netd.Msg, *netd.Error) {
	if err := netd.CheckClient(m.env.Dir, "openvpn", m.env.Owner); err != nil {
		return netd.Msg{}, refused(fmt.Sprintf("The helper has no OpenVPN it can trust (%v); reinstall the helper.", err))
	}
	var mg openvpn.Management
	var reply netd.Msg
	uid := -1
	if runtime.GOOS == "windows" {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return netd.Msg{}, failed("finding a port for OpenVPN: " + err.Error())
		}
		addr := ln.Addr().String()
		ln.Close()
		pass := rand.Text()
		pf := filepath.Join(m.run, h+".pw")
		if err := writePrivate(pf, pass+"\n"); err != nil {
			return netd.Msg{}, failed("writing OpenVPN's password file: " + err.Error())
		}
		mg, reply = openvpn.Management{Addr: addr, PassFile: pf}, netd.Msg{Mgmt: addr, MgmtPass: pass}
	} else {
		u, err := user.LookupId(m.env.User)
		if err == nil {
			uid, err = strconv.Atoi(u.Uid)
		}
		if err != nil {
			return netd.Msg{}, failed("looking up the installing user: " + err.Error())
		}
		sock := filepath.Join(m.run, h+".sock")
		mg, reply = openvpn.Management{Socket: sock, User: u.Username}, netd.Msg{Mgmt: sock}
	}
	conf, err := openvpn.Build(*p, mg)
	if err != nil {
		m.cleanup(h)
		return netd.Msg{}, refused("Hostbay will not run this OpenVPN profile: " + err.Error())
	}
	path := filepath.Join(m.run, h+".conf")
	if err := writePrivate(path, conf); err != nil {
		m.cleanup(h)
		return netd.Msg{}, failed("writing the OpenVPN configuration: " + err.Error())
	}
	// Its output is dropped: Hostbay reads states from the management interface.
	proc, err := m.start(netd.ClientPath(m.env.Dir, "openvpn"), []string{"--config", path}, nil, io.Discard)
	if err != nil {
		m.cleanup(h)
		return netd.Msg{}, failed("starting OpenVPN: " + err.Error())
	}
	exited := make(chan error, 1)
	stop := supervise(proc, exited)
	end := m.add(h, stop)
	result := make(chan error, 1) // the run's exit, kept for the ended event
	go func() {
		err := <-exited
		result <- err
		end()
		reason := "OpenVPN stopped."
		if err != nil {
			reason = "OpenVPN exited: " + err.Error()
		}
		ended(reason)
	}()
	if mg.Socket != "" {
		if err := waitSocket(mg.Socket, uid, result); err != nil {
			stop()
			return netd.Msg{}, failed(err.Error())
		}
	}
	return reply, nil
}

// waitSocket waits for OpenVPN's management socket and hands it to the
// installing user alone: OpenVPN creates it with root's umask, which on
// Linux would leave the user unable to connect. management-client-user
// then checks the user again at each connection.
func waitSocket(sock string, uid int, exited <-chan error) error {
	deadline := time.Now().Add(socketWait)
	for {
		if fi, err := os.Lstat(sock); err == nil && fi.Mode()&os.ModeSocket != 0 {
			if err := os.Chown(sock, uid, -1); err != nil {
				return fmt.Errorf("handing OpenVPN's management socket to you: %v", err)
			}
			return os.Chmod(sock, 0o600)
		}
		select {
		case err := <-exited:
			return fmt.Errorf("OpenVPN exited before it was ready: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("OpenVPN opened no management socket within %v", socketWait)
		}
	}
}

// writePrivate writes a file only root (administrators on Windows) can read.
func writePrivate(path, text string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(text)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = restrict(path)
	}
	return err
}
