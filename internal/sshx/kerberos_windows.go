package sshx

import (
	"errors"
	"fmt"
	"strings"
	"syscall"

	"github.com/alexbrainman/sspi"
	"github.com/alexbrainman/sspi/kerberos"
	"golang.org/x/crypto/ssh"
)

// sspiClient is one login's Kerberos context through Windows SSPI, using
// the signed-in user's tickets (R51).
type sspiClient struct {
	cred *sspi.Credentials
	ctx  *kerberos.ClientContext
}

func newKerberosClient() (ssh.GSSAPIClient, error) {
	cred, err := kerberos.AcquireCurrentUserCredentials()
	if err != nil {
		return nil, noTicket(err.Error())
	}
	return &sspiClient{cred: cred}, nil
}

// secENoCredentials is SEC_E_NO_CREDENTIALS: the user has no ticket.
const secENoCredentials = syscall.Errno(0x8009030E)

// Mutual authentication and integrity; never ISC_REQ_DELEGATE.
const sspiFlags = sspi.ISC_REQ_MUTUAL_AUTH | sspi.ISC_REQ_CONNECTION | sspi.ISC_REQ_INTEGRITY

func (c *sspiClient) InitSecContext(target string, token []byte, _ bool) ([]byte, bool, error) {
	var done bool
	var out []byte
	var err error
	if c.ctx == nil {
		// SSPI names the service host/name where GSSAPI says host@name.
		c.ctx, done, out, err = kerberos.NewClientContextWithFlags(c.cred, strings.Replace(target, "@", "/", 1), sspiFlags)
		if err != nil {
			c.ctx = nil
			if errors.Is(err, secENoCredentials) {
				return nil, false, noTicket(err.Error())
			}
			return nil, false, fmt.Errorf("Kerberos: %w", err)
		}
	} else if done, out, err = c.ctx.Update(token); err != nil {
		return nil, false, fmt.Errorf("Kerberos: %w", err)
	}
	if done {
		if err := c.ctx.VerifyFlags(); err != nil {
			return nil, false, errors.New("Kerberos: the server did not prove its identity (no mutual authentication)")
		}
	}
	return out, !done, nil
}

func (c *sspiClient) GetMIC(field []byte) ([]byte, error) {
	if c.ctx == nil {
		return nil, errors.New("Kerberos: no security context")
	}
	return c.ctx.MakeSignature(field, 0, 0)
}

func (c *sspiClient) DeleteSecContext() error {
	if c.ctx != nil {
		c.ctx.Release()
		c.ctx = nil
	}
	if c.cred != nil {
		c.cred.Release()
		c.cred = nil
	}
	return nil
}
