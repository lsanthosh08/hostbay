package app

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// effectiveAutoProxy reads the automatic proxy macOS applies, from scutil,
// which needs no privilege.
func effectiveAutoProxy() (string, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/scutil", "--proxy").Output()
	if err != nil {
		return "", false, err
	}
	var url string
	on := false
	for _, l := range strings.Split(string(out), "\n") {
		k, v, _ := strings.Cut(l, ":")
		switch strings.TrimSpace(k) {
		case "ProxyAutoConfigEnable":
			on = strings.TrimSpace(v) == "1"
		case "ProxyAutoConfigURLString":
			url = strings.TrimSpace(v)
		}
	}
	return url, on, nil
}
