package netd

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// darwinProxies sets each network service's automatic proxy with
// /usr/sbin/networksetup, by absolute path and argv; tests replace run.
type darwinProxies struct {
	run func(args ...string) (string, error)
}

func newProxies() Proxies { return darwinProxies{run: networksetup} }

// networksetup runs under a deadline: the helper holds its operation lock
// meanwhile, so a hung call would stall every route change.
func networksetup(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/networksetup", args...).CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return "", errors.New(msg)
		}
		return "", fmt.Errorf("networksetup: %w", err)
	}
	return string(out), nil
}

// Services skips the listing's header line and disabled services, which
// networksetup marks with a leading asterisk.
func (p darwinProxies) Services() ([]string, error) {
	out, err := p.run("-listallnetworkservices")
	if err != nil {
		return nil, err
	}
	var svcs []string
	for i, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "*") || i == 0 && strings.Contains(l, "asterisk") {
			continue
		}
		svcs = append(svcs, l)
	}
	return svcs, nil
}

// Get reads "URL: <url>" and "Enabled: Yes"; an unset URL reads "(null)".
func (p darwinProxies) Get(service string) (ProxyState, error) {
	out, err := p.run("-getautoproxyurl", service)
	if err != nil {
		return ProxyState{}, err
	}
	var st ProxyState
	for _, l := range strings.Split(out, "\n") {
		k, v, _ := strings.Cut(l, ":")
		switch v = strings.TrimSpace(v); strings.TrimSpace(k) {
		case "URL":
			if v != "(null)" {
				st.URL = v
			}
		case "Enabled":
			st.On = v == "Yes"
		}
	}
	return st, nil
}

func (p darwinProxies) Set(service string, st ProxyState) error {
	if st.URL != "" {
		if _, err := p.run("-setautoproxyurl", service, st.URL); err != nil {
			return err
		}
	}
	state := "off"
	if st.On {
		state = "on"
	}
	_, err := p.run("-setautoproxystate", service, state)
	return err
}
