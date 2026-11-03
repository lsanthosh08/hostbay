//go:build !darwin

package app

import "errors"

// effectiveAutoProxy is unknown off macOS, where Hostbay sets no system proxy.
func effectiveAutoProxy() (string, bool, error) {
	return "", false, errors.New("the system proxy is read on macOS only")
}
