package agentexec

import (
	"cmp"
	"regexp"
	"slices"
	"strings"
)

// ansi matches CSI, OSC, DCS/PM/APC and two-byte escape sequences.
var ansi = regexp.MustCompile("\x1b\\[[0-?]*[ -/]*[@-~]|\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)|\x1b[P^_][^\x1b]*\x1b\\\\|\x1b[@-Z\\\\-_]")

// removed stands in for a secret taken out of what is sent.
const removed = "[secret removed]"

// Scrub is command output as it may leave Hostbay, to a provider or an MCP
// client (KTD31): ANSI sequences and other control characters stripped,
// and every exact secret removed, before and after stripping, so colour
// codes cannot split one.
func Scrub(s string, secrets []string) string {
	s = scrub(s, secrets)
	s = ansi.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || (r >= 0x20 && r != 0x7f && (r < 0x80 || r > 0x9f)) {
			return r
		}
		return -1
	}, s)
	return scrub(s, secrets)
}

func scrub(s string, secrets []string) string {
	ss := slices.Clone(secrets)
	slices.SortFunc(ss, func(a, b string) int { return cmp.Compare(len(b), len(a)) }) // longest first
	for _, sec := range ss {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, removed)
		}
	}
	return s
}
