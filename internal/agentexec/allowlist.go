package agentexec

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"hostbay/internal/store"
)

// CheckBytes refuses a proposal holding a C0 or C1 control byte (newline,
// CR and tab included), DEL, a bidi control or a zero-width character, so
// what an approval card shows is exactly what runs (KTD32).
func CheckBytes(s string) error {
	for _, r := range s {
		switch {
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			return fmt.Errorf("the command holds the control character U+%04X; propose it on one line of visible characters", r)
		case store.Invisible(r):
			return fmt.Errorf("the command holds the invisible character U+%04X; propose it with visible characters only", r)
		}
	}
	return nil
}

// plainArg is an argument a prefix entry or placeholder accepts: no shell
// metacharacter, and not an option, so options match only as written.
var plainArg = regexp.MustCompile(`^[A-Za-z0-9._/:=@%+][A-Za-z0-9._/:=@%+-]*$`)

// words reads cmd as one simple command whose words are all plain
// literals: no expansion, substitution, glob, brace, tilde, quote, escape,
// redirect, assignment, comment or control operator. Anything else fails.
func words(cmd string) ([]string, bool) {
	if CheckBytes(cmd) != nil || strings.ContainsAny(cmd, "#\\") {
		return nil, false
	}
	f, err := syntax.NewParser().Parse(strings.NewReader(cmd), "")
	if err != nil || len(f.Stmts) != 1 || len(f.Last) > 0 {
		return nil, false
	}
	st := f.Stmts[0]
	if st.Negated || st.Background || st.Coprocess || len(st.Redirs) > 0 || st.Semicolon.IsValid() || len(st.Comments) > 0 {
		return nil, false
	}
	call, ok := st.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Assigns) > 0 || len(call.Args) == 0 {
		return nil, false
	}
	out := make([]string, 0, len(call.Args))
	for _, w := range call.Args {
		if len(w.Parts) != 1 {
			return nil, false
		}
		lit, ok := w.Parts[0].(*syntax.Lit)
		if !ok || strings.ContainsAny(lit.Value, "*?[]{}~$`'\"!&|;<>()") {
			return nil, false
		}
		out = append(out, lit.Value)
	}
	return out, true
}

// Match reports whether cmd may run without asking under entries. It can
// only when cmd is one simple command of plain literal words whose
// argument list equals an entry, or extends a prefix entry with plain
// arguments. A placeholder such as <unit> takes one plain argument.
func Match(cmd string, entries []store.AllowEntry) bool {
	argv, ok := words(cmd)
	if !ok {
		return false
	}
	for _, e := range entries {
		if matches(argv, e) {
			return true
		}
	}
	return false
}

func matches(argv []string, e store.AllowEntry) bool {
	if len(e.Argv) == 0 || len(argv) < len(e.Argv) || (!e.Prefix && len(argv) != len(e.Argv)) {
		return false
	}
	if e.Prefix && runsArgs(e.Argv[0]) {
		return false // saved before its program was listed
	}
	for i, w := range e.Argv {
		if placeholder(w) {
			if !plainArg.MatchString(argv[i]) {
				return false
			}
		} else if argv[i] != w {
			return false
		}
	}
	for _, a := range argv[len(e.Argv):] {
		if !plainArg.MatchString(a) {
			return false
		}
	}
	return true
}

func placeholder(w string) bool { return len(w) > 2 && w[0] == '<' && w[len(w)-1] == '>' }

