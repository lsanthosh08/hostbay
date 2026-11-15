package tunnel

import (
	"net/netip"
	"slices"
	"testing"
)

// The darwin oc-script configures only the utun the helper chose, named as
// its argument, and only with host addresses a tunnel can carry; anything
// else from openconnect's environment is ignored or refused.
func TestParseScript(t *testing.T) {
	ok := map[string]struct {
		env  map[string]string
		want []string
	}{
		"ipv4":           {map[string]string{"INTERNAL_IP4_ADDRESS": "10.62.4.7", "INTERNAL_IP4_NETMASKLEN": "32"}, []string{"10.62.4.7/32"}},
		"ipv4 netmask":   {map[string]string{"INTERNAL_IP4_ADDRESS": "10.62.4.7", "INTERNAL_IP4_NETMASKLEN": "16"}, []string{"10.62.4.7/32"}},
		"ipv4 no mask":   {map[string]string{"INTERNAL_IP4_ADDRESS": "10.62.4.7"}, []string{"10.62.4.7/32"}},
		"ipv6 prefix":    {map[string]string{"INTERNAL_IP6_ADDRESS": "fd00::5", "INTERNAL_IP6_NETMASK": "fd00::5/64"}, []string{"fd00::5/128"}},
		"ipv6 from mask": {map[string]string{"INTERNAL_IP6_NETMASK": "fd00::5/64"}, []string{"fd00::5/128"}},
		"both":           {map[string]string{"INTERNAL_IP4_ADDRESS": "172.16.58.9", "INTERNAL_IP6_ADDRESS": "2001:db8::9"}, []string{"172.16.58.9/32", "2001:db8::9/128"}},
	}
	for name, c := range ok {
		c.env["reason"], c.env["TUNDEV"] = "connect", "utun9"
		iface, addrs, err := parseScript([]string{"utun9"}, getenv(c.env))
		if err != nil || iface != "utun9" || !slices.Equal(strs(addrs), c.want) {
			t.Errorf("%s: got %q %v %v, want utun9 %v", name, iface, addrs, err, c.want)
		}
	}

	bad := map[string]struct {
		args []string
		env  map[string]string
	}{
		"other utun":     {[]string{"utun9"}, map[string]string{"TUNDEV": "utun3", "INTERNAL_IP4_ADDRESS": "10.0.0.5"}},
		"shell in tun":   {[]string{"utun3"}, map[string]string{"TUNDEV": "utun3; rm -rf /", "INTERNAL_IP4_ADDRESS": "10.0.0.5"}},
		"shell in arg":   {[]string{"utun3; rm -rf /"}, map[string]string{"TUNDEV": "utun3; rm -rf /", "INTERNAL_IP4_ADDRESS": "10.0.0.5"}},
		"not a utun":     {[]string{"en0"}, map[string]string{"TUNDEV": "en0", "INTERNAL_IP4_ADDRESS": "10.0.0.5"}},
		"no arg":         {nil, map[string]string{"TUNDEV": "", "INTERNAL_IP4_ADDRESS": "10.0.0.5"}},
		"two args":       {[]string{"utun9", "utun9"}, map[string]string{"TUNDEV": "utun9", "INTERNAL_IP4_ADDRESS": "10.0.0.5"}},
		"no address":     {[]string{"utun9"}, map[string]string{"TUNDEV": "utun9"}},
		"unparsable":     {[]string{"utun9"}, map[string]string{"TUNDEV": "utun9", "INTERNAL_IP4_ADDRESS": "10.0.0.5 alias"}},
		"ipv6 as ipv4":   {[]string{"utun9"}, map[string]string{"TUNDEV": "utun9", "INTERNAL_IP4_ADDRESS": "fd00::5"}},
		"ipv4 as ipv6":   {[]string{"utun9"}, map[string]string{"TUNDEV": "utun9", "INTERNAL_IP6_ADDRESS": "10.0.0.5"}},
		"mapped ipv6":    {[]string{"utun9"}, map[string]string{"TUNDEV": "utun9", "INTERNAL_IP6_ADDRESS": "::ffff:10.0.0.5"}},
		"zoned ipv6":     {[]string{"utun9"}, map[string]string{"TUNDEV": "utun9", "INTERNAL_IP6_ADDRESS": "fd00::5%en0"}},
		"loopback":       {[]string{"utun9"}, map[string]string{"TUNDEV": "utun9", "INTERNAL_IP4_ADDRESS": "127.0.0.1"}},
		"unspecified":    {[]string{"utun9"}, map[string]string{"TUNDEV": "utun9", "INTERNAL_IP4_ADDRESS": "0.0.0.0"}},
		"multicast":      {[]string{"utun9"}, map[string]string{"TUNDEV": "utun9", "INTERNAL_IP4_ADDRESS": "224.0.0.1"}},
		"broadcast":      {[]string{"utun9"}, map[string]string{"TUNDEV": "utun9", "INTERNAL_IP4_ADDRESS": "255.255.255.255"}},
		"link-local":     {[]string{"utun9"}, map[string]string{"TUNDEV": "utun9", "INTERNAL_IP4_ADDRESS": "169.254.1.1"}},
		"ipv6 loopback":  {[]string{"utun9"}, map[string]string{"TUNDEV": "utun9", "INTERNAL_IP6_ADDRESS": "::1"}},
		"ipv6 any":       {[]string{"utun9"}, map[string]string{"TUNDEV": "utun9", "INTERNAL_IP6_ADDRESS": "::"}},
		"ipv6 multicast": {[]string{"utun9"}, map[string]string{"TUNDEV": "utun9", "INTERNAL_IP6_ADDRESS": "ff02::1"}},
		"ipv6 link":      {[]string{"utun9"}, map[string]string{"TUNDEV": "utun9", "INTERNAL_IP6_ADDRESS": "fe80::1"}},
		"one bad of two": {[]string{"utun9"}, map[string]string{"TUNDEV": "utun9", "INTERNAL_IP4_ADDRESS": "10.0.0.5", "INTERNAL_IP6_ADDRESS": "fe80::1"}},
	}
	for name, c := range bad {
		c.env["reason"] = "connect"
		if iface, addrs, err := parseScript(c.args, getenv(c.env)); err == nil || iface != "" || addrs != nil {
			t.Errorf("%s: got %q %v %v, want an error and nothing to configure", name, iface, addrs, err)
		}
	}

	for _, reason := range []string{"pre-init", "disconnect", "reconnect", "attempt-reconnect"} {
		env := map[string]string{"reason": reason, "TUNDEV": "utun3; rm -rf /", "INTERNAL_IP4_ADDRESS": "10.0.0.5"}
		if iface, addrs, err := parseScript([]string{"utun9"}, getenv(env)); err != nil || iface != "" || addrs != nil {
			t.Errorf("reason %s: got %q %v %v, want nothing to do", reason, iface, addrs, err)
		}
	}
}

// The helper picks a utun no interface has yet.
func TestFreeUtun(t *testing.T) {
	present := []string{"lo0", "en0", "utun0", "utun1", "utun2", "utun3", "utun4", "utun5"}
	got := freeUtun(present)
	if !utunName.MatchString(got) || slices.Contains(present, got) {
		t.Errorf("freeUtun = %q, want a utun not in %v", got, present)
	}
	if again := freeUtun(append(present, got)); again == got || !utunName.MatchString(again) {
		t.Errorf("with %s taken, freeUtun = %q", got, again)
	}
}

func getenv(env map[string]string) func(string) string {
	return func(k string) string { return env[k] }
}

func strs(ps []netip.Prefix) []string {
	var s []string
	for _, p := range ps {
		s = append(s, p.String())
	}
	return s
}
