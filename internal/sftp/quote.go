package sftp

import "strings"

// Quote makes s one POSIX shell word: single quotes, with each embedded
// single quote closed, backslash-escaped and reopened. Inside single quotes
// sh expands nothing, so $(), backticks, ;, * and newlines stay literal. A
// leading dash is still an option to the command it is passed to; callers
// put -- before paths.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
