package agentexec

import "testing"

func TestReadOnly(t *testing.T) {
	yes := []string{
		"ls -la /var/log", "df -h", "uptime", "cat /etc/os-release", "tail -n 100 /var/log/syslog",
		"ps aux | grep nginx", `grep -E "error|warn" /var/log/app.log | tail -n 20`, "systemctl status nginx",
		"docker ps", "docker logs --tail 50 web", "git log --oneline -n 5", "kubectl get pods -n prod",
		"journalctl -u nginx -n 100", "ip addr", "date -u", "sort -u names.txt",
	}
	no := []string{
		"", "   ", "rm -rf /tmp/x", "ls; rm x", "ls && reboot", "ls > out", "cat a >> b", "echo $(whoami)", "ls `id`",
		"tail -f /var/log/syslog", "tail -nf 10 x", "journalctl -f", "docker logs -f web", "docker run alpine", "docker ps | sh",
		"sort -o out in", "date -s 2020-01-01", "ip link set eth0 down", "systemctl restart nginx", "kubectl get secrets",
		"kubectl delete pod x", "kubectl get pods -w", "git push", "git remote add x y", "sed -i s/a/b/ f", "find / -delete",
		"cat ~/.ssh/id_rsa", "cat /etc/shadow", "cat .env", "grep x /home/u/.aws/credentials", "ls |", "| ls", "ls || ls",
		"cat 'unterminated", "ls\nrm x", "bash -c ls", "env", "curl http://x", "ls *", "ps eww", "docker stats",
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
