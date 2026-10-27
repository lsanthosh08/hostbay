package mcp

import "golang.org/x/sys/windows"

// stillActive is GetExitCodeProcess's code for a process still running.
const stillActive = 259

// alive reports whether process pid exists.
func alive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	return windows.GetExitCodeProcess(h, &code) == nil && code == stillActive
}
