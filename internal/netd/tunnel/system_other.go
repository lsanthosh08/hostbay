//go:build !windows && !darwin

package tunnel

import (
	"errors"
	"io"
)

const ocSystems = "OpenConnect runs system-wide only on macOS and Windows."

// ocTarget: only macOS and Windows run OpenConnect in the helper.
func ocTarget(string, string) (string, string, string, error) {
	return "", "", "", errors.New(ocSystems)
}

func createAdapter(string) (io.Closer, error) { return nil, errors.New(ocSystems) }

// Script is openconnect's script on macOS and Windows only.
func Script([]string) error { return errors.New("oc-script runs on macOS and Windows only") }
