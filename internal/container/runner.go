// Package container drives a container engine (Docker, Podman, nerdctl or
// WSLC) through its own CLI (KTD27), on a connected host over SSH or on
// this computer. It learns from uniterm's backend/container (Apache-2.0,
// github.com/ys-ll/uniterm): the engine differences, the WSLC flags and
// output shapes, and the runner split.
package container

import (
	"context"
	"strings"

	"hostbay/internal/admin"
	"hostbay/internal/sftp"
	"hostbay/internal/terminal"
)

// Runner runs one engine command given as an argument list whose first
// element is the engine's name (argv[0] is "docker", "podman", ...).
type Runner interface {
	// Run runs argv to the end with stdin (nil for none). A non-zero exit
	// is not an error here; Code carries it.
	Run(ctx context.Context, argv []string, stdin []byte) (admin.Result, error)
	// PTY starts argv on a terminal of cols x rows.
	PTY(ctx context.Context, argv []string, cols, rows int) (terminal.Transport, error)
}

// Command joins argv into one POSIX shell command line for a remote host.
// Words made only of characters the shell treats literally stay bare, so
// the command reads as typed; every other word goes through sftp.Quote.
func Command(argv []string) string {
	words := make([]string, len(argv))
	for i, a := range argv {
		words[i] = a
		if a == "" || strings.Trim(a, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./:=@%+,") != "" {
			words[i] = sftp.Quote(a)
		}
	}
	return strings.Join(words, " ")
}
