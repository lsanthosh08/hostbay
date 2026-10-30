package netd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Config is what install records in the helper's root-owned directory: the
// user who installed it and the Hostbay executable they paired it with.
// Releases are not code-signed yet, so the helper admits a caller by user
// and executable path (R31); a signature check replaces the path once they
// are.
type Config struct {
	// UID is the installing user: a numeric uid on macOS and Linux, a SID on
	// Windows.
	UID string `json:"uid"`
	// App is Hostbay's absolute path, symlinks resolved.
	App string `json:"app"`
}

// Peer is what the system says about the process at the other end of a
// connection.
type Peer struct {
	UID string
	Exe string
}

// authorize admits only the installing user's paired Hostbay.
func authorize(p Peer, cfg Config) error {
	if cfg.UID == "" || cfg.App == "" {
		return &Error{CodeDenied, "The helper has no pairing; reinstall it."}
	}
	if p.UID != cfg.UID {
		return &Error{CodeDenied, "Only the user who installed the helper may use it."}
	}
	same := p.Exe != "" && filepath.Clean(p.Exe) == filepath.Clean(cfg.App)
	if runtime.GOOS == "windows" {
		same = p.Exe != "" && strings.EqualFold(filepath.Clean(p.Exe), filepath.Clean(cfg.App))
	}
	if !same {
		return &Error{CodeDenied, fmt.Sprintf("The helper is paired with Hostbay at %s; pair it with this copy.", cfg.App)}
	}
	return nil
}

// checkOwned refuses a file the helper trusts (its config, and from U12 the
// openvpn and openconnect copies it starts) unless the file and its
// directory belong to owner and neither is writable by group or others, and
// the file is no symlink. owner is root (0) outside tests.
func checkOwned(path string, owner int) error {
	for _, p := range []string{path, filepath.Dir(path)} {
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink", p)
		}
		if runtime.GOOS == "windows" {
			continue // mode bits mean nothing there; the Program Files ACL admits only administrators
		}
		if fi.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("%s is writable by other users", p)
		}
		if uid, ok := fileOwner(fi); ok && uid != owner {
			return fmt.Errorf("%s belongs to uid %d, not %d", p, uid, owner)
		}
	}
	return nil
}

// loadConfig reads the pairing at path, refusing one that is not root-owned.
func loadConfig(path string) (Config, error) {
	var cfg Config
	if err := checkOwned(path, 0); err != nil {
		return cfg, fmt.Errorf("the helper config: %w", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("reading %s: %w", path, err)
	}
	return cfg, nil
}
