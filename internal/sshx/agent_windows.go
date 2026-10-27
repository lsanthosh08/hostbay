package sshx

import (
	"io"

	"github.com/Microsoft/go-winio"
	sshagent "github.com/xanzy/ssh-agent"
	"golang.org/x/crypto/ssh/agent"
)

// openAgent connects to Windows' OpenSSH agent pipe, or else to Pageant.
func openAgent() (agent.Agent, io.Closer, error) {
	if c, err := winio.DialPipe(`\\.\pipe\openssh-ssh-agent`, nil); err == nil {
		return agent.NewClient(c), c, nil
	}
	ag, _, err := sshagent.New() // Pageant: a window message per request, nothing to close
	if err != nil {
		return nil, nil, errNoAgent
	}
	return ag, nopCloser{}, nil
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }
