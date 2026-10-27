package monitor

import (
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"
)

// TopProcs is how many processes a sample carries, and MaxCommand how many
// characters of each command line (KTD29).
const (
	TopProcs   = 50
	MaxCommand = 256
)

// Sample is one tick's figures. Sizes are bytes and rates bytes per second.
// Partial is set when the output hit the cap and was cut.
type Sample struct {
	Time      int64      `json:"time"` // Unix milliseconds
	Partial   bool       `json:"partial,omitempty"`
	CPU       float64    `json:"cpu"` // percent busy across all cores
	Cores     int        `json:"cores"`
	Load      [3]float64 `json:"load"`
	MemTotal  uint64     `json:"memTotal"`
	MemUsed   uint64     `json:"memUsed"`
	SwapTotal uint64     `json:"swapTotal"`
	SwapUsed  uint64     `json:"swapUsed"`
	RxRate    float64    `json:"rxRate"`
	TxRate    float64    `json:"txRate"`
	Disks     []Disk     `json:"disks"`
	Ifaces    []Iface    `json:"ifaces"`
	ProcCount int        `json:"procCount"`
	Procs     []Proc     `json:"procs"`
	Ports     []Port     `json:"ports"`
}

type Disk struct {
	FS    string `json:"fs"`
	Mount string `json:"mount"`
	Size  uint64 `json:"size"`
	Used  uint64 `json:"used"`
}

type Iface struct {
	Name   string   `json:"name"`
	State  string   `json:"state"`
	MAC    string   `json:"mac"`
	MTU    int      `json:"mtu"`
	Addrs  []string `json:"addrs"`
	RxRate float64  `json:"rxRate"`
	TxRate float64  `json:"txRate"`
}

// Proc is one process. CPU is -1 where ps cannot report it (BusyBox).
type Proc struct {
	PID     int     `json:"pid"`
	User    string  `json:"user"`
	CPU     float64 `json:"cpu"`
	RSS     uint64  `json:"rss"`
	Command string  `json:"command"`
}

// Port is one listening socket. Process is empty when the socket belongs
// to another user's process, which a sampler without sudo cannot name.
type Port struct {
	Proto   string `json:"proto"` // tcp, tcp6, udp, udp6
	Addr    string `json:"addr"`
	Port    int    `json:"port"`
	Process string `json:"process,omitempty"`
}

// counters are the raw cumulative figures of one output, from which two
// consecutive samples make rates.
type counters struct {
	cpuTotal, cpuIdle uint64
	net               map[string][2]uint64 // interface -> rx, tx bytes
}

// raw is one parsed output before rates.
type raw struct {
	os string
	s  Sample
	c  counters
}

// sectionKeys are the KEY:: lines Script prints. Only these start a section.
var sectionKeys = map[string]bool{
	"STAT": true, "MEMINFO": true, "LOADAVG": true, "DF": true, "NETDEV": true, "PS": true,
	"SS": true, "NETSTAT": true, "PROCNET": true, "IPJ": true, "SYSNET": true, "IPO": true,
}

// sections splits Script's output into its sections by key, plus OS. A
// section line is "<nonce>:KEY::", with the nonce from the NONCE:: line the
// script printed first, so a process name or command line from another
// user on the host cannot start a section of its own.
func sections(out string) map[string][]string {
	m := map[string][]string{}
	key, nonce := "", ""
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if key == "" {
			if v, ok := strings.CutPrefix(line, "NONCE::"); ok {
				nonce = v
				continue
			}
			if v, ok := strings.CutPrefix(line, "OS::"); ok {
				m["OS"] = []string{strings.TrimSpace(v)}
				continue
			}
		}
		if h, ok := strings.CutPrefix(line, nonce+":"); ok {
			if k, ok := strings.CutSuffix(h, "::"); ok && sectionKeys[k] {
				key = k
				continue
			}
		}
		if key != "" {
			m[key] = append(m[key], line)
		}
	}
	return m
}

