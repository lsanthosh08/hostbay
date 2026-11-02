package agentexec

import (
	"slices"
	"testing"
)

func TestReadOnly(t *testing.T) {
	yes := []string{
		"ls -la /var/log", "df -h", "uptime", "cat /etc/os-release", "tail -n 100 /var/log/syslog", "head -5 notes.txt",
		"ps aux | grep nginx", `grep -E "error|warn" /var/log/app.log | tail -n 20`, "grep -in --max-count=5 timeout app.log",
		"systemctl status nginx", "systemctl list-units --failed --no-pager", "docker ps -a", "docker logs --tail 50 web",
		"journalctl -u nginx -n 100 --no-pager", "ip addr", "ip -br addr", "ip link show", "ip route", "date -u", "date +%s",
		"sort -u names.txt", "sort -rn -k2 sizes.txt", "hostname", "hostname -f", "ss -tlnp", "file -b data.bin", "ps -ef",
	}
	no := []string{
		"", "   ", "rm -rf /tmp/x", "ls; rm x", "ls && reboot", "ls > out", "cat a >> b", "echo $(whoami)", "ls `id`",
		"tail -f /var/log/syslog", "tail -nf 10 x", "tail --fo x", "journalctl -f", "journalctl --rot", "journalctl --vacuum-s=1M",
		"journalctl --vacuum-size=1M", "docker logs -f web", "docker run alpine", "docker inspect web", "docker ps | sh", "docker stats",
		"sort -o out in", "sort --out=x in", "sort --compress-program=sh in", "sort -T /tmp in",
		"date -s 2020-01-01", "date 010112002020", "ip link set eth0 down", "ip l s eth0 down", "ip a a 10.0.0.1/24 dev eth0",
		"ip addr add 10.0.0.1/24 dev eth0", "ip netns exec x sh", "ss -D out.bin", "ss -K", "hostname evil", "hostname -b x",
		"systemctl restart nginx", "systemctl status -Hroot@host nginx", "systemctl show nginx", "kubectl get pods", "git log --output=/tmp/x",
		"git diff --output=.bashrc", "git status", "sed -i s/a/b/ f", "find / -delete", "arch rm x", "arch -x86_64 sh",
		"cat ~/.ssh/id_rsa", "cat /etc/shadow", "cat /etc/shado[w]", "cat .env", "cat /proc/1/environ", "grep x /home/u/.aws/credentials",
		"grep -r password .", "grep --recursive x .", "grep --recur x .", "grep -f patterns.txt file", "ls |", "| ls", "ls || ls",
		"cat 'unterminated", "ls\nrm x", "bash -c ls", "env", "curl http://x", "ls *", "ps eww", "ps aux e", "file -C", "file --compile",
	}
	for _, c := range yes {
		if !ReadOnly(c) {
			t.Errorf("ReadOnly(%q) = false, want true", c)
		}
	}
	for _, c := range no {
		if ReadOnly(c) {
			t.Errorf("ReadOnly(%q) = true, want false", c)
		}
	}
}

// A program that runs its arguments as code is read-only here only when its
// spec names the verbs that cannot.
func TestReadOnlyProgramsThatRunCodeNameTheirVerbs(t *testing.T) {
	for prog, spec := range readOnlyProgs {
		if runsArgs(prog) && len(spec.verbs) == 0 {
			t.Errorf("%s runs its arguments as code and ReadOnly allows it with any operand", prog)
		}
	}
	if !slices.Contains(readOnlyProgs["ip"].verbs, "addr") {
		t.Fatal("ip lost its verbs")
	}
}
