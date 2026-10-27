//go:build !windows

package sshx

import (
	"io"
	"net"
	"os"

	"golang.org/x/crypto/ssh/agent"
)

// openAgent connects to the agent at SSH_AUTH_SOCK.
func openAgent() (agent.Agent, io.Closer, error) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, nil, errNoAgent
	}
	c, err := net.Dial("unix", sock)
	if err != nil {
		return nil, nil, errNoAgent
	}
	return agent.NewClient(c), c, nil
}
