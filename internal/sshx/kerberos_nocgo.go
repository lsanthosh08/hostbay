//go:build !windows && !cgo

package sshx

import "golang.org/x/crypto/ssh"

// Without cgo there is no way to load the system GSSAPI library.
func newKerberosClient() (ssh.GSSAPIClient, error) { return nil, errKerberosUnavailable }
