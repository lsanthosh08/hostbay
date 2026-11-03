//go:build !darwin

package netd

func newProxies() Proxies { return nil }
