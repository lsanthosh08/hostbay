//go:build !windows && !darwin

package tunnel

import (
	"errors"
	"io"
)

const ocSystems = "OpenConnect runs system-wide only on macOS and Windows."

func ocTarget(string, string) (string, string, string, error) {
	return "", "", "", errors.New(ocSystems)
}

func createAdapter(string) (io.Closer, error) { return nil, errors.New(ocSystems) }

func Script([]string) error { return errors.New(ocSystems) }