// runsCode are programs that run their arguments, or a command named in
// them, as code: a prefix entry may not start with one (KTD33). It errs on
// the side of listing: an exact entry still allows any of them.
var runsCode = []string{
	// shells and interpreters
	"sh", "bash", "zsh", "dash", "ksh", "mksh", "ash", "yash", "csh", "tcsh", "fish", "rc", "elvish", "nu", "xonsh", "pwsh", "powershell",
	"cmd", "wsl", "tclsh", "wish", "expect", "awk", "gawk", "mawk", "nawk", "sed", "nodejs", "deno", "bun", "bunx", "npx", "pnpx",
	"Rscript", "julia", "runghc", "runhaskell", "scala", "groovy", "kotlin", "jshell", "java", "dotnet", "mono", "swift", "osascript",
	"erl", "escript", "elixir", "iex", "racket", "guile", "sbcl", "clisp", "ocaml", "gdb", "lldb", "busybox", "toybox",
	// shell builtins that run code or change what later commands run
	"eval", "exec", "command", "builtin", "source", ".", "trap", "alias",
	// programs that run a command they are given
	"env", "sudo", "su", "doas", "pkexec", "runuser", "sg", "newgrp", "setpriv", "xargs", "parallel", "find", "ssh", "sshpass",
	"nohup", "setsid", "nice", "ionice", "chrt", "taskset", "numactl", "prlimit", "cpulimit", "timeout", "time", "watch", "flock",
	"stdbuf", "unbuffer", "script", "rlwrap", "chronic", "entr", "strace", "ltrace", "valgrind", "perf", "tmux", "screen", "byobu",
	"dtach", "abduco", "chroot", "unshare", "nsenter", "ip", "systemd-run", "systemd-nspawn", "machinectl", "firejail", "bwrap",
	"proot", "fakeroot", "fakechroot", "torsocks", "proxychains", "proxychains4", "eatmydata", "chpst", "start-stop-daemon",
	"daemonize", "caffeinate", "arch", "sandbox-exec", "launchctl", "service", "run-parts", "crontab", "at", "batch",
	"socat", "ifne", "systemd-cat", "systemd-inhibit", "ssh-agent", "dbus-run-session", "dbus-launch", "xvfb-run", "run0",
	"setarch", "linux32", "linux64", "runcon", "faketime", "hyperfine", "srun", "salloc", "sbatch", "sem", "xcrun",
	// build and package tools that run project or package code
	"make", "ninja", "rake", "gradle", "mvn", "ant", "just", "npm", "pnpm", "yarn", "cargo", "go", "composer", "bundle", "gem",
	"uv", "uvx", "poetry", "pdm", "conda", "mamba", "tox", "nox", "pytest",
	// tools that run commands in containers, clusters or other hosts
	"git", "docker", "docker-compose", "podman", "nerdctl", "lxc", "lxc-attach", "kubectl", "oc", "ansible", "ansible-playbook",
	"terraform", "pdsh", "pssh", "flatpak", "snap", "crictl", "ctr", "incus", "buildah", "k3s", "microk8s",
}

// runsCodePrefix are names that come with a version, such as python3.12
// or pip3, and the dynamic loader, which runs the program it is given.
var runsCodePrefix = []string{"python", "pypy", "pip", "perl", "ruby", "irb", "php", "lua", "node", "tclsh", "wish", "ld-linux", "ld.so"}

func runsArgs(prog string) bool {
	prog = path.Base(prog)
	return slices.Contains(runsCode, prog) || slices.ContainsFunc(runsCodePrefix, func(p string) bool { return strings.HasPrefix(prog, p) })
}

// ParseEntry checks a command the user adds to a host's allowlist: plain
// literal words, and for a prefix entry, a first program that does not run
// its arguments as code.
func ParseEntry(cmd string, prefix bool) (store.AllowEntry, error) {
	argv, ok := words(strings.TrimSpace(cmd))
	if !ok {
		return store.AllowEntry{}, errors.New("an entry is one command of plain words: no quotes, variables, wildcards, redirects or operators")
	}
	if prefix && runsArgs(argv[0]) {
		return store.AllowEntry{}, fmt.Errorf("%s runs its arguments as code, so it can only be allowed with exact arguments", path.Base(argv[0]))
	}
	return store.AllowEntry{Argv: argv, Prefix: prefix}, nil
}

// Command is how an entry is written.
func Command(e store.AllowEntry) string { return strings.Join(e.Argv, " ") }