// parse reads one output of Script. It never fails: what a section lacks
// stays zero or empty.
func parse(out string) raw {
	m := sections(out)
	r := raw{c: counters{net: map[string][2]uint64{}}}
	if v := m["OS"]; len(v) > 0 {
		r.os = v[0]
	}
	parseStat(m["STAT"], &r)
	parseMeminfo(m["MEMINFO"], &r.s)
	parseLoad(m["LOADAVG"], &r.s)
	r.s.Disks = parseDF(m["DF"])
	parseNetDev(m["NETDEV"], &r.c)
	r.s.Procs = parsePS(m["PS"])
	switch {
	case m["SS"] != nil:
		r.s.Ports = parseSS(m["SS"])
	case m["NETSTAT"] != nil:
		r.s.Ports = parseNetstat(m["NETSTAT"])
	default:
		r.s.Ports = parseProcNet(m["PROCNET"])
	}
	if m["IPJ"] != nil {
		r.s.Ifaces = parseIPJSON(strings.Join(m["IPJ"], "\n"))
	} else {
		r.s.Ifaces = parseSysNet(m["SYSNET"], m["IPO"])
	}
	slices.SortFunc(r.s.Ports, func(a, b Port) int {
		if a.Port != b.Port {
			return a.Port - b.Port
		}
		return strings.Compare(a.Proto+a.Addr, b.Proto+b.Addr)
	})
	return r
}

func atou(s string) uint64 { n, _ := strconv.ParseUint(s, 10, 64); return n }

func parseStat(lines []string, r *raw) {
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) == 0 {
			continue
		}
		if f[0] != "cpu" {
			if strings.HasPrefix(f[0], "cpu") {
				r.s.Cores++
			}
			continue
		}
		// user nice system idle iowait irq softirq steal; guest time is
		// already counted in user.
		for i, v := range f[1:min(len(f), 9)] {
			n := atou(v)
			r.c.cpuTotal += n
			if i == 3 || i == 4 {
				r.c.cpuIdle += n
			}
		}
	}
}

func parseMeminfo(lines []string, s *Sample) {
	kb := map[string]uint64{}
	for _, l := range lines {
		k, v, ok := strings.Cut(l, ":")
		if f := strings.Fields(v); ok && len(f) > 0 {
			kb[k] = atou(f[0]) * 1024
		}
	}
	avail, ok := kb["MemAvailable"]
	if !ok { // kernels before 3.14
		avail = kb["MemFree"] + kb["Buffers"] + kb["Cached"]
	}
	s.MemTotal = kb["MemTotal"]
	s.MemUsed = s.MemTotal - min(avail, s.MemTotal)
	s.SwapTotal = kb["SwapTotal"]
	s.SwapUsed = s.SwapTotal - min(kb["SwapFree"], s.SwapTotal)
}

func parseLoad(lines []string, s *Sample) {
	if len(lines) == 0 {
		return
	}
	for i, v := range strings.Fields(lines[0]) {
		if i == 3 {
			break
		}
		s.Load[i], _ = strconv.ParseFloat(v, 64)
	}
}

// pseudoFS are df rows that are not disks.
var pseudoFS = map[string]bool{"tmpfs": true, "devtmpfs": true, "udev": true, "none": true, "shm": true, "run": true, "efivarfs": true, "cgroup": true, "cgroup2": true}

// parseDF reads `df -Pk`: Filesystem, 1024-blocks, Used, Available,
// Capacity, Mounted on (which may hold spaces).
func parseDF(lines []string) []Disk {
	out := []Disk{}
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) < 6 || f[0] == "Filesystem" || pseudoFS[f[0]] || strings.HasPrefix(f[5], "/snap/") {
			continue
		}
		size, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil || size == 0 {
			continue
		}
		out = append(out, Disk{FS: f[0], Mount: strings.Join(f[5:], " "), Size: size * 1024, Used: atou(f[2]) * 1024})
	}
	return out
}

