//go:build windows

package portowner

import (
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// x/sys/windows does not wrap GetExtendedTcpTable.
var procGetExtendedTcpTable = windows.NewLazySystemDLL("iphlpapi.dll").NewProc("GetExtendedTcpTable")

const tcpTableOwnerPIDListener = 3 // TCP_TABLE_OWNER_PID_LISTENER

// Name is the image name, without ".exe", of the process listening on
// TCP port over IPv4.
func Name(port int) (string, error) {
	size := uint32(16 << 10)
	var table []byte
	for range 4 { // the table can grow between the size call and the fill
		table = make([]byte, size)
		r, _, _ := procGetExtendedTcpTable.Call(uintptr(unsafe.Pointer(&table[0])), uintptr(unsafe.Pointer(&size)), 0,
			windows.AF_INET, tcpTableOwnerPIDListener, 0)
		if r == 0 {
			break
		}
		if syscall.Errno(r) != windows.ERROR_INSUFFICIENT_BUFFER {
			return "", syscall.Errno(r)
		}
		table = nil
	}
	pid := tablePID(table, port)
	if pid == 0 {
		return "", errors.New("no process listens on port " + strconv.Itoa(port))
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return "", err
	}
	name := filepath.Base(windows.UTF16ToString(buf[:n]))
	return strings.TrimSuffix(name, filepath.Ext(name)), nil
}

// InUse reports whether err is a listen that failed because the address is
// already in use.
func InUse(err error) bool {
	return errors.Is(err, windows.WSAEADDRINUSE) || errors.Is(err, syscall.EADDRINUSE)
}
