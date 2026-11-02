package agentexec

import (
	"slices"
	"strings"
)

// ReadOnly reports whether cmd only reads: one program, or a pipeline of
// them, from the list below, with no redirect, chaining, substitution or
// option known to write, follow or run something. It is deliberately
// short; a command it does not know asks, as any other does. It does not
// make reading harmless: output still goes to the client, so an argument
// that names a key, token or secret file is refused here too.
func ReadOnly(cmd string) bool {
	segs, ok := pipeline(cmd)
	if !ok {
		return false
	}
	for _, seg := range segs {
		if len(seg) == 0 {
			return false
		}
		check, known := readOnlyProgs[seg[0]]
		if !known || !check(seg[1:]) {
			return false
		}
		for _, a := range seg[1:] {
			if sensitiveArg(a) {
				return false
			}
		}
	}
	return len(segs) > 0
}

// pipeline splits cmd on | into words, honouring quotes. Anything that
// chains, redirects, substitutes or expands is refused, quoted or not: the
// shell on the far side would act on it.
func pipeline(cmd string) ([][]string, bool) {
	var segs [][]string
	var cur []string
	var w strings.Builder
	var quote rune
	started := false
	flush := func() {
		if started {
			cur = append(cur, w.String())
			w.Reset()
			started = false
		}
	}
	for _, r := range cmd {
		switch {
		case strings.ContainsRune(";&<>`$()\\{}!\n\r\x00*?~", r):
			return nil, false
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				w.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, started = r, true
		case r == '|':
			flush()
			segs, cur = append(segs, cur), nil
		case r == ' ' || r == '\t':
			flush()
		default:
			w.WriteRune(r)
			started = true
		}
	}
	if quote != 0 {
		return nil, false
	}
	flush()
	return append(segs, cur), true
}

var sensitiveParts = []string{".ssh", "id_rsa", "id_ed25519", "id_ecdsa", "id_dsa", "shadow", ".env", "secret", "credential",
	"token", ".pem", ".key", ".pgpass", ".netrc", ".aws", ".kube", ".gnupg", "history", "private", "passwd", "keytab", ".p12", ".pfx"}

func sensitiveArg(a string) bool {
	a = strings.ToLower(a)
	return slices.ContainsFunc(sensitiveParts, func(p string) bool { return strings.Contains(a, p) })
}

// anyArg is the check for a program that cannot write or follow whatever
// its arguments are.
func anyArg([]string) bool { return true }

// without refuses the listed options, long or short, and a short option
// cluster holding one of the short ones (-fn holds -f).
func without(bad ...string) func([]string) bool {
	return func(args []string) bool {
		for _, a := range args {
			if slices.Contains(bad, a) {
				return false
			}
			if len(a) > 1 && a[0] == '-' && a[1] != '-' {
				for _, b := range bad {
					if len(b) == 2 && strings.Contains(a[1:], b[1:]) {
						return false
					}
				}
			}
			for _, b := range bad {
				if len(b) > 2 && strings.HasPrefix(a, b+"=") {
					return false
				}
			}
		}
		return true
	}
}

// sub allows a program only when its first argument is one of subs and no
// later argument is in bad.
func sub(subs []string, bad ...string) func([]string) bool {
	return func(args []string) bool {
		return len(args) > 0 && slices.Contains(subs, args[0]) && !slices.ContainsFunc(args[1:], func(a string) bool { return slices.Contains(bad, a) })
	}
}

var follow = []string{"-f", "-F", "--follow", "--follow=name", "--follow=descriptor"}

var readOnlyProgs = map[string]func([]string) bool{
	"pwd": anyArg, "whoami": anyArg, "id": anyArg, "hostname": without("-b", "-F", "--file"), "uname": anyArg,
	"uptime": anyArg, "df": anyArg, "du": anyArg, "free": anyArg, "w": anyArg, "who": anyArg, "last": anyArg,
	"lsblk": anyArg, "lscpu": anyArg, "nproc": anyArg, "arch": anyArg, "groups": anyArg, "which": anyArg,
	"ls": anyArg, "stat": anyArg, "wc": anyArg, "cut": anyArg, "tr": anyArg, "basename": anyArg, "dirname": anyArg,
	"cat": anyArg, "head": anyArg, "file": without("-C", "--compile"), "sort": without("-o", "--output", "-T", "--temporary-directory"),
	"tail": without(follow...), "grep": anyArg, "egrep": anyArg, "fgrep": anyArg,
	"date": func(args []string) bool {
		for _, a := range args {
			if a != "-u" && a != "-R" && a != "-I" && !strings.HasPrefix(a, "+") {
				return false
			}
		}
		return true
	},
	"ps": func(args []string) bool {
		for _, a := range args {
			if !slices.Contains([]string{"aux", "ax", "-e", "-A", "-f", "-ef", "-aux", "-ax"}, a) {
				return false
			}
		}
		return true
	},
	"ss": without("-K", "--kill"), "netstat": anyArg,
	"ip":         sub([]string{"addr", "address", "a", "link", "l", "route", "r", "neigh", "n"}, "add", "del", "delete", "flush", "set", "change", "replace"),
	"systemctl":  sub([]string{"status", "is-active", "is-enabled", "is-failed", "list-units", "list-unit-files", "list-timers", "show", "cat"}),
	"journalctl": without(append(slices.Clone(follow), "--vacuum-size", "--vacuum-time", "--vacuum-files", "--flush", "--rotate", "--sync", "--setup-keys", "--update-catalog")...),
	"docker":     dockerReadOnly,
	"git":        sub([]string{"status", "log", "diff", "show", "rev-parse", "describe", "remote"}, "--output", "-o", "set-url", "add", "remove", "rm", "rename", "prune"),
	"kubectl":    kubectlReadOnly,
}

func dockerReadOnly(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "ps", "images", "version", "info", "inspect", "top", "port", "diff":
		return true
	case "logs", "stats", "events":
		return without(append(slices.Clone(follow), "--follow")...)(args[1:]) && (args[0] != "stats" || slices.Contains(args, "--no-stream")) && args[0] != "events"
	}
	return false
}

func kubectlReadOnly(args []string) bool {
	if len(args) == 0 || !slices.Contains([]string{"get", "describe", "logs", "top", "version", "cluster-info"}, args[0]) {
		return false
	}
	// A Secret's data is the secret.
	if slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(strings.ToLower(a), "secret") }) {
		return false
	}
	return without(append(slices.Clone(follow), "--follow", "-w", "--watch")...)(args[1:])
}
