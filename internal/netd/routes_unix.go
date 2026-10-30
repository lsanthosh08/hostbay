//go:build darwin || linux

package netd

import (
	"net"
	"runtime"
)

// unixInterfaces lists the system's interfaces, a point-to-point one being
// a tunnel.
func unixInterfaces() ([]Iface, error) {
	nis, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]Iface, len(nis))
	for i, ni := range nis {
		out[i] = Iface{ni.Name, isTunnel(runtime.GOOS, ni.Name, ni.Flags&net.FlagPointToPoint != 0, 0, "")}
	}
	return out, nil
}
