// Package admin runs commands on a connected host for the admin tabs.
package admin

import (
	"bytes"
	"context"
	"errors"
	"time"

	"golang.org/x/crypto/ssh"
)

// ExecTimeout bounds one generic exec, which exec.rs left unbounded, so a
// command that never ends cannot hold a call open forever.
const ExecTimeout = 2 * time.Minute

// Result is one command's output, kept apart by stream as exec.rs did.
// Code is nil when the command ended without an exit status (by a signal,
// for example).
type Result struct {
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	Code   *int   `json:"code"`
}

// Exec runs command on its own session channel until it exits or ctx ends.
// Stderr is extended data type 1.
func Exec(ctx context.Context, c *ssh.Client, command string) (Result, error) {
	return ExecStdin(ctx, c, command, nil)
}

// ExecStdin is Exec with stdin, so a secret such as a registry password
// reaches the command without appearing in its text.
// shortcut: x/crypto/ssh discards other extended data types, which exec.rs
// sent to stdout; no known server sends them, so this needs a raw channel
// reader only if one turns up.
func ExecStdin(ctx context.Context, c *ssh.Client, command string, stdin []byte) (Result, error) {
	sess, err := c.NewSession()
	if err != nil {
		return Result{}, err
	}
	defer sess.Close()
	var stdout, stderr bytes.Buffer
	sess.Stdout, sess.Stderr = &stdout, &stderr
	if stdin != nil {
		sess.Stdin = bytes.NewReader(stdin)
	}

	stop := context.AfterFunc(ctx, func() { sess.Close() })
	err = sess.Run(command)
	if !stop() {
		return Result{}, ctx.Err()
	}

	r := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	var exit *ssh.ExitError
	var missing *ssh.ExitMissingError
	switch {
	case err == nil:
		r.Code = new(int)
	case errors.As(err, &exit):
		if exit.Signal() == "" {
			code := exit.ExitStatus()
			r.Code = &code
		}
	case errors.As(err, &missing):
	default:
		return Result{}, err
	}
	return r, nil
}
