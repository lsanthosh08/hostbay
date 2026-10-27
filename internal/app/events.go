package app

// Event names the UI subscribes to (src/api.ts). Each carries the ID the
// backend returned, so one tab never sees another's stream.

func PtyDataEvent(ptyID string) string       { return "pty://" + ptyID }
func PtyClosedEvent(ptyID string) string     { return "pty-closed://" + ptyID }
func ConnStateEvent(sessionID string) string { return "conn-state://" + sessionID }
func RdpFrameEvent(rdpID string) string      { return "rdp-frame://" + rdpID }
func RdpSizeEvent(rdpID string) string       { return "rdp-size://" + rdpID }
func RdpClosedEvent(rdpID string) string     { return "rdp-closed://" + rdpID }
func RdpClipboardEvent(rdpID string) string  { return "rdp-clipboard://" + rdpID }
func RdpClipFilesEvent(rdpID string) string  { return "rdp-clip-files://" + rdpID }

// QueryStateEvent carries a database query's state (database.State): at
// most every 100 ms while it runs, then done, error, cancelled or expired.
func QueryStateEvent(queryID string) string { return "query-state://" + queryID }

// ZmodemEvent fires when `sz` or `rz` starts in an SSH terminal; its data is
// "download" or "upload" (see ZmodemStreamName).
func ZmodemEvent(ptyID string) string { return "zmodem://" + ptyID }

// MonitorEvent carries a Monitor tab's monitor.Event each tick: its
// sampler's state and the new sample.
func MonitorEvent(monitorID string) string { return "monitor-sample://" + monitorID }

// AIEvent carries one AI conversation's ai.Event stream.
func AIEvent(convID string) string { return "ai://" + convID }

// AIApprovalsEvent carries the app-wide approval tray's whole list
// ([]agentexec.Pending) whenever it changes.
const AIApprovalsEvent = "ai-approvals"

// TransferEvent carries every transfer task's progress (transfer.Event).
// It is one app-wide name because the transfer panel shows all tasks; each
// payload names its taskId.
const TransferEvent = "transfer"

// TerminalNoticeEvent carries a line the terminal's tab shows about its
// tools: a post-login rule that stopped, or a log that could not be written.
func TerminalNoticeEvent(ptyID string) string { return "terminal-notice://" + ptyID }
