package agentexec

import (
	"slices"
	"strings"
)

// ReadOnly reports whether cmd only reads: one program, or a pipeline of
// them, from the list below, with no redirect, chaining, substitution or
// glob, and with every option on that program's allowlist. It is
// deliberately short; a command it does not know asks, as any other does.
// Options are allowed by name, never denied by name: a denylist misses
// abbreviations (--out for --output) and other spellings of what it meant
// to stop. It does not make reading harmless: output still goes to the
// client, so an argument that names a key, token or secret file is refused
// here too, as a best effort.
func ReadOnly(cmd string) bool {
	segs, ok := pipeline(cmd)
	if !ok {
		return false
	}
	for _, seg := range segs {
		if len(seg) == 0 {
			return false
		}
		spec, known := readOnlyProgs[seg[0]]
		if !known || !spec.allows(seg[1:]) {
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
		case strings.ContainsRune(";&<>`$()[]\\{}!\n\r\x00*?~", r):
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
	"token", ".pem", ".key", ".pgpass", ".netrc", ".aws", ".kube", ".gnupg", "history", "private", "passwd", "keytab", ".p12", ".pfx", "environ"}

func sensitiveArg(a string) bool {
	a = strings.ToLower(a)
	return slices.ContainsFunc(sensitiveParts, func(p string) bool { return strings.Contains(a, p) })
}

// progSpec is what one program may be given. A word that starts with - must
// be listed: as a long option by exact name (with =value or not), or as a
// cluster of the listed short letters and digits. Words that do not start
// with - are operands (paths, patterns, values of an option) and are free
// unless the program takes a verb or no operands at all.
type progSpec struct {
	anyOpt    bool     // every option is read-only: no list needed
	short     string   // allowed short option letters
	long      []string // allowed long option names, without the dashes
	verbs     []string // the first word must be one of these, exactly
	noOperand bool     // no word but options
	operands  []string // when set, the only operands allowed, exactly
	plus      bool     // operands must start with + (date +%s; date MMDDhhmm sets the clock)
	need      string   // an option the call must carry (docker stats --no-stream)
}

func (p progSpec) allows(args []string) bool {
	if len(p.verbs) > 0 {
		// The verb is the first word that is not an option: ip -br addr.
		i := slices.IndexFunc(args, func(a string) bool { return !strings.HasPrefix(a, "-") })
		if i < 0 || !slices.Contains(p.verbs, args[i]) {
			return false
		}
	}
	seenNeed := p.need == ""
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "--"):
			name, _, _ := strings.Cut(a[2:], "=")
			if !p.anyOpt && !slices.Contains(p.long, name) {
				return false
			}
			seenNeed = seenNeed || name == p.need
		case len(a) > 1 && a[0] == '-':
			for _, r := range a[1:] {
				if !p.anyOpt && !strings.ContainsRune(p.short, r) && (r < '0' || r > '9') {
					return false
				}
			}
		case p.noOperand:
			return false
		case p.plus && !strings.HasPrefix(a, "+"):
			return false
		case p.operands != nil && !slices.Contains(p.operands, a):
			return false
		}
	}
	return seenNeed
}

func anyOpts() progSpec { return progSpec{anyOpt: true} }

func shortOnly(letters string) progSpec { return progSpec{short: letters} }

var readOnlyProgs = map[string]progSpec{
	"pwd": anyOpts(), "whoami": anyOpts(), "id": anyOpts(), "uname": anyOpts(), "uptime": anyOpts(),
	"nproc": anyOpts(), "groups": anyOpts(), "lscpu": anyOpts(), "lsblk": anyOpts(), "w": anyOpts(), "who": anyOpts(),
	"last": anyOpts(), "df": anyOpts(), "du": anyOpts(), "free": anyOpts(), "ls": anyOpts(), "stat": anyOpts(),
	"wc": anyOpts(), "cut": anyOpts(), "tr": anyOpts(), "basename": anyOpts(), "dirname": anyOpts(), "which": anyOpts(),
	"netstat": anyOpts(), "cat": anyOpts(),
	// hostname with an operand sets the name.
	"hostname": {short: "fsiIdA", noOperand: true},
	"file":     {short: "bikLhsz", long: []string{"brief", "mime", "mime-type", "mime-encoding", "dereference"}},
	// head and tail read a file once; -f/-F follow and never return.
	"head": {short: "nqcv", long: []string{"lines", "bytes", "quiet", "silent", "verbose"}},
	"tail": {short: "nqcv", long: []string{"lines", "bytes", "quiet", "silent", "verbose"}},
	// grep without -r reads only the files it is named; -r would walk past
	// the names the sensitive-argument check can see.
	"grep":  grepSpec,
	"egrep": grepSpec,
	"fgrep": grepSpec,
	// sort -o and --compress-program write and run.
	"sort": {short: "nrufbdgMhVk" + "t", long: []string{"numeric-sort", "reverse", "unique", "ignore-case", "version-sort", "human-numeric-sort", "key", "field-separator"}},
	"date": {plus: true, short: "uRI", long: []string{"utc", "universal", "rfc-email", "iso-8601"}},
	"ps":   {operands: []string{"aux", "ax"}, short: "eAf"},
	// ss -D writes a file, -K kills sockets.
	"ss":         shortOnly("tuxlnaperHoOs46"),
	"ip":         {verbs: []string{"addr", "address", "link", "route", "neigh"}, short: "s46br", long: []string{"brief", "stats", "color"}, operands: []string{"addr", "address", "link", "route", "neigh", "show", "list"}},
	"systemctl":  {verbs: []string{"status", "is-active", "is-enabled", "is-failed", "list-units", "list-unit-files", "list-timers"}, short: "la", long: []string{"no-pager", "full", "all", "failed"}},
	"journalctl": {short: "unpbrqSU", long: []string{"no-pager", "unit", "lines", "priority", "boot", "reverse", "since", "until", "quiet", "utc"}},
	"docker":     {verbs: []string{"ps", "images", "version", "info", "logs"}, short: "aqlsnt", long: []string{"all", "quiet", "latest", "size", "no-trunc", "tail", "since", "until", "timestamps", "format", "filter"}},
}

var grepSpec = progSpec{
	short: "ivnclLwxEFGhHoqsaIAmBCe",
	long: []string{"ignore-case", "invert-match", "line-number", "count", "files-with-matches", "files-without-match", "word-regexp", "line-regexp",
		"extended-regexp", "fixed-strings", "no-filename", "with-filename", "only-matching", "quiet", "silent", "text", "max-count", "regexp",
		"after-context", "before-context", "context"},
}
