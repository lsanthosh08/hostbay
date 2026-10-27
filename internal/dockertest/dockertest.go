// Package dockertest runs throwaway containers for integration tests:
// random names, waiting until ready, cleanup when the test ends, and a skip
// when Docker is missing. Only integration-tagged tests use it.
package dockertest

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Require skips the test unless a Docker daemon answers.
func Require(t testing.TB) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not reachable")
	}
}

// Name is prefix with a random suffix, so parallel runs never collide.
func Name(prefix string) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return prefix + "-" + hex.EncodeToString(b[:])
}

// Docker runs a docker command and returns its trimmed output, failing the
// test on an error.
func Docker(t testing.TB, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// Network creates a network removed when the test ends, and returns its
// name.
func Network(t testing.TB) string {
	t.Helper()
	name := Name("hostbay-it")
	Docker(t, "network", "create", name)
	t.Cleanup(func() { exec.Command("docker", "network", "rm", name).Run() })
	return name
}

// Start runs a detached container named name (docker run args after the
// name), removed when the test ends.
func Start(t testing.TB, name string, args ...string) {
	t.Helper()
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })
	Docker(t, append([]string{"run", "-d", "--rm", "--name", name}, args...)...)
}

// WaitExec waits until the command succeeds inside the container.
func WaitExec(t testing.TB, name string, timeout time.Duration, cmd ...string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for exec.Command("docker", append([]string{"exec", name}, cmd...)...).Run() != nil {
		if time.Now().After(deadline) {
			out, _ := exec.Command("docker", "logs", name).CombinedOutput()
			t.Fatalf("%s never became ready:\n%s", name, out)
		}
		time.Sleep(time.Second)
	}
}

// Port is the localhost port a container's port is published on.
func Port(t testing.TB, name, port string) int {
	t.Helper()
	_, p, err := net.SplitHostPort(strings.SplitN(Docker(t, "port", name, port), "\n", 2)[0])
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(p)
	return n
}
