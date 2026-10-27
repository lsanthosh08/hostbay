package container

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

type Container struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Image  string `json:"image"`
	Status string `json:"status"`
	State  string `json:"state"`
	Ports  string `json:"ports"`
}

type Image struct {
	ID         string `json:"id"`
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	Size       string `json:"size"`
	Created    string `json:"created"`
}

type Stat struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	CPU     string `json:"cpu"`
	Mem     string `json:"mem"`
	MemPerc string `json:"memPerc"`
}

// decode reads an engine's JSON list output: one array (Podman, WSLC) or
// one object per line (Docker, nerdctl; lines that are not JSON, such as
// warnings, are skipped). An item for which ok is false did not decode.
// Output that is not empty but yields no item is an error, so a shape the
// decoder does not know never shows as an empty list.
func decode[T any](kind Kind, out string, ok func(T) bool) ([]T, error) {
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	var items []T
	if strings.HasPrefix(out, "[") {
		if json.Unmarshal([]byte(out), &items) != nil {
			items = nil
		}
	} else {
		for _, line := range strings.Split(out, "\n") {
			var v T
			if json.Unmarshal([]byte(strings.TrimSpace(line)), &v) == nil {
				items = append(items, v)
			}
		}
	}
	items = slices.DeleteFunc(items, func(v T) bool { return !ok(v) })
	if len(items) == 0 && out != "[]" {
		return nil, fmt.Errorf("%s output not understood", kind)
	}
	return items, nil
}

// num reads a JSON number or string as text: WSLC prints numbers where
// Docker prints strings.
type num string

func (n *num) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*n = num(s)
		return nil
	}
	var f json.Number
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	*n = num(f.String())
	return nil
}

// names reads Podman's Names, a list, or Docker's, a string.
type names []string

func (n *names) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*n = names{s}
		return nil
	}
	return json.Unmarshal(b, (*[]string)(n))
}

func (n names) first() string {
	if len(n) == 0 {
		return ""
	}
	return n[0]
}

// The structs below follow each engine's output, checked against the
// fixtures in testdata/containers.

type dockerPs struct{ ID, Names, Image, Status, State, Ports string }

type podmanPort struct {
	HostIP        string `json:"host_ip"`
	ContainerPort int    `json:"container_port"`
	HostPort      int    `json:"host_port"`
	Range         int    `json:"range"`
	Protocol      string `json:"protocol"`
}

type podmanPs struct {
	ID     string `json:"Id"`
	Names  names
	Image  string
	State  string
	Status string
	Ports  []podmanPort
}

type wslcPort struct {
	BindingAddress string
	ContainerPort  num
	HostPort       num
	Protocol       num // 6 is TCP, 17 UDP
}

type wslcPs struct {
	ID    string `json:"Id"`
	Name  string
	Image string
	State num
	Ports []wslcPort
}

func decodeContainers(kind Kind, out string) ([]Container, error) {
	switch kind {
	case Podman:
		l, err := decode(kind, out, func(c podmanPs) bool { return c.ID != "" })
		return convert(l, func(c podmanPs) Container {
			ports := make([]string, len(c.Ports))
			for i, p := range c.Ports {
				ports[i] = podmanPortText(p)
			}
			return Container{c.ID, c.Names.first(), c.Image, c.Status, c.State, strings.Join(ports, ", ")}
		}), err
	case WSLC:
		l, err := decode(kind, out, func(c wslcPs) bool { return c.ID != "" })
		return convert(l, func(c wslcPs) Container {
			ports := make([]string, len(c.Ports))
			for i, p := range c.Ports {
				ports[i] = portText(p.BindingAddress, string(p.HostPort), string(p.ContainerPort), map[num]string{"17": "udp"}[p.Protocol])
			}
			state := wslcState(string(c.State))
			return Container{c.ID, c.Name, c.Image, state, state, strings.Join(ports, ", ")}
		}), err
	}
	l, err := decode(kind, out, func(c dockerPs) bool { return c.ID != "" })
	return convert(l, func(c dockerPs) Container {
		state := c.State
		if state == "" { // nerdctl prints no State, only "Up", "Created" or "Exited (0) ..."
			state = stateOf(c.Status)
		}
		return Container{c.ID, c.Names, c.Image, c.Status, state, c.Ports}
	}), err
}

// portText writes a published port as Docker does: 0.0.0.0:8080->80/tcp.
func portText(hostIP, hostPort, containerPort, proto string) string {
	if proto == "" {
		proto = "tcp"
	}
	if hostPort == "" || hostPort == "0" {
		return containerPort + "/" + proto
	}
	if hostIP == "" {
		hostIP = "0.0.0.0"
	}
	return hostIP + ":" + hostPort + "->" + containerPort + "/" + proto
}

// podmanPortText writes a Podman port, whose range covers that many ports.
func podmanPortText(p podmanPort) string {
	hp, cp := strconv.Itoa(p.HostPort), strconv.Itoa(p.ContainerPort)
	if p.Range > 1 {
		hp += "-" + strconv.Itoa(p.HostPort+p.Range-1)
		cp += "-" + strconv.Itoa(p.ContainerPort+p.Range-1)
	}
	return portText(p.HostIP, hp, cp, p.Protocol)
}

// wslcState names WSLC's numeric states as Docker does (uniterm's parse.go).
func wslcState(s string) string {
	switch s {
	case "0", "1":
		return "created"
	case "2":
		return "running"
	case "3", "5":
		return "exited"
	case "4":
		return "paused"
	}
	return s
}

