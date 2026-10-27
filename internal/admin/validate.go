package admin

import (
	"errors"
	"fmt"
	"strings"
)

// Validators for every value the admin commands put into remote command
// text. They port the Rust allowlists and reject rather than escape; callers
// still shell-quote what passes.

func isAlnum(c rune) bool {
	return c < 0x80 && (c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9')
}

// ValidToken accepts ASCII letters and digits plus the punctuation in extra
// (docker.rs validate_token). A leading dash is refused too, which the Rust
// code missed: "-f" or "--help" would reach docker as an option.
func ValidToken(s, extra string) error {
	if s == "" {
		return errors.New("empty argument")
	}
	for _, c := range s {
		if !isAlnum(c) && !strings.ContainsRune(extra, c) {
			return fmt.Errorf("illegal characters in argument: %q", s)
		}
	}
	if s[0] == '-' {
		return fmt.Errorf("argument must not start with '-': %q", s)
	}
	return nil
}

// validUnit is the systemd unit rule from services.rs, which also accepted
// an empty name and a leading dash (an option to systemctl).
func validUnit(s string) error {
	if ValidToken(s, ".-_@:") != nil {
		return errors.New("invalid unit name")
	}
	return nil
}

// validIisName is iis.rs valid_name: letters, digits, space, '.', '_', '-'.
func validIisName(s string) error {
	if s == "" {
		return errors.New("invalid name")
	}
	for _, c := range s {
		if !isAlnum(c) && !strings.ContainsRune(" ._-", c) {
			return errors.New("invalid name")
		}
	}
	return nil
}

// ValidComposePath keeps docker.rs's ban on quotes and backticks, and adds
// a ban on newlines (find's output is read line by line) and on a leading
// dash, which find would take as an expression such as -delete.
func ValidComposePath(p string) error {
	if p == "" || p[0] == '-' || strings.ContainsAny(p, "'\"`\n\r") {
		return fmt.Errorf("illegal characters in path: %q", p)
	}
	return nil
}
