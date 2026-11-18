//go:build !windows

package portowner

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// Name is the name of the process listening on TCP port, read from lsof.
// Some Linux systems have no lsof; Name then fails and the caller words its
// message without a name.
func Name(port int) (string, error) {
	path, err := exec.LookPath("lsof")
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// lsof exits 1 when nothing listens, so its output decides, not its status.
	out, _ := exec.CommandContext(ctx, path, "-nP", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN", "-Fpc").Output()
	if name := lsofName(string(out)); name != "" {
		return name, nil
	}
	return "", errors.New("lsof names no process listening on port " + strconv.Itoa(port))
}

// InUse reports whether err is a listen that failed because the address is
// already in use.
func InUse(err error) bool { return errors.Is(err, syscall.EADDRINUSE) }