// parseNetDev reads /proc/net/dev: "name: rx_bytes ... (8 rx fields)
// tx_bytes ...". Old kernels print no space after the colon.
func parseNetDev(lines []string, c *counters) {
	for _, l := range lines {
		name, rest, ok := strings.Cut(l, ":")
		f := strings.Fields(rest)
		if !ok || len(f) < 9 {
			continue
		}
		c.net[strings.TrimSpace(name)] = [2]uint64{atou(f[0]), atou(f[8])}
	}
}

// size parses a ps size column: KiB, or BusyBox's 12m, 1.5g.
func size(s string) uint64 {
	mul := 1024.0
	switch {
	case strings.HasSuffix(s, "k"):
		s = s[:len(s)-1]
	case strings.HasSuffix(s, "m"):
		s, mul = s[:len(s)-1], 1024*1024
	case strings.HasSuffix(s, "g"):
		s, mul = s[:len(s)-1], 1024*1024*1024
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 || math.IsInf(v, 0) || math.IsNaN(v) {
		return 0
	}
	return uint64(v * mul)
}

// parsePS reads ps output by its header, since each fallback prints other
// columns: procps (PID USER %CPU RSS COMMAND), BusyBox -o (PID USER RSS
// COMMAND), bare BusyBox (PID USER TIME COMMAND), or the /proc loop (PID
// COMMAND). COMMAND is last and takes the rest of the line. Only the top
// TopProcs by CPU, then memory, are kept; Sample.ProcCount has the total.
func parsePS(lines []string) []Proc {
	var cols []string
	out := []Proc{}
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) == 0 {
			continue
		}
		if f[0] == "PID" {
			cols = f
			continue
		}
		if len(cols) == 0 || len(f) < len(cols) {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		p := Proc{PID: pid, CPU: -1}
		for i, c := range cols[:len(cols)-1] {
			switch c {
			case "USER":
				p.User = f[i]
			case "%CPU":
				p.CPU, _ = strconv.ParseFloat(f[i], 64)
			case "RSS":
				p.RSS = size(f[i])
			}
		}
		p.Command = strings.Join(f[len(cols)-1:], " ")
		if len(p.Command) > MaxCommand { // no more bytes means no more runes
			if r := []rune(p.Command); len(r) > MaxCommand {
				p.Command = string(r[:MaxCommand])
			}
		}
		out = append(out, p)
	}
	return out
}

// top keeps the busiest n processes.
func top(ps []Proc, n int) []Proc {
	slices.SortStableFunc(ps, func(a, b Proc) int {
		if a.CPU != b.CPU {
			if a.CPU > b.CPU {
				return -1
			}
			return 1
		}
		if a.RSS != b.RSS {
			if a.RSS > b.RSS {
				return -1
			}
			return 1
		}
		return a.PID - b.PID
	})
	return ps[:min(len(ps), n)]
}

// hostPort splits a local address such as 0.0.0.0:22, [::]:22, *:22,
// :::22 or 127.0.0.53%lo:53 at its last colon.
func hostPort(s string) (string, int, bool) {
	i := strings.LastIndexByte(s, ':')
	if i < 0 {
		return "", 0, false
	}
	port, err := strconv.Atoi(s[i+1:])
	if err != nil {
		return "", 0, false
	}
	return strings.Trim(s[:i], "[]"), port, true
}

// ssProcess names the first process in ss's users:(("sshd",pid=1,fd=3)).
func ssProcess(f []string) string {
	for _, v := range f {
		if rest, ok := strings.CutPrefix(v, `users:(("`); ok {
			name, _, _ := strings.Cut(rest, `"`)
			return name
		}
	}
	return ""
}

// parseSS reads `ss -tulnp`: Netid State Recv-Q Send-Q Local Peer [users].
func parseSS(lines []string) []Port {
	out := []Port{}
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) < 5 || (f[0] != "tcp" && f[0] != "udp") {
			continue
		}
		addr, port, ok := hostPort(f[4])
		if !ok {
			continue
		}
		proto := f[0]
		if strings.Contains(addr, ":") {
			proto += "6"
		}
		out = append(out, Port{Proto: proto, Addr: addr, Port: port, Process: ssProcess(f[5:])})
	}
	return out
}

