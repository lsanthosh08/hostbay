package tunnel

import (
	"fmt"
	"maps"
	"os/exec"
	"slices"
	"strings"

	"hostbay/internal/netd"
)

// dcoName is the ovpn-dco adapter the AWS SSO client runs on. OpenVPN 2.7
// dropped Wintun, and without openvpnserv.exe nothing creates an adapter
// for it, so the helper keeps this one.
const dcoName = "Hostbay AWS VPN"

// runTapctl runs the helper's tapctl and returns its output; tests replace
// it.
var runTapctl = func(path string, env []string, args ...string) (string, error) {
	cmd := exec.Command(path, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("tapctl %s: %v: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// dcoAdapter returns the adapter's name for tunnel h, creating it when it
// is missing. h holds it until cleanup.
func (m *Manager) dcoAdapter(h string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if slices.Contains(slices.Collect(maps.Values(m.ifaces)), dcoName) {
		return "", errDCOBusy
	}
	if err := netd.CheckClient(m.env.Dir, "tapctl", m.env.Owner); err != nil {
		return "", fmt.Errorf("the helper has no tapctl it can trust (%v); reinstall the helper", err)
	}
	path := netd.ClientPath(m.env.Dir, "tapctl")
	out, err := runTapctl(path, m.childEnv, "list", "--hwid", "ovpn-dco")
	if err != nil {
		return "", err
	}
	// Each line is the adapter's GUID, a tab and its name.
	for _, l := range strings.Split(out, "\n") {
		if _, name, ok := strings.Cut(strings.TrimSpace(l), "\t"); ok && strings.TrimSpace(name) == dcoName {
			m.ifaces[h] = dcoName
			return dcoName, nil
		}
	}
	if _, err := runTapctl(path, m.childEnv, "create", "--hwid", "ovpn-dco", "--name", dcoName); err != nil {
		return "", err
	}
	m.ifaces[h] = dcoName
	return dcoName, nil
}
