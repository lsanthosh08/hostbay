package vpnconf

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"hostbay/internal/importer"
)

// Found is one VPN configuration an installed client keeps (R39).
type Found struct {
	Client string `json:"client"`
	Path   string `json:"path"`
	// Readable is false for a file only its owner (often root) can read:
	// it is listed by name, and importing it fails until it can be read.
	Readable bool `json:"readable"`
}

// location is a folder a client keeps configurations in; match says
// whether an entry is one, and whether this user can read it.
type location struct {
	Client string
	Dir    string
	Match  func(path string, e fs.DirEntry) (ok, readable bool)
}

// Scan lists the VPN configurations installed clients keep on this
// computer, without reading files this user cannot read.
// shortcut: the locations are the clients' documented defaults, checked
// one folder deep; add others as users report them.
func Scan() []Found {
	home, _ := os.UserHomeDir()
	lib := filepath.Join(home, "Library", "Application Support")
	tunnelhub := location{"TunnelHub", filepath.Join(home, ".config", "tunnelhub"), matchName("vpn-config.json")}
	var locs []location
	switch runtime.GOOS {
	case "darwin":
		locs = []location{
			{"Tunnelblick", filepath.Join(lib, "Tunnelblick", "Configurations"), matchExt(".tblk")},
			{"Viscosity", filepath.Join(lib, "Viscosity", "OpenVPN"), viscDir},
			{"OpenVPN Connect", filepath.Join(lib, "OpenVPN Connect", "profiles"), matchExt(".ovpn")},
			{"AnyConnect", "/opt/cisco/anyconnect/profile", matchExt(".xml")},
			{"Cisco Secure Client", "/opt/cisco/secureclient/vpn/profile", matchExt(".xml")},
			tunnelhub,
		}
	case "linux":
		locs = []location{
			{"NetworkManager", "/etc/NetworkManager/system-connections", nmVPN},
			{"AnyConnect", "/opt/cisco/anyconnect/profile", matchExt(".xml")},
			{"Cisco Secure Client", "/opt/cisco/secureclient/vpn/profile", matchExt(".xml")},
			tunnelhub,
		}
	case "windows":
		appdata, programData := os.Getenv("APPDATA"), os.Getenv("ProgramData")
		locs = []location{
			{"Windows", filepath.Join(appdata, "Microsoft", "Network", "Connections", "Pbk"), matchName("rasphone.pbk")},
			{"OpenVPN GUI", filepath.Join(home, "OpenVPN", "config"), matchExt(".ovpn")},
			{"OpenVPN Connect", filepath.Join(appdata, "OpenVPN Connect", "profiles"), matchExt(".ovpn")},
			{"AnyConnect", filepath.Join(programData, "Cisco", "Cisco AnyConnect Secure Mobility Client", "Profile"), matchExt(".xml")},
			{"Cisco Secure Client", filepath.Join(programData, "Cisco", "Cisco Secure Client", "VPN", "Profile"), matchExt(".xml")},
		}
	}
	return scan(locs)
}

func scan(locs []location) []Found {
	out := []Found{}
	for _, l := range locs {
		entries, err := os.ReadDir(l.Dir) // a folder only root can list shows nothing
		if err != nil {
			continue
		}
		for _, e := range entries {
			p := filepath.Join(l.Dir, e.Name())
			if ok, readable := l.Match(p, e); ok {
				out = append(out, Found{Client: l.Client, Path: p, Readable: readable})
			}
		}
	}
	return out
}

// canRead opens path without reading it.
func canRead(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

func matchExt(ext string) func(string, fs.DirEntry) (bool, bool) {
	return func(p string, e fs.DirEntry) (bool, bool) {
		if !strings.EqualFold(filepath.Ext(e.Name()), ext) {
			return false, false
		}
		return true, canRead(p)
	}
}

func matchName(name string) func(string, fs.DirEntry) (bool, bool) {
	return func(p string, e fs.DirEntry) (bool, bool) {
		if !strings.EqualFold(e.Name(), name) {
			return false, false
		}
		return true, canRead(p)
	}
}

// viscDir matches a Viscosity connection folder, which holds config.conf.
func viscDir(p string, e fs.DirEntry) (bool, bool) {
	if !e.IsDir() {
		return false, false
	}
	if _, err := os.Stat(filepath.Join(p, "config.conf")); err != nil {
		return false, false
	}
	return true, canRead(filepath.Join(p, "config.conf"))
}

// nmVPN matches a NetworkManager connection that is a VPN. One this user
// cannot read is listed by name, since its type cannot be told.
func nmVPN(p string, e fs.DirEntry) (bool, bool) {
	if !strings.EqualFold(filepath.Ext(e.Name()), ".nmconnection") {
		return false, false
	}
	raw, err := importer.ReadFile(p)
	if err != nil {
		return true, false
	}
	t := find(parseINI(importer.Text(raw)), "connection").get("type")
	return t == "vpn" || t == "wireguard", true
}
