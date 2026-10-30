//go:build !windows

package tunnel

// restrict and restrictDir are no-ops: writePrivate's mode 0600 already
// admits only root, and the run folder must stay open for the user to reach
// OpenVPN's management socket, which management-client-user guards.
func restrict(string) error    { return nil }
func restrictDir(string) error { return nil }
