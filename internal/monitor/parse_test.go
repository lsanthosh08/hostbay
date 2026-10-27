package monitor

import (
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func fixture(t *testing.T, distro string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("../../testdata/monitor", distro, "sample.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func findPort(ps []Port, proto string, port int) *Port {
	for i := range ps {
		if ps[i].Proto == proto && ps[i].Port == port {
			return &ps[i]
		}
	}
	return nil
}

func findIface(is []Iface, name string) *Iface {
	for i := range is {
		if is[i].Name == name {
			return &is[i]
		}
	}
	return nil
}

// Each fixture is Script's output captured on that distribution; the
// expected figures are read off the fixture by hand.
func TestParseDistributions(t *testing.T) {
	type want struct {
		cores             int
		memTotal, memUsed uint64 // kB
		swapUsed          uint64 // kB
		load0             float64
		mounts            []string
		procs             int
		busiest           string
		busiestCPU        float64
		busiestRSS        uint64 // bytes
		port              Port
		iface             string
		addr              string
		mac               string
	}
	for distro, w := range map[string]want{
		"debian": {
			cores: 2, memTotal: 4022136, memUsed: 4022136 - 2871904, swapUsed: 1048572 - 996348, load0: 0.42,
			mounts: []string{"/", "/boot/efi", "/srv/backup data"}, procs: 9,
			busiest: "node /srv/app/server.js --port 3000", busiestCPU: 7.5, busiestRSS: 412880 * 1024,
			port: Port{Proto: "tcp", Addr: "*", Port: 3000, Process: "node"}, iface: "ens3", addr: "10.0.2.15/24", mac: "52:54:00:12:34:56",
		},
		"rhel": {
			cores: 4, memTotal: 16104952, memUsed: 16104952 - 9810344, swapUsed: 8257532 - 7781120, load0: 1.84,
			mounts: []string{"/", "/boot", "/home"}, procs: 9,
			busiest: "/usr/libexec/mysqld --basedir=/usr", busiestCPU: 4.1, busiestRSS: 1622312 * 1024,
			port: Port{Proto: "tcp", Addr: "127.0.0.1", Port: 3306}, iface: "docker0", addr: "172.17.0.1/16", mac: "02:42:9f:11:c0:7e",
		},
		"alpine": {
			cores: 1, memTotal: 493568, memUsed: 493568 - 401220, load0: 0.08,
			mounts: []string{"/", "/boot"}, procs: 7,
			// BusyBox ps reports no CPU, so memory decides.
			busiest: "/usr/bin/redis-server 127.0.0.1:6379", busiestCPU: -1, busiestRSS: 12 * 1024 * 1024,
			port: Port{Proto: "tcp", Addr: "127.0.0.1", Port: 6379}, iface: "eth0", addr: "10.0.2.15/24", mac: "08:00:27:5e:1a:44",
		},
	} {
		t.Run(distro, func(t *testing.T) {
			r := parse(fixture(t, distro))
			s := finish(nil, r, 0)
			if r.os != "Linux" || s.Cores != w.cores || s.Load[0] != w.load0 {
				t.Errorf("os %q cores %d load %v", r.os, s.Cores, s.Load)
			}
			if s.CPU <= 0 || s.CPU >= 100 {
				t.Errorf("cpu %v", s.CPU)
			}
			if s.MemTotal != w.memTotal*1024 || s.MemUsed != w.memUsed*1024 || s.SwapUsed != w.swapUsed*1024 {
				t.Errorf("mem %d/%d swap %d", s.MemUsed, s.MemTotal, s.SwapUsed)
			}
			var mounts []string
			for _, d := range s.Disks {
				mounts = append(mounts, d.Mount)
				if d.Used == 0 || d.Size <= d.Used {
					t.Errorf("disk %+v", d)
				}
			}
			if strings.Join(mounts, ",") != strings.Join(w.mounts, ",") {
				t.Errorf("mounts %q", mounts)
			}
			if s.ProcCount != w.procs || len(s.Procs) != w.procs {
				t.Fatalf("procs %d (%d kept)", s.ProcCount, len(s.Procs))
			}
			if p := s.Procs[0]; p.Command != w.busiest || p.CPU != w.busiestCPU || p.RSS != w.busiestRSS || p.User == "" {
				t.Errorf("busiest %+v", p)
			}
			if p := findPort(s.Ports, w.port.Proto, w.port.Port); p == nil || *p != w.port {
				t.Errorf("port %+v in %+v", p, s.Ports)
			}
			if findPort(s.Ports, "tcp6", 22) == nil {
				t.Errorf("no tcp6 :22 in %+v", s.Ports)
			}
			i := findIface(s.Ifaces, w.iface)
			if i == nil || i.MAC != w.mac || i.MTU != 1500 || i.State == "" || !strings.Contains(strings.Join(i.Addrs, " "), w.addr) {
				t.Errorf("iface %+v", i)
			}
			if r.c.net[w.iface][0] == 0 || r.c.net[w.iface][1] == 0 {
				t.Errorf("net counters %v", r.c.net)
			}
		})
	}
}

func TestParseNetstatFallback(t *testing.T) {
	// net-tools as root names processes, trailing args cut at the column.
	ps := parseNetstat(strings.Split(`Active Internet connections (only servers)
Proto Recv-Q Send-Q Local Address           Foreign Address         State       PID/Program name
tcp        0      0 0.0.0.0:22              0.0.0.0:*               LISTEN      601/sshd: /usr/sbin
tcp6       0      0 :::80                   :::*                    LISTEN      790/nginx: master p
udp        0      0 0.0.0.0:68              0.0.0.0:*                           512/dhclient`, "\n"))
	want := []Port{{"tcp", "0.0.0.0", 22, "sshd"}, {"tcp6", "::", 80, "nginx"}, {"udp", "0.0.0.0", 68, "dhclient"}}
	if len(ps) != len(want) {
		t.Fatalf("ports %+v", ps)
	}
	for i := range want {
		if ps[i] != want[i] {
			t.Errorf("port %d = %+v, want %+v", i, ps[i], want[i])
		}
	}
}

func TestParseProcNetFallback(t *testing.T) {
	out := "OS::Linux\n:PROCNET::\n@tcp\n" +
		"  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
		"   0: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 18311 1 0000000000000000 100 0 0 10 0\n" +
		"   1: 0100007F:18EB 00000000:0000 0A 00000000:00000000 00:00000000 00000000   100        0 21023 1 0000000000000000 100 0 0 10 0\n" +
		"   2: 0F02000A:0016 0202000A:D3A2 01 00000000:00000000 02:000A3D70 00000000     0        0 31337 4 0000000000000000 20 4 29 10 -1\n" +
		"@tcp6\n" +
		"  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
		"   0: 00000000000000000000000000000000:0016 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 18313 1 0000000000000000 100 0 0 10 0\n" +
		"   1: 00000000000000000000000001000000:0277 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 18320 1 0000000000000000 100 0 0 10 0\n" +
		"@udp\n" +
		"   sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode ref pointer drops\n" +
		"  981: 00000000:0044 00000000:0000 07 00000000:00000000 00:00000000 00000000     0        0 15862 2 0000000000000000 0\n" +
		"@udp6\n"
	got := parse(out).s.Ports
	want := []Port{{"tcp", "0.0.0.0", 22, ""}, {"tcp6", "::", 22, ""}, {"udp", "0.0.0.0", 68, ""}, {"tcp6", "::1", 631, ""}, {"tcp", "127.0.0.1", 6379, ""}}
	if len(got) != len(want) {
		t.Fatalf("ports %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("port %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseBusyBoxPS(t *testing.T) {
	// Bare BusyBox ps, and the /proc loop Script runs without ps.
	for _, out := range []string{
		"PID   USER     TIME  COMMAND\n    1 root      0:01 /sbin/init\n  412 nobody    0:00 /usr/sbin/dnsmasq -k --user=nobody\n",
		"PID COMMAND\n1 /sbin/init\n412 /usr/sbin/dnsmasq -k --user=nobody\n",
	} {
		ps := parsePS(strings.Split(out, "\n"))
		if len(ps) != 2 || ps[1].PID != 412 || ps[1].Command != "/usr/sbin/dnsmasq -k --user=nobody" || ps[1].CPU != -1 {
			t.Errorf("%q: %+v", out, ps)
		}
	}
}

func TestParseCapsProcessesAndCommandLines(t *testing.T) {
	var b strings.Builder
	b.WriteString(":PS::\n    PID USER     %CPU   RSS COMMAND\n")
	for i := range 80 {
		b.WriteString("    " + strconv.Itoa(100+i) + " u 1.0 10 " + strings.Repeat("x", 400) + "\n")
	}
	s := finish(nil, parse(b.String()), 0)
	if s.ProcCount != 80 || len(s.Procs) != TopProcs || len([]rune(s.Procs[0].Command)) != MaxCommand {
		t.Fatalf("count %d kept %d len %d", s.ProcCount, len(s.Procs), len(s.Procs[0].Command))
	}
}

func netdev(rx, tx uint64) string {
	return "OS::Linux\n:STAT::\ncpu 100 0 100 800 0 0 0 0\n:NETDEV::\n  eth0: " +
		u(rx) + " 0 0 0 0 0 0 0 " + u(tx) + " 0 0 0 0 0 0 0\n"
}

func u(n uint64) string { return strconv.FormatUint(n, 10) }

func TestRatesFromDeltas(t *testing.T) {
	prev := parse(netdev(1000, 5000))
	s := finish(&prev.c, parse(netdev(4000, 5600)), 2)
	if s.RxRate != 1500 || s.TxRate != 300 {
		t.Fatalf("rates %v %v", s.RxRate, s.TxRate)
	}
}

func TestCounterWrapGivesNoNegativeRate(t *testing.T) {
	// A 32-bit counter wraps from near the top; a 64-bit one was reset.
	prev := parse(netdev(math.MaxUint32-999, 1<<40))
	s := finish(&prev.c, parse(netdev(1000, 10)), 1)
	if s.RxRate != 2000 || s.TxRate != 0 {
		t.Fatalf("rates %v %v", s.RxRate, s.TxRate)
	}
	if i := findIface(s.Ifaces, "eth0"); i != nil && (i.RxRate < 0 || i.TxRate < 0) {
		t.Fatalf("iface %+v", i)
	}
}

func TestCPUFromDeltas(t *testing.T) {
	prev := parse(":STAT::\ncpu 100 0 100 800 0 0 0 0\n")
	cur := parse(":STAT::\ncpu 175 0 125 900 0 0 0 0\n") // 100 busy of 200
	if s := finish(&prev.c, cur, 1); s.CPU != 50 {
		t.Fatalf("cpu %v", s.CPU)
	}
}

func TestUnsupportedSystem(t *testing.T) {
	if r := parse("OS::Darwin\n"); r.os != "Darwin" {
		t.Fatalf("os %q", r.os)
	}
}

// A command line from the host that holds a section line cannot start a
// section: it does not know this run's nonce. The ps fallback over /proc
// also folds a command line's newlines, as a Debian slim image showed.
func TestSectionsNeedTheRunsNonce(t *testing.T) {
	out := "NONCE::ab12\nOS::Linux\nab12:PS::\nPID COMMAND\n7 sh -c x\nSS::\n:SS::\ncd34:SS::\ntcp LISTEN 0 1 0.0.0.0:4444 0.0.0.0:*\nab12:SS::\n"
	m := sections(out)
	if len(m["PS"]) != 6 || len(m["SS"]) != 1 || m["SS"][0] != "" {
		t.Fatalf("%q", m)
	}
	if ps := parse(out).s.Ports; len(ps) != 0 {
		t.Fatalf("forged ports %+v", ps)
	}
	if !strings.Contains(Script, `tr '\0\n' '  '`) {
		t.Fatal("the /proc fallback no longer folds newlines in command lines")
	}
}

// Script picks ss, then netstat, then /proc/net/tcp. It runs here under
// /bin/sh with a PATH holding only stand-ins and the tools it needs, so the
// test host's own ss or netstat cannot answer.
func TestScriptPortFallbacks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs /bin/sh")
	}
	for _, tc := range []struct {
		tools []string
		want  string
	}{
		{[]string{"ss", "netstat"}, "SS::"},
		{[]string{"netstat"}, "NETSTAT::"},
		{nil, "PROCNET::"},
	} {
		dir := t.TempDir()
		stub := func(name, body string) {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		stub("uname", "echo Linux")
		for _, tool := range tc.tools {
			stub(tool, "echo "+tool+" ran")
		}
		for _, tool := range []string{"cat", "grep", "df", "ps", "tr", "od"} {
			if p, err := exec.LookPath(tool); err == nil {
				os.Symlink(p, filepath.Join(dir, tool))
			}
		}
		cmd := exec.Command("/bin/sh", "-c", Script)
		cmd.Env = []string{"PATH=" + dir}
		out, _ := cmd.Output()
		m := sections(string(out))
		key := strings.TrimSuffix(tc.want, "::")
		if m[key] == nil {
			t.Errorf("tools %v: no %s section in\n%s", tc.tools, tc.want, out)
		}
		for _, other := range []string{"SS", "NETSTAT", "PROCNET"} {
			if other != key && m[other] != nil {
				t.Errorf("tools %v: also ran %s", tc.tools, other)
			}
		}
		if len(tc.tools) > 0 && !strings.Contains(strings.Join(m[key], "\n"), tc.tools[0]+" ran") {
			t.Errorf("tools %v: %q", tc.tools, m[key])
		}
	}
}

func FuzzParse(f *testing.F) {
	for _, d := range []string{"debian", "rhel", "alpine"} {
		b, err := os.ReadFile(filepath.Join("../../testdata/monitor", d, "sample.txt"))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(string(b))
	}
	f.Add("OS::Linux\n:PROCNET::\n@tcp6\n 0: zz:zz 0 0A\n:IPJ::\n[{]\n:PS::\nPID\n1\n")
	f.Fuzz(func(t *testing.T, out string) {
		prev := parse(out)
		s := finish(&prev.c, parse(out+"\n"+out), 1)
		if s.CPU < 0 || s.CPU > 100 || s.RxRate < 0 || s.TxRate < 0 || len(s.Procs) > TopProcs {
			t.Fatalf("sample %+v", s)
		}
	})
}