func stateOf(status string) string {
	switch {
	case strings.HasPrefix(status, "Up"):
		return "running"
	case strings.HasPrefix(status, "Exited"):
		return "exited"
	}
	return strings.ToLower(strings.Fields(status + " ")[0])
}

type dockerImage struct{ ID, Repository, Tag, Size, CreatedSince string }

type podmanImage struct {
	ID       string `json:"Id"`
	RepoTags []string
	Names    []string
	Size     int64
	Created  int64
}

type wslcImage struct {
	ID         string `json:"Id"`
	Repository string
	Tag        string
	Size       int64
	CreatedAt  int64
}

func decodeImages(kind Kind, out string) ([]Image, error) {
	switch kind {
	case Podman:
		l, err := decode(kind, out, func(i podmanImage) bool { return i.ID != "" })
		return convert(l, func(i podmanImage) Image {
			repo, tag := "<none>", "<none>"
			if ref := append(i.RepoTags, i.Names...); len(ref) > 0 {
				repo, tag = splitRef(ref[0])
			}
			return Image{i.ID, repo, tag, humanSize(i.Size), unixDate(i.Created)}
		}), err
	case WSLC:
		l, err := decode(kind, out, func(i wslcImage) bool { return i.ID != "" })
		return convert(l, func(i wslcImage) Image {
			return Image{i.ID, i.Repository, i.Tag, humanSize(i.Size), unixDate(i.CreatedAt)}
		}), err
	}
	l, err := decode(kind, out, func(i dockerImage) bool { return i.ID != "" })
	return convert(l, func(i dockerImage) Image { return Image{i.ID, i.Repository, i.Tag, i.Size, i.CreatedSince} }), err
}

// splitRef splits "docker.io/library/nginx:1.27" at the tag's colon, not a
// registry port's.
func splitRef(ref string) (repo, tag string) {
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[:i], ref[i+1:]
	}
	return ref, "<none>"
}

// humanSize writes bytes as Docker does: decimal units, three digits.
func humanSize(n int64) string {
	f, units := float64(n), []string{"B", "kB", "MB", "GB", "TB"}
	i := 0
	for f >= 1000 && i < len(units)-1 {
		f /= 1000
		i++
	}
	return strconv.FormatFloat(f, 'g', 3, 64) + units[i]
}

func unixDate(sec int64) string {
	if sec == 0 {
		return ""
	}
	return time.Unix(sec, 0).Format("2006-01-02 15:04")
}

type dockerStat struct{ ID, Name, CPUPerc, MemUsage, MemPerc string }

type podmanStat struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	CPUPercent string `json:"cpu_percent"`
	MemUsage   string `json:"mem_usage"`
	MemPercent string `json:"mem_percent"`
}

func decodeStats(kind Kind, out string) ([]Stat, error) {
	if kind == Podman {
		l, err := decode(kind, out, func(s podmanStat) bool { return s.ID != "" })
		return convert(l, func(s podmanStat) Stat { return Stat{s.ID, s.Name, s.CPUPercent, s.MemUsage, s.MemPercent} }), err
	}
	// shortcut: WSLC's stats shape is unrecorded and decoded as Docker's;
	// "wslc output not understood" says when the Windows check finds otherwise.
	l, err := decode(kind, out, func(s dockerStat) bool { return s.ID != "" })
	return convert(l, func(s dockerStat) Stat { return Stat{s.ID, s.Name, s.CPUPerc, s.MemUsage, s.MemPerc} }), err
}

// inspected is the part of inspect the confirmations read. Docker, Podman
// and nerdctl print Mounts and HostConfig; WSLC prints mounts, which JSON
// field matching also reads into Mounts.
type inspected struct {
	ID         string `json:"Id"`
	HostConfig struct{ Privileged bool }
	Mounts     []struct{ Type, Source string }
}

func decodeExposure(kind Kind, out string) (Exposure, error) {
	l, err := decode(kind, out, func(i inspected) bool { return i.ID != "" })
	if err != nil || len(l) == 0 {
		return Exposure{}, fmt.Errorf("%s output not understood", kind)
	}
	x := Exposure{Privileged: l[0].HostConfig.Privileged}
	for _, m := range l[0].Mounts {
		if m.Type == "bind" || m.Type == "" { // a mount of no stated type counts as a bind

			x.Binds = append(x.Binds, m.Source)
		}
	}
	return x, nil
}

// mergeLogs interleaves two streams of `logs -t` lines by their leading
// RFC 3339 timestamp, stdout first on a tie.
func mergeLogs(stdout, stderr string) string {
	split := func(s string) []string {
		if s = strings.TrimRight(s, "\n"); s == "" {
			return nil
		}
		return strings.Split(s, "\n")
	}
	type line struct {
		t time.Time
		s string
	}
	lines := convert(append(split(stdout), split(stderr)...), func(s string) line {
		var t time.Time
		if f := strings.Fields(s); len(f) > 0 {
			t, _ = time.Parse(time.RFC3339Nano, f[0])
		}
		return line{t, s}
	})
	slices.SortStableFunc(lines, func(a, b line) int { return a.t.Compare(b.t) })
	return strings.Join(convert(lines, func(l line) string { return l.s }), "\n")
}

func convert[T, U any](in []T, f func(T) U) []U {
	out := make([]U, len(in))
	for i, v := range in {
		out[i] = f(v)
	}
	return out
}
