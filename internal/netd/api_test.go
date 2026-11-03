package netd

import "testing"

func TestValidateRefusesBadRequests(t *testing.T) {
	add := func(r Route) Msg { return Msg{T: Add, V: Version, Route: &r} }
	bad := map[string]Msg{
		"shell in dst":        add(Route{Dst: "10.0.0.0/8; reboot", Iface: "utun3"}),
		"substitution in dst": add(Route{Dst: "$(reboot)", Iface: "utun3"}),
		"host bits":           add(Route{Dst: "10.0.0.1/8", Iface: "utun3"}),
		"no prefix length":    add(Route{Dst: "10.0.0.0", Iface: "utun3"}),
		"mapped v6":           add(Route{Dst: "::ffff:10.0.0.0/104", Iface: "utun3"}),
		"shell in iface":      add(Route{Dst: "10.0.0.0/8", Iface: "utun3;reboot"}),
		"option as iface":     add(Route{Dst: "10.0.0.0/8", Iface: "-interface"}),
		"backtick iface":      add(Route{Dst: "10.0.0.0/8", Iface: "`id`"}),
		"newline iface":       add(Route{Dst: "10.0.0.0/8", Iface: "utun3\nreboot"}),
		"iface and gateway":   add(Route{Dst: "10.0.0.0/8", Iface: "utun3", Gateway: true}),
		"neither":             add(Route{Dst: "10.0.0.0/8"}),
		"no route":            {T: Add, V: Version},
		"delete both":         {T: Delete, V: Version, Route: &Route{Dst: "10.0.0.0/8", Iface: "utun3", Gateway: true}},
		"delete no iface":     {T: Delete, V: Version, Route: &Route{Dst: "10.0.0.0/8"}},
		"mtu 100":             {T: SetMTU, V: Version, Iface: "utun3", MTU: 100},
		"mtu 9001":            {T: SetMTU, V: Version, Iface: "utun3", MTU: 9001},
		"mtu shell iface":     {T: SetMTU, V: Version, Iface: "utun3 && reboot", MTU: 1400},
		"unknown":             {T: "exec", V: Version},
		"proxy https":         {T: SetProxy, V: Version, URL: "https://127.0.0.1:8080/hostbay-pac/abcdefghijklmnop/1.pac"},
		"proxy remote host":   {T: SetProxy, V: Version, URL: "http://10.0.0.1:8080/hostbay-pac/abcdefghijklmnop/1.pac"},
		"proxy localhost":     {T: SetProxy, V: Version, URL: "http://localhost:8080/hostbay-pac/abcdefghijklmnop/1.pac"},
		"proxy no port":       {T: SetProxy, V: Version, URL: "http://127.0.0.1/hostbay-pac/abcdefghijklmnop/1.pac"},
		"proxy port 0":        {T: SetProxy, V: Version, URL: "http://127.0.0.1:0/hostbay-pac/abcdefghijklmnop/1.pac"},
		"proxy quote":         {T: SetProxy, V: Version, URL: `http://127.0.0.1:8080/hostbay-pac/abc"defghijklmnop/1.pac`},
		"proxy other path":    {T: SetProxy, V: Version, URL: "http://127.0.0.1:8080/other.pac"},
		"proxy query":         {T: SetProxy, V: Version, URL: "http://127.0.0.1:8080/hostbay-pac/abcdefghijklmnop/1.pac?v=2"},
		"proxy none":          {T: SetProxy, V: Version},
	}
	for name, m := range bad {
		if err := validate(m); err == nil || err.Code != CodeInvalid {
			t.Errorf("%s: validate = %v, want %s", name, err, CodeInvalid)
		}
	}

	good := map[string]Msg{
		"add via tunnel":  add(Route{Dst: "10.0.0.0/8", Iface: "utun3"}),
		"add via gateway": add(Route{Dst: "203.0.113.7/32", Gateway: true}),
		"add v6":          add(Route{Dst: "fd00::/8", Iface: "wg0"}),
		"windows name":    add(Route{Dst: "10.0.0.0/8", Iface: "OpenVPN Wintun (2)"}),
		"delete":          {T: Delete, V: Version, Route: &Route{Dst: "0.0.0.0/1", Iface: "utun5"}},
		"delete gateway":  {T: Delete, V: Version, Route: &Route{Dst: "203.0.113.7/32", Gateway: true}},
		"mtu":             {T: SetMTU, V: Version, Iface: "utun3", MTU: 1400},
		"proxy":           {T: SetProxy, V: Version, URL: "http://127.0.0.1:8080/hostbay-pac/abcdefghijklmnop/12.pac"},
		"proxy clear":     {T: ClearProxy, V: Version},
	}
	for name, m := range good {
		if err := validate(m); err != nil {
			t.Errorf("%s: validate = %v", name, err)
		}
	}
}
