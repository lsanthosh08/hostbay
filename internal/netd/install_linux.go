package netd

import (
	"fmt"
	"os"
	"path/filepath"
)

func stateDir() string   { return linuxDir }
func socketPath() string { return linuxSocket }

// Install installs the helper beside Hostbay after one polkit prompt.
func Install() error {
	helper, err := besideSelf(helperName)
	if err != nil {
		return err
	}
	return installLinux(helper)
}

// Uninstall removes the helper after one polkit prompt.
func Uninstall() error { return uninstallLinux() }

// systemdUnit takes the installed helper's path.
const systemdUnit = `[Unit]
Description=Hostbay network helper

[Service]
ExecStart=%s serve
RuntimeDirectory=hostbay-netd
Restart=on-failure

[Install]
WantedBy=multi-user.target
`

func systemctl(args ...string) error {
	path := "/usr/bin/systemctl"
	if _, err := os.Stat(path); err != nil {
		path = "/bin/systemctl"
	}
	return runTool(path, args...)
}

// SetUp is the root side of Install: "pkexec hostbay-netd install". The
// installing user comes from pkexec, not from an argument.
func SetUp(string) error {
	uid := os.Getenv("PKEXEC_UID")
	if err := validUID(uid); err != nil {
		return err
	}
	if err := setUpDir(linuxDir, uid); err != nil {
		return err
	}
	if err := writeFile(linuxUnit, []byte(fmt.Sprintf(systemdUnit, filepath.Join(linuxDir, helperName))), 0o644); err != nil {
		return err
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", "hostbay-netd.service"}, {"restart", "hostbay-netd.service"}} {
		if err := systemctl(args...); err != nil {
			return err
		}
	}
	return nil
}

// TearDown is the root side of Uninstall. Stopping the unit runs the
// helper's last sweep.
func TearDown() error {
	systemctl("disable", "--now", "hostbay-netd.service")
	if err := os.Remove(linuxUnit); err != nil && !os.IsNotExist(err) {
		return err
	}
	systemctl("daemon-reload")
	return os.RemoveAll(linuxDir)
}
