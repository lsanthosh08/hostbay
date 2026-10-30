//go:build !windows

package tunnel

import (
	"errors"
	"io"
)

// createAdapter: only Windows runs OpenConnect in the helper.
func createAdapter(string) (io.Closer, error) {
	return nil, errors.New("OpenConnect runs system-wide only on Windows")
}

// Script is openconnect's script on Windows only.
func Script() error { return errors.New("oc-script runs on Windows only") }
