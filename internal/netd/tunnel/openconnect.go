package tunnel

import (
	"bufio"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"hostbay/internal/netd"
	"hostbay/internal/vpn/openconnect"
)

// openConnect runs the helper's openconnect on a Wintun adapter it creates
// (Windows, where openconnect has no unprivileged mode). openconnect calls
// the helper back as its script ("hostbay-netd oc-script", Script), which
// puts the tunnel's addresses on the adapter; Hostbay routes through it
// (U11). The password goes on stdin.
func (m *Manager) openConnect(h string, oc *netd.OpenConnect, up func(string, []string), ended func(string)) (netd.Msg, *netd.Error) {
	if err := netd.CheckClient(m.env.Dir, "openconnect", m.env.Owner); err != nil {
		return netd.Msg{}, refused(fmt.Sprintf("The helper has no OpenConnect it can trust (%v); reinstall the helper.", err))
	}
	name := "Hostbay " + h[:8]
	script := `"` + filepath.Join(m.env.Dir, "hostbay-netd.exe") + `" oc-script`
	args, err := openconnect.SystemArgs(oc.Type, oc.Config, map[string]string{"password": oc.Password}, name, script)
	if err != nil {
		return netd.Msg{}, refused(err.Error())
	}
	adapter, err := m.adapter(name)
	if err != nil {
		return netd.Msg{}, failed("creating the tunnel adapter: " + err.Error())
	}
	var stdin io.Reader
	if oc.Password != "" {
		stdin = strings.NewReader(oc.Password + "\n")
	}
	out, outW := io.Pipe()
	proc, err := m.start(netd.ClientPath(m.env.Dir, "openconnect"), args, stdin, outW)
	if err != nil {
		adapter.Close()
		return netd.Msg{}, failed("starting OpenConnect: " + err.Error())
	}
	go func() {
		// shortcut: "up" is read from openconnect's own progress line; if a
		// release rewords it, have oc-script report instead.
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if rest, ok := strings.CutPrefix(sc.Text(), "Configured as "); ok {
				addr, _, _ := strings.Cut(rest, ",")
				up(name, []string{strings.TrimSpace(addr)})
			}
		}
		io.Copy(io.Discard, out)
	}()
	exited := make(chan error, 1)
	end := m.add(h, supervise(proc, exited))
	go func() {
		err := <-exited
		outW.Close()
		adapter.Close()
		end()
		reason := "OpenConnect stopped."
		if err != nil {
			reason = "OpenConnect exited: " + err.Error()
		}
		ended(reason)
	}()
	return netd.Msg{Iface: name}, nil
}
