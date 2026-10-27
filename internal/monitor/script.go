// Package monitor samples a Linux host's load over its shared SSH
// connection (R59, KTD29): one constant read-only script per tick, parsed
// and turned into rates here.
package monitor

// Script is the whole command each tick runs. It is a constant: no host
// field, alias or page value ever enters it, and Go does all filtering.
// Each section starts with a "<nonce>:KEY::" line of its own, the nonce
// drawn on the host each run, so text from the host (a command line, a
// process name) cannot pose as a section; the key says which fallback
// produced it. It reads only, needs no sudo, and on a system other than
// Linux prints OS:: and stops.
const Script = `exec 2>/dev/null
k=$(od -A n -N 8 -t x1 /dev/urandom | tr -d ' \n')
echo "NONCE::$k"
os=$(uname -s)
echo "OS::$os"
[ "$os" = Linux ] || exit 0
echo "$k:STAT::"
grep '^cpu' /proc/stat
echo "$k:MEMINFO::"
cat /proc/meminfo
echo "$k:LOADAVG::"
cat /proc/loadavg
echo "$k:DF::"
if command -v timeout >/dev/null; then timeout 5 df -Pk; else df -Pk; fi
echo "$k:NETDEV::"
cat /proc/net/dev
echo "$k:PS::"
if command -v ps >/dev/null; then
  ps -eo pid,user,pcpu,rss,args || ps -o pid,user,rss,args || ps
else
  echo "PID COMMAND"
  for d in /proc/[0-9]*; do
    c=$(tr '\0\n' '  ' <"$d/cmdline")
    [ -n "$c" ] || c="[$(tr '\n' ' ' <"$d/comm")]"
    echo "${d#/proc/} $c"
  done
fi
if command -v ss >/dev/null; then
  echo "$k:SS::"
  ss -tulnp
elif command -v netstat >/dev/null; then
  echo "$k:NETSTAT::"
  netstat -tulnp
else
  echo "$k:PROCNET::"
  for f in tcp tcp6 udp udp6; do echo "@$f"; cat "/proc/net/$f"; done
fi
j=$(ip -j addr show)
case "$j" in
"["*)
  echo "$k:IPJ::"
  echo "$j"
  ;;
*)
  echo "$k:SYSNET::"
  for d in /sys/class/net/*; do echo "${d##*/} $(cat "$d/operstate") $(cat "$d/address") $(cat "$d/mtu")"; done
  echo "$k:IPO::"
  ip -o addr show
  ;;
esac
`
