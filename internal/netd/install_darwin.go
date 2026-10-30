package netd

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func stateDir() string   { return darwinDir }
func socketPath() string { return filepath.Join(darwinDir, "netd.sock") }

// Install installs the helper beside Hostbay after one administrator prompt.
func Install() error {
	helper, err := besideSelf(helperName)
	if err != nil {
		return err
	}
	return installDarwin(helper, os.Getuid())
}

// Uninstall removes the helper after one administrator prompt.
func Uninstall() error { return uninstallDarwin() }

const launchdPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>serve</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
</dict>
</plist>
`

// SetUp is the root side of Install: "hostbay-netd install <uid>".
func SetUp(uid string) error {
	if err := validUID(uid); err != nil {
		return err
	}
	if err := setUpDir(darwinDir, uid); err != nil {
		return err
	}
	plist := fmt.Sprintf(launchdPlist, darwinLabel, filepath.Join(darwinDir, helperName))
	if err := writeFile(darwinPlist, []byte(plist), 0o644); err != nil {
		return err
	}
	runTool("/bin/launchctl", "bootout", "system/"+darwinLabel) // loaded only on a reinstall
	var err error
	for range 5 { // a job just booted out can take a moment to unload
		if err = runTool("/bin/launchctl", "bootstrap", "system", darwinPlist); err == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return err
}

// TearDown is the root side of Uninstall. Booting the daemon out stops it,
// and its last sweep removes Hostbay's routes.
func TearDown() error {
	runTool("/bin/launchctl", "bootout", "system/"+darwinLabel)
	if err := os.Remove(darwinPlist); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.RemoveAll(darwinDir)
}
