package admin

import (
	"context"
	"errors"
	"strings"

	"golang.org/x/crypto/ssh"

	"hostbay/internal/sftp"
)

type SystemdUnit struct {
	Unit        string `json:"unit"`
	Load        string `json:"load"`
	Active      string `json:"active"`
	Sub         string `json:"sub"`
	Description string `json:"description"`
}

func SystemdList(ctx context.Context, c *ssh.Client) ([]SystemdUnit, error) {
	// --plain --no-legend --no-pager keeps columns clean
	r, err := Exec(ctx, c, "systemctl list-units --type=service --all --plain --no-legend --no-pager 2>/dev/null")
	if err != nil {
		return nil, err
	}
	var units []SystemdUnit
	for _, line := range strings.Split(r.Stdout, "\n") {
		f := strings.Fields(line) // unit load active sub description...
		if len(f) == 0 {
			continue
		}
		for len(f) < 4 {
			f = append(f, "")
		}
		units = append(units, SystemdUnit{f[0], f[1], f[2], f[3], strings.Join(f[4:], " ")})
	}
	return units, nil
}

func SystemdAction(ctx context.Context, c *ssh.Client, unit, action string) (Result, error) {
	switch action {
	case "start", "stop", "restart", "status", "enable", "disable":
	default:
		return Result{}, errors.New("invalid action")
	}
	if err := validUnit(unit); err != nil {
		return Result{}, err
	}
	return Exec(ctx, c, "sudo -n systemctl "+action+" "+sftp.Quote(unit)+" 2>&1")
}
