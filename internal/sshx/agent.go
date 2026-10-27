package sshx

import (
	"errors"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

var errNoAgent = errors.New("no SSH agent is running on this computer; start one and add your key with ssh-add")

// agentAuth signs the login with the local agent's keys (R50). done closes
// the agent connection once the handshake is over.
func agentAuth() (m ssh.AuthMethod, done func(), err error) {
	ag, c, err := openAgent()
	if err != nil {
		return nil, nil, err
	}
	return ssh.PublicKeysCallback(ag.Signers), func() { c.Close() }, nil
}

// forwardAgent serves the server's agent channels on c from the local
// agent. It is registered once per connection at dial time; each session
// that wants the agent asks with agent.RequestAgentForwarding. A connection
// that does not forward has no handler, so x/crypto/ssh refuses the
// channel.
func forwardAgent(c *ssh.Client) {
	chans := c.HandleChannelOpen("auth-agent@openssh.com")
	go func() {
		for nch := range chans {
			go serveAgent(nch)
		}
	}()
}

func serveAgent(nch ssh.NewChannel) {
	ag, c, err := openAgent()
	if err != nil {
		nch.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	defer c.Close()
	ch, reqs, err := nch.Accept()
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	agent.ServeAgent(ag, ch)
	ch.Close()
}
