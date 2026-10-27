package update

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Install swaps the running Hostbay.app for the one in the verified zip at
// path. The whole bundle moves, so the RDP helper inside it stays in step
// with the app. The new version runs from the next launch.
func Install(path string, _ Asset) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	// exe is Hostbay.app/Contents/MacOS/Hostbay.
	bundle := filepath.Dir(filepath.Dir(filepath.Dir(exe)))
	if !strings.HasSuffix(bundle, ".app") {
		return errors.New("Hostbay is not running from an app bundle; update it from the release page")
	}
	return swapBundle(path, bundle)
}

// swapBundle unpacks zip into a stage directory beside bundle, then swaps
// the two with renames on the same volume. A failure before the second
// rename leaves the installed bundle as it was.
func swapBundle(zip, bundle string) error {
	stage, err := os.MkdirTemp(filepath.Dir(bundle), ".hostbay-update-")
	if err != nil {
		return fmt.Errorf("stage the update beside %s: %w", bundle, err)
	}
	defer os.RemoveAll(stage)
	// ditto is how the release workflow packed the bundle, so it restores
	// the same modes and links.
	if out, err := exec.Command("/usr/bin/ditto", "-x", "-k", zip, stage).CombinedOutput(); err != nil {
		return fmt.Errorf("unpack the update: %v %s", err, out)
	}
	next := filepath.Join(stage, "Hostbay.app")
	if fi, err := os.Stat(next); err != nil || !fi.IsDir() {
		return errors.New("the update has no Hostbay.app")
	}
	// The old bundle moves into the stage, which the deferred RemoveAll
	// deletes; the running process keeps its open files.
	old := filepath.Join(stage, "old.app")
	if err := os.Rename(bundle, old); err != nil {
		return err
	}
	if err := os.Rename(next, bundle); err != nil {
		if rerr := os.Rename(old, bundle); rerr != nil {
			return fmt.Errorf("install the update: %v; restore the old app: %v", err, rerr)
		}
		return err
	}
	return nil
}
