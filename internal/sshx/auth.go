package sshx

import (
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"

	"hostbay/internal/apperr"
)

// Auth is one hop's credentials, already decrypted by the caller. A key
// (Key or KeyPath) is tried before Password when both are set. Agent and
// Kerberos log in without a stored secret (R50, R51).
type Auth struct {
	Password   string
	Key        []byte // private key: OpenSSH, PEM or PuTTY .ppk
	KeyPath    string // used when Key is empty; a leading ~/ means the home directory
	Passphrase string // for an encrypted Key
	Agent      bool   // sign with the local SSH agent's keys
	Kerberos   bool   // gssapi-with-mic with the user's Kerberos ticket
}

var errKerberosUnavailable = errors.New("Kerberos is unavailable on this computer: the system GSSAPI library was not found")

// noTicket is a Kerberos login with no usable ticket.
func noTicket(detail string) error {
	return apperr.New(apperr.SecretNeeded, "No valid Kerberos ticket; run kinit, then connect again ("+detail+")").With("kind", "kerberos")
}

// authMethods builds hop i's auth methods; done releases what they hold
// once the handshake is over. A hop with no usable secret returns
// SecretNeeded, so nothing is sent with an empty password (R29).
func authMethods(i int, h Hop) (methods []ssh.AuthMethod, done func(), err error) {
	done = func() {}
	switch {
	case h.Auth.Agent:
		m, d, err := agentAuth()
		if err != nil {
			return nil, nil, fmt.Errorf("%s (hop %d): %w", h.Host, i, err)
		}
		return []ssh.AuthMethod{m}, d, nil
	case h.Auth.Kerberos:
		c, err := newKerberosClient()
		if err != nil {
			return nil, nil, fmt.Errorf("%s (hop %d): %w", h.Host, i, err)
		}
		return []ssh.AuthMethod{ssh.GSSAPIWithMICAuthMethod(c, h.Host)}, done, nil
	}
	if len(h.Auth.Key) > 0 || h.Auth.KeyPath != "" {
		s, err := loadSigner(h.Auth)
		if err != nil {
			var ae *apperr.Error
			if errors.As(err, &ae) {
				return nil, nil, ae.With("hop", strconv.Itoa(i)).With("host", h.Host)
			}
			return nil, nil, fmt.Errorf("%s (hop %d): %w", h.Host, i, err)
		}
		methods = append(methods, ssh.PublicKeys(s))
	}
	if h.Auth.Password != "" {
		methods = append(methods, ssh.Password(h.Auth.Password))
	}
	if len(methods) == 0 {
		return nil, nil, apperr.New(apperr.SecretNeeded, "A password is needed for "+h.User+"@"+h.Host).
			With("hop", strconv.Itoa(i)).With("host", h.Host).With("kind", "password")
	}
	return methods, done, nil
}

// loadSigner parses a's private key. RSA keys sign with rsa-sha2-512 or
// rsa-sha2-256 only, never SHA-1 ssh-rsa.
func loadSigner(a Auth) (ssh.Signer, error) {
	data := a.Key
	if len(data) == 0 {
		path := a.KeyPath
		if rest, ok := strings.CutPrefix(path, "~/"); ok {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, err
			}
			path = filepath.Join(home, rest)
		}
		var err error
		if data, err = os.ReadFile(path); err != nil {
			return nil, fmt.Errorf("read key: %w", err)
		}
	}

	var key any
	var err error
	if isPPK(data) {
		key, _, err = ParsePPK(data, []byte(a.Passphrase))
	} else {
		key, err = ssh.ParseRawPrivateKey(data)
		var missing *ssh.PassphraseMissingError
		if errors.As(err, &missing) && a.Passphrase != "" {
			key, err = ssh.ParseRawPrivateKeyWithPassphrase(data, []byte(a.Passphrase))
		}
	}
	var missing *ssh.PassphraseMissingError
	switch {
	case errors.As(err, &missing):
		return nil, apperr.New(apperr.SecretNeeded, "This key needs its passphrase").With("kind", "passphrase")
	case errors.Is(err, x509.IncorrectPasswordError):
		return nil, apperr.New(apperr.SecretNeeded, "The key passphrase is wrong").With("kind", "passphrase")
	case err != nil:
		return nil, fmt.Errorf("parse key: %w", err)
	}

	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		return nil, err
	}
	if signer.PublicKey().Type() != ssh.KeyAlgoRSA {
		return signer, nil
	}
	as, ok := signer.(ssh.AlgorithmSigner)
	if !ok {
		return nil, errors.New("RSA key cannot choose its signature algorithm")
	}
	return ssh.NewSignerWithAlgorithms(as, []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256})
}
