//go:build !windows

package tunnel

// dcoAdapter is Windows only.
func (m *Manager) dcoAdapter() (string, error) { return "", nil }
