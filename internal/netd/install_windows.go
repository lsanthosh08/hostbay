package netd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// stateDir is under Program Files, which only administrators can write, and
// apart from Hostbay's own folder, which a per-user install puts in the
// user's profile and an uninstall removes whole.
func stateDir() string {
	pf, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, 0)
	if err != nil {
		pf = `C:\Program Files`
	}
	return filepath.Join(pf, "HostbayNetd")
}

// Install installs the helper beside Hostbay after one UAC prompt.
func Install() error {
	helper, err := besideSelf(helperName)
	if err != nil {
		return err
	}
	sid, err := currentUID()
	if err != nil {
		return err
	}
	return installWindows(helper, sid)
}

// Uninstall removes the helper after one UAC prompt. It runs the copy beside
// Hostbay: Windows cannot delete the installed one while it runs.
func Uninstall() error {
	helper, err := besideSelf(helperName)
	if err != nil {
		return err
	}
	return uninstallWindows(helper)
}

// SetUp is the root side of Install: "hostbay-netd.exe install <sid>",
// started elevated.
func SetUp(sid string) error {
	if _, err := windows.StringToSid(sid); err != nil || !sidPattern.MatchString(sid) {
		return fmt.Errorf("%q is not a user SID", sid)
	}
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(windowsService)
	if err == nil {
		defer s.Close()
		stop(s) // a running helper holds its binary open
	}
	dir := stateDir()
	if err := setUpDir(dir, sid); err != nil {
		return err
	}
	bin := filepath.Join(dir, exeName(helperName))
	if s == nil {
		s, err = m.CreateService(windowsService, bin, mgr.Config{
			DisplayName: "Hostbay network helper",
			Description: "Adds and removes routes through VPN tunnels for Hostbay.",
			StartType:   mgr.StartAutomatic,
		}, "serve")
		if err != nil {
			return err
		}
		defer s.Close()
	} else {
		cfg, err := s.Config()
		if err != nil {
			return err
		}
		cfg.BinaryPathName = syscall.EscapeArg(bin) + " serve"
		cfg.StartType = mgr.StartAutomatic
		if err := s.UpdateConfig(cfg); err != nil {
			return err
		}
	}
	return s.Start()
}

// TearDown is the root side of Uninstall. Stopping the service runs the
// helper's last sweep.
func TearDown() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	if s, err := m.OpenService(windowsService); err == nil {
		stop(s)
		err = s.Delete()
		s.Close()
		if err != nil {
			return err
		}
	}
	return removeAll(stateDir())
}

func stop(s *mgr.Service) {
	st, err := s.Control(svc.Stop)
	for i := 0; err == nil && st.State != svc.Stopped && i < 50; i++ {
		time.Sleep(200 * time.Millisecond)
		st, err = s.Query()
	}
}

func removeAll(dir string) error {
	var err error
	for range 10 { // the stopped service may hold its binary for a moment
		if err = os.RemoveAll(dir); err == nil {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return err
}

var (
	shell32             = windows.NewLazySystemDLL("shell32.dll")
	procShellExecuteExW = shell32.NewProc("ShellExecuteExW")
)

// shellExecuteInfo is SHELLEXECUTEINFOW.
type shellExecuteInfo struct {
	cbSize      uint32
	fMask       uint32
	hwnd        windows.Handle
	verb        *uint16
	file        *uint16
	parameters  *uint16
	directory   *uint16
	show        int32
	instApp     windows.Handle
	idList      uintptr
	class       *uint16
	keyClass    windows.Handle
	hotKey      uint32
	iconMonitor windows.Handle
	process     windows.Handle
}

// elevateNative starts file with the "runas" verb, which shows the UAC
// prompt, and waits for it to exit.
func elevateNative(file string, args ...string) (int, error) {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = syscall.EscapeArg(a)
	}
	verb, _ := windows.UTF16PtrFromString("runas")
	f, err := windows.UTF16PtrFromString(file)
	if err != nil {
		return 0, err
	}
	params, err := windows.UTF16PtrFromString(strings.Join(quoted, " "))
	if err != nil {
		return 0, err
	}
	const seeMaskNoCloseProcess = 0x40
	info := shellExecuteInfo{fMask: seeMaskNoCloseProcess, verb: verb, file: f, parameters: params}
	info.cbSize = uint32(unsafe.Sizeof(info))
	if r, _, err := procShellExecuteExW.Call(uintptr(unsafe.Pointer(&info))); r == 0 {
		if errors.Is(err, windows.ERROR_CANCELLED) {
			return 0, errElevationCancelled
		}
		return 0, err
	}
	defer windows.CloseHandle(info.process)
	if _, err := windows.WaitForSingleObject(info.process, windows.INFINITE); err != nil {
		return 0, err
	}
	var code uint32
	if err := windows.GetExitCodeProcess(info.process, &code); err != nil {
		return 0, err
	}
	return int(code), nil
}
