//go:build !windows

package netd

import "errors"

func elevateNative(string, ...string) (int, error) {
	return 0, errors.New("UAC elevation exists only on Windows")
}
