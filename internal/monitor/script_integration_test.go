//go:build integration

package monitor

import (
	"os/exec"
	"testing"
)

// Script runs on real Debian and Alpine userlands over a real
// Linux kernel, and what it prints parses into every figure. Slim images
// have no ss or ip, so this also runs the /proc/net and /sys/class/net
// fallbacks.
func TestDockerScriptParsesOnEachDistribution(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not reachable")
	}
	// shortcut: no RHEL-family image; the rhel fixture covers its output.
	for _, image := range []string{"debian:bookworm-slim", "alpine:3.20"} {
		t.Run(image, func(t *testing.T) {
			// As over SSH, the exit status means nothing (a missing ip ends it
			// with 127); only output counts.
			out, err := exec.Command("docker", "run", "--rm", image, "sh", "-c", Script).Output()
			if _, exited := err.(*exec.ExitError); err != nil && !exited {
				t.Fatalf("docker run: %v\n%s", err, out)
			}
			r := parse(string(out))
			s := finish(nil, r, 0)
			if r.os != "Linux" || s.Cores == 0 || s.MemTotal == 0 || s.MemUsed == 0 || s.CPU <= 0 {
				t.Errorf("os %q cores %d mem %d/%d cpu %v", r.os, s.Cores, s.MemUsed, s.MemTotal, s.CPU)
			}
			if len(s.Disks) == 0 || len(s.Procs) == 0 || len(s.Ifaces) == 0 || len(r.c.net) == 0 {
				t.Errorf("disks %v procs %v ifaces %v net %v\n%s", s.Disks, s.Procs, s.Ifaces, r.c.net, out)
			}
			if len(out) > MaxOutput {
				t.Errorf("output %d bytes", len(out))
			}
		})
	}
}
