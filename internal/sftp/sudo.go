package sftp

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strings"

	"hostbay/internal/apperr"
)

// sudoWrite stages content in a private temp folder over SFTP, then has
// root copy it into p with cat. cat truncates and rewrites p in place, so p
// keeps its owner, group and mode, as files.rs's sudo tee did. The password
// reaches sudo on stdin only, so the file bytes never share a pipe with it:
// when sudo has cached credentials and reads nothing, nothing leaks into p.
func (c *Conn) sudoWrite(p, content, password string) error {
	var b [16]byte
	rand.Read(b[:])
	dir := "/tmp/.hostbay-" + hex.EncodeToString(b[:])
	tmp := dir + "/f"
	// SFTP open takes no mode, so the folder is locked down before the file
	// exists; no other user can open the staged content.
	if err := c.sftp.Mkdir(dir); err != nil {
		return fmt.Errorf("write failed: staging %s: %w", dir, err)
	}
	defer c.sftp.RemoveDirectory(dir)
	if err := c.sftp.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("write failed: staging %s: %w", dir, err)
	}
	defer c.sftp.Remove(tmp) // runs before the folder's removal
	if err := c.put(tmp, strings.NewReader(content)); err != nil {
		return fmt.Errorf("write failed: staging %s: %w", tmp, err)
	}
	return c.shell("write", true, password, "sh", "-c", `cat -- "$1" > "$2"`, "sh", tmp, p)
}

// shell runs args, each quoted as one word, optionally as root. With a
// password, sudo -S reads it from stdin; without one, sudo -n fails at once
// instead of waiting on a prompt nobody sees. The password is never part of
// the command line or an error.
func (c *Conn) shell(op string, sudo bool, password string, args ...string) error {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = Quote(a)
	}
	cmd := strings.Join(q, " ")
	var stdin io.Reader
	if sudo {
		// C locale so the messages sudoFailure matches are in English.
		if password != "" {
			cmd = "LC_ALL=C sudo -S -p '' -- " + cmd
			stdin = strings.NewReader(password + "\n")
		} else {
			cmd = "LC_ALL=C sudo -n -- " + cmd
		}
	}
	s, err := c.ssh.NewSession()
	if err != nil {
		return err
	}
	defer s.Close()
	var stderr bytes.Buffer
	s.Stdin, s.Stderr = stdin, &stderr
	if err := s.Run(cmd); err != nil {
		return failure(op, sudo, strings.TrimSpace(stderr.String()), err)
	}
	return nil
}

// failure turns sudo's password complaints into SecretNeeded (kind sudo),
// which the UI answers by asking for a sudo password, and anything else
// into op's stderr.
func failure(op string, sudo bool, stderr string, err error) error {
	if sudo {
		switch {
		case strings.Contains(stderr, "incorrect password"), strings.Contains(stderr, "Sorry, try again"):
			return apperr.New(apperr.SecretNeeded, op+" failed: sudo password incorrect").With("kind", "sudo")
		case strings.Contains(stderr, "a password is required"), strings.Contains(stderr, "no password was provided"):
			return apperr.New(apperr.SecretNeeded, op+" failed: sudo requires a password").With("kind", "sudo")
		}
	}
	if stderr == "" {
		stderr = err.Error()
	}
	return fmt.Errorf("%s failed: %s", op, stderr)
}