// parseNetstat reads `netstat -tulnp` (net-tools or BusyBox): Proto Recv-Q
// Send-Q Local Foreign [State] PID/Program, where PID/Program is "-" for
// another user's socket.
func parseNetstat(lines []string) []Port {
	out := []Port{}
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) < 5 || !strings.HasPrefix(f[0], "tcp") && !strings.HasPrefix(f[0], "udp") {
			continue
		}
		addr, port, ok := hostPort(f[3])
		if !ok {
			continue
		}
		p := Port{Proto: f[0], Addr: addr, Port: port}
		for _, v := range f[5:] {
			if pid, name, ok := strings.Cut(v, "/"); ok {
				if _, err := strconv.Atoi(pid); err == nil {
					p.Process = strings.TrimSuffix(name, ":") // net-tools: "sshd: /usr/sbin"
					break
				}
			}
		}
		if p.Proto == "tcp" || p.Proto == "udp" {
			if strings.Contains(addr, ":") {
				p.Proto += "6"
			}
		}
		out = append(out, p)
	}
	return out
}

// parseProcNet reads /proc/net/{tcp,tcp6,udp,udp6}, each after an @name
// line. Addresses are hex, each 32-bit word in host (little-endian) order.
// Listening TCP is state 0A; an unconnected UDP socket is 07.
// shortcut: reads the words as little-endian, which every common Linux
// host is; a big-endian host (s390x) shows swapped addresses until this
// learns its byte order.
func parseProcNet(lines []string) []Port {
	out := []Port{}
	proto := ""
	for _, l := range lines {
		if p, ok := strings.CutPrefix(l, "@"); ok {
			proto = p
			continue
		}
		f := strings.Fields(l)
		if len(f) < 4 || !strings.HasSuffix(f[0], ":") {
			continue
		}
		want := "07"
		if strings.HasPrefix(proto, "tcp") {
			want = "0A"
		}
		if f[3] != want {
			continue
		}
		a, p, ok := strings.Cut(f[1], ":")
		port, err := strconv.ParseUint(p, 16, 16)
		ip := hexIP(a)
		if !ok || err != nil || ip == "" {
			continue
		}
		out = append(out, Port{Proto: proto, Addr: ip, Port: int(port)})
	}
	return out
}

func hexIP(h string) string {
	if len(h) != 8 && len(h) != 32 {
		return ""
	}
	b := make([]byte, 0, len(h)/2)
	for w := 0; w < len(h); w += 8 {
		v, err := strconv.ParseUint(h[w:w+8], 16, 32)
		if err != nil {
			return ""
		}
		b = append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
	}
	if len(b) == 4 {
		return strconv.Itoa(int(b[0])) + "." + strconv.Itoa(int(b[1])) + "." + strconv.Itoa(int(b[2])) + "." + strconv.Itoa(int(b[3]))
	}
	var parts []string
	for i := 0; i < 16; i += 2 {
		parts = append(parts, strconv.FormatUint(uint64(b[i])<<8|uint64(b[i+1]), 16))
	}
	return compactIPv6(parts)
}

// compactIPv6 joins eight groups, folding the first longest zero run to ::.
func compactIPv6(g []string) string {
	best, bestLen := -1, 1
	for i := 0; i < len(g); {
		j := i
		for j < len(g) && g[j] == "0" {
			j++
		}
		if j-i > bestLen {
			best, bestLen = i, j-i
		}
		i = max(j, i+1)
	}
	if best < 0 {
		return strings.Join(g, ":")
	}
	return strings.Join(g[:best], ":") + "::" + strings.Join(g[best+bestLen:], ":")
}

