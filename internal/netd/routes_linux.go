package netd

import (
	"context"
	"encoding/json"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// linuxTable reads and changes the main route table with iproute2's ip, by
// absolute path and argv.
type linuxTable struct{ ip string }

func newTable() Table {
	t := linuxTable{ip: "/usr/sbin/ip"}
	for _, p := range []string{"/usr/sbin/ip", "/sbin/ip", "/usr/bin/ip", "/bin/ip"} {
		if _, err := os.Stat(p); err == nil {
			t.ip = p
			break
		}
	}
	return t
}

func (linuxTable) Interfaces() ([]Iface, error) { return unixInterfaces() }

func (t linuxTable) Routes() ([]SysRoute, error) {
	var out []SysRoute
	for _, fam := range []string{"-4", "-6"} {
		b, err := exec.Command(t.ip, "-json", fam, "route", "show", "table", "main").Output()
		if err != nil && fam == "-6" {
			continue // a kernel booted with ipv6.disable=1 has no IPv6 table
		}
		if err != nil {
			return nil, err
		}
		var rows []struct{ Dst, Gateway, Dev string }
		if err := json.Unmarshal(b, &rows); err != nil {
			return nil, err
		}
		for _, row := range rows {
			if row.Dev == "" { // blackhole, unreachable
				continue
			}
			dst := row.Dst
			switch {
			case dst == "default" && fam == "-4":
				dst = "0.0.0.0/0"
			case dst == "default":
				dst = "::/0"
			case !strings.Contains(dst, "/"):
				a, err := netip.ParseAddr(dst)
				if err != nil {
					continue
				}
				dst = netip.PrefixFrom(a, a.BitLen()).String()
			}
			p, err := netip.ParsePrefix(dst)
			if err != nil {
				continue
			}
			r := SysRoute{Dst: p, Iface: row.Dev}
			r.Gateway, _ = netip.ParseAddr(row.Gateway)
			out = append(out, r)
		}
	}
	return out, nil
}

func (t linuxTable) change(verb string, r SysRoute) error {
	args := []string{"route", verb, r.Dst.String(), "dev", r.Iface}
	if r.Gateway.IsValid() {
		args = append(args, "via", r.Gateway.WithZone("").String())
	}
	return runTool(t.ip, args...)
}

func (t linuxTable) Add(r SysRoute) error    { return t.change("add", r) }
func (t linuxTable) Delete(r SysRoute) error { return t.change("del", r) }

func (t linuxTable) SetMTU(iface string, mtu int) error {
	return runTool(t.ip, "link", "set", "dev", iface, "mtu", strconv.Itoa(mtu))
}

// watchRoutes calls notify for each message on rtnetlink's IPv4 and IPv6
// route groups.
func watchRoutes(ctx context.Context, notify func()) error {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, unix.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: unix.RTMGRP_IPV4_ROUTE | unix.RTMGRP_IPV6_ROUTE}); err != nil {
		unix.Close(fd)
		return err
	}
	f := os.NewFile(uintptr(fd), "rtnetlink")
	context.AfterFunc(ctx, func() { f.Close() })
	buf := make([]byte, 64*1024)
	for {
		if _, err := f.Read(buf); err != nil {
			return err
		}
		notify()
	}
}
