module hostbay

go 1.27.0

// SQL Server makes a new self-signed certificate at each start with a random
// serial, about half of them negative, which crypto/x509 refuses to parse by
// default. This only allows parsing it; the chain or pin check still runs.
godebug x509negativeserial=1

require (
	github.com/Microsoft/go-winio v0.6.2
	github.com/UserExistsError/conpty v0.1.4
	github.com/alexbrainman/sspi v0.0.0-20250919150558-7d374ff0d59e
	github.com/cloudsoda/go-smb2 v0.0.0-20260701064823-d8c5600d73b8
	github.com/coder/websocket v1.8.14
	github.com/creack/pty v1.1.24
	github.com/fclairamb/ftpserverlib v0.32.4
	github.com/go-git/go-git/v5 v5.19.2
	github.com/go-sql-driver/mysql v1.10.1
	github.com/jlaffaye/ftp v0.2.4
	github.com/kevinburke/ssh_config v1.6.0
	github.com/lib/pq v1.12.3
	github.com/microsoft/go-mssqldb v1.11.2
	github.com/modelcontextprotocol/go-sdk v1.8.0
	github.com/pkg/sftp v1.13.11
	github.com/redis/go-redis/v9 v9.23.0
	github.com/rhnvrm/simples3 v0.11.1
	github.com/sijms/go-ora/v2 v2.9.0
	github.com/spf13/afero v1.15.0
	github.com/studio-b12/gowebdav v0.13.0
	github.com/unixshells/mosh-go v0.5.2
	github.com/unixshells/vt-go v0.1.0
	github.com/wailsapp/wails/v3 v3.0.0-beta.27
	github.com/xanzy/ssh-agent v0.3.3
	github.com/zalando/go-keyring v0.2.6
	go.bug.st/serial v1.7.1
	go.mongodb.org/mongo-driver/v2 v2.9.2
	go.yaml.in/yaml/v3 v3.0.5
	golang.org/x/crypto v0.55.0
	golang.org/x/net v0.58.0
	golang.org/x/sys v0.49.0
	golang.zx2c4.com/wireguard v0.0.0-20261024220952-b3bc704ef0ee
	modernc.org/sqlite v1.61.0
	mvdan.cc/sh/v3 v3.12.0
)

require (
	al.essio.dev/pkg/shellescape v1.6.0 // indirect
	dario.cat/mergo v1.0.2 // indirect
	filippo.io/edwards25519 v1.2.0 // indirect
	github.com/ProtonMail/go-crypto v1.3.0 // indirect
	github.com/adrg/xdg v0.5.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/charmbracelet/colorprofile v0.4.2 // indirect
	github.com/charmbracelet/ultraviolet v0.0.0-20260303162955-0b88c25f3fff // indirect
	github.com/charmbracelet/x/ansi v0.11.6 // indirect
	github.com/charmbracelet/x/exp/ordered v0.1.0 // indirect
	github.com/charmbracelet/x/term v0.2.2 // indirect
	github.com/charmbracelet/x/termios v0.1.1 // indirect
	github.com/charmbracelet/x/windows v0.2.2 // indirect
	github.com/clipperhouse/displaywidth v0.9.0 // indirect
	github.com/clipperhouse/stringish v0.1.1 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/cloudflare/circl v1.6.3 // indirect
	github.com/cloudsoda/sddl v0.0.0-20250224235906-926454e91efc // indirect
	github.com/cyphar/filepath-securejoin v0.6.1 // indirect
	github.com/danieljoos/wincred v1.2.3 // indirect
	github.com/dustin/go-humanize v1.1.0 // indirect
	github.com/emirpasic/gods v1.18.1 // indirect
	github.com/geoffgarside/ber v1.1.0 // indirect
	github.com/go-git/gcfg v1.5.1-0.20230307220236-3a3c6141e376 // indirect
	github.com/go-git/go-billy/v5 v5.9.0 // indirect
	github.com/go-ole/go-ole v1.3.0 // indirect
	github.com/godbus/dbus/v5 v5.2.2 // indirect
	github.com/golang-sql/civil v0.0.0-20220223132316-b832511892a9 // indirect
	github.com/golang-sql/sqlexp v0.1.0 // indirect
	github.com/golang/groupcache v0.0.0-20241129210726-2c02b8208cf8 // indirect
	github.com/google/btree v1.1.2 // indirect
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/hashicorp/go-uuid v1.0.3 // indirect
	github.com/jbenet/go-context v0.0.0-20150711004518-d14ea06fba99 // indirect
	github.com/jcmturner/aescts/v2 v2.0.0 // indirect
	github.com/jcmturner/dnsutils/v2 v2.0.0 // indirect
	github.com/jcmturner/gofork v1.7.6 // indirect
	github.com/jcmturner/goidentity/v6 v6.0.1 // indirect
	github.com/jcmturner/gokrb5/v8 v8.4.4 // indirect
	github.com/jcmturner/rpc/v2 v2.0.3 // indirect
	github.com/klauspost/compress v1.19.2 // indirect
	github.com/klauspost/cpuid/v2 v2.4.0 // indirect
	github.com/kr/fs v0.1.0 // indirect
	github.com/lucasb-eyer/go-colorful v1.3.0 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/mattn/go-runewidth v0.0.19 // indirect
	github.com/muesli/cancelreader v0.2.2 // indirect
	github.com/ncruces/go-strftime v1.1.0 // indirect
	github.com/pjbgf/sha1cd v0.6.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/sergi/go-diff v1.4.0 // indirect
	github.com/shopspring/decimal v1.4.0 // indirect
	github.com/skeema/knownhosts v1.3.2 // indirect
	github.com/xdg-go/pbkdf2 v1.0.0 // indirect
	github.com/xdg-go/scram v1.2.0 // indirect
	github.com/xdg-go/stringprep v1.0.4 // indirect
	github.com/xo/terminfo v0.0.0-20220910002029-abceb7e1c41e // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	github.com/youmark/pkcs8 v0.0.0-20240726163527-a2c0da244d78 // indirect
	go.uber.org/atomic v1.12.0 // indirect
	golang.org/x/oauth2 v0.35.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	golang.org/x/time v0.15.0 // indirect
	golang.zx2c4.com/wintun v0.0.0-20230126152724-0fa3db229ce2 // indirect
	gopkg.in/warnings.v0 v0.1.2 // indirect
	gvisor.dev/gvisor v0.0.0-20250503011706-39ed1f5ac29c // indirect
	modernc.org/libc v1.79.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)
