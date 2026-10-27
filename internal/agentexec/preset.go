package agentexec

import "hostbay/internal/store"

// Preset is Hostbay's built-in read-only list for "dangerous plus write"
// mode (KTD33): fixed-argument introspection and no general file reader,
// because output goes to the provider.
var Preset = []store.AllowEntry{
	{Argv: []string{"uptime"}},
	{Argv: []string{"df", "-h"}},
	{Argv: []string{"free", "-m"}},
	{Argv: []string{"whoami"}},
	{Argv: []string{"hostname"}},
	{Argv: []string{"id"}},
	{Argv: []string{"date"}},
	{Argv: []string{"uname", "-a"}},
	{Argv: []string{"systemctl", "status", "<unit>"}},
	{Argv: []string{"docker", "ps"}},
}