// parseIPJSON reads `ip -j addr show`.
func parseIPJSON(s string) []Iface {
	var in []struct {
		Name      string `json:"ifname"`
		State     string `json:"operstate"`
		MAC       string `json:"address"`
		MTU       int    `json:"mtu"`
		AddrInfos []struct {
			Local  string `json:"local"`
			Prefix int    `json:"prefixlen"`
		} `json:"addr_info"`
	}
	if json.Unmarshal([]byte(s), &in) != nil {
		return []Iface{}
	}
	out := []Iface{}
	for _, i := range in {
		ifc := Iface{Name: i.Name, State: strings.ToLower(i.State), MAC: i.MAC, MTU: i.MTU, Addrs: []string{}}
		for _, a := range i.AddrInfos {
			if a.Local != "" {
				ifc.Addrs = append(ifc.Addrs, a.Local+"/"+strconv.Itoa(a.Prefix))
			}
		}
		out = append(out, ifc)
	}
	return out
}

// parseSysNet reads the /sys/class/net loop's "name operstate mac mtu"
// lines, with addresses from `ip -o addr show` ("2: eth0 inet 10.0.0.5/24
// ...") where ip is there.
func parseSysNet(sys, ipo []string) []Iface {
	out := []Iface{}
	at := map[string]int{}
	for _, l := range sys {
		f := strings.Fields(l)
		if len(f) < 2 { // bonding_masters is a file; an unmatched glob prints "*"
			continue
		}
		ifc := Iface{Name: f[0], Addrs: []string{}}
		if len(f) > 1 {
			ifc.State = f[1]
		}
		if len(f) > 2 {
			ifc.MAC = f[2]
		}
		if len(f) > 3 {
			ifc.MTU, _ = strconv.Atoi(f[3])
		}
		at[ifc.Name] = len(out)
		out = append(out, ifc)
	}
	for _, l := range ipo {
		f := strings.Fields(l)
		if len(f) < 4 || (f[2] != "inet" && f[2] != "inet6") {
			continue
		}
		if i, ok := at[strings.TrimSuffix(f[1], ":")]; ok {
			out[i].Addrs = append(out[i].Addrs, f[3])
		}
	}
	return out
}

// delta is how far a cumulative counter moved. A counter lower than before
// either wrapped at 32 bits (it was near the top) or was reset, which
// counts as no movement, so a rate is never negative.
func delta(prev, cur uint64) uint64 {
	if cur >= prev {
		return cur - prev
	}
	if prev <= math.MaxUint32 && prev-cur > math.MaxUint32/2 {
		return cur + (math.MaxUint32 + 1 - prev)
	}
	return 0
}

// finish turns cur into a Sample, with rates against prev over secs
// seconds. Without prev, CPU is the average since boot and rates are 0.
func finish(prev *counters, cur raw, secs float64) Sample {
	s := cur.s
	dTotal, dIdle := cur.c.cpuTotal, cur.c.cpuIdle
	if prev != nil && cur.c.cpuTotal > prev.cpuTotal {
		dTotal, dIdle = cur.c.cpuTotal-prev.cpuTotal, delta(prev.cpuIdle, cur.c.cpuIdle)
	}
	if dTotal > 0 {
		s.CPU = min(100, 100*float64(dTotal-min(dIdle, dTotal))/float64(dTotal)) // float rounding can pass 100
	}
	rates := map[string][2]float64{}
	if prev != nil && secs > 0 {
		for name, c := range cur.c.net {
			if p, ok := prev.net[name]; ok {
				rates[name] = [2]float64{float64(delta(p[0], c[0])) / secs, float64(delta(p[1], c[1])) / secs}
			}
		}
	}
	for name, r := range rates {
		if name != "lo" {
			s.RxRate += r[0]
			s.TxRate += r[1]
		}
	}
	for i := range s.Ifaces {
		r := rates[s.Ifaces[i].Name]
		s.Ifaces[i].RxRate, s.Ifaces[i].TxRate = r[0], r[1]
	}
	s.ProcCount = len(s.Procs)
	s.Procs = top(s.Procs, TopProcs)
	return s
}
