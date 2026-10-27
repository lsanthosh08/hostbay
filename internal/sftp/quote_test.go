package sftp

import "testing"

func TestQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/etc/hosts":         `'/etc/hosts'`, // ported from files.rs quote_wraps_and_rejects
		"":                   `''`,
		"/tmp/a b":           `'/tmp/a b'`,
		"/tmp/a'b":           `'/tmp/a'\''b'`, // files.rs refused this; it is now quoted
		"''":                 `''\'''\'''`,
		"/tmp/$(id)`id`;rm":  "'/tmp/$(id)`id`;rm'",
		"-rf":                `'-rf'`,
		"line1\nline2":       "'line1\nline2'",
		`back\slash "dq" $x`: `'back\slash "dq" $x'`,
	} {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %s, want %s", in, got, want)
		}
	}
}
