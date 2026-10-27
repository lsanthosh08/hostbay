package app

import (
	"hostbay/internal/apperr"
	"hostbay/internal/container"
	"hostbay/internal/store"
)

// Payload shapes the UI already uses (src/api.ts). Field names and JSON tags
// are the contract: camelCase, `isDir` on file entries.

type Host struct {
	ID            string   `json:"id"`
	Alias         string   `json:"alias"`
	Hostname      string   `json:"hostname"`
	Port          int      `json:"port"`
	User          string   `json:"user"`
	IdentityFile  string   `json:"identityFile"`
	Tags          []string `json:"tags"`
	LastConnected string   `json:"lastConnected,omitempty"`
	FolderID      *string  `json:"folderId,omitempty"`
	Order         int      `json:"order,omitempty"`
	AuthType      *string  `json:"authType,omitempty"`
	Password      *string  `json:"password,omitempty"`
	HasPassword   bool     `json:"hasPassword,omitempty"`
	Kind          *string  `json:"kind,omitempty"`
	RdpPort       *int     `json:"rdpPort,omitempty"`
	RdpSecurity   *string  `json:"rdpSecurity,omitempty"`
	Mac           *string  `json:"mac,omitempty"`
	JumpHostID    *string  `json:"jumpHostId,omitempty"` // round-trips so saving the form keeps the link
	// File-only kinds ftp, smb, webdav and s3 (store.Host has the details).
	// TLS is also a VNC host's security mode, "vencrypt" or "none", and a
	// SPICE host's, "tls" or "none".
	TLS       *string `json:"tls,omitempty"`
	URL       *string `json:"url,omitempty"`
	Domain    *string `json:"domain,omitempty"`
	Bucket    *string `json:"bucket,omitempty"`
	Region    *string `json:"region,omitempty"`
	PathStyle *bool   `json:"pathStyle,omitempty"`
	// Connection extras (R48–R52); authType may also be "agent" or
	// "kerberos" for SSH.
	ProxyID      *string           `json:"proxyId,omitempty"`
	ForwardAgent *bool             `json:"forwardAgent,omitempty"`
	Algorithms   *store.Algorithms `json:"algorithms,omitempty"`
	Keepalive    *int              `json:"keepalive,omitempty"` // seconds; 0 or unset means 30
	LocalEcho    *bool             `json:"localEcho,omitempty"` // raw TCP
	// X11 forwarding, off unless set, and the X11 desktop's command (R47).
	X11        *bool   `json:"x11,omitempty"`
	X11Desktop *string `json:"x11Desktop,omitempty"`
	// Database kinds (R56). TLS is "verify" (unset) or "none"; an
	// elasticsearch or rqlite host has none and connects as its URL says.
	// Database is a Redis host's database number, a MongoDB host's auth
	// database and Oracle's service name. File is a
	// SQLite host's path, shown only: SaveHost ignores it and sets the path
	// from FilePick, the token DatabasePickFile returned for a file the
	// user chose in a dialog Go opened.
	Database *string `json:"database,omitempty"`
	File     *string `json:"file,omitempty"`
	FilePick *string `json:"filePick,omitempty"`
	// Kubernetes (R57): the namespace a tab opens on. URL is the API
	// server; its CA and TLS server name come only from a kubeconfig
	// import, never from the page.
	Namespace *string `json:"namespace,omitempty"`
}

// Proxy is a SOCKS5 or HTTP CONNECT proxy for Settings (R49). Like a
// host's, its password is never sent back, only whether one is saved.
type Proxy struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Type        string  `json:"type"` // "socks5" or "http"
	Host        string  `json:"host"`
	Port        int     `json:"port"`
	User        string  `json:"user,omitempty"`
	Password    *string `json:"password,omitempty"`
	HasPassword bool    `json:"hasPassword,omitempty"`
}

// ConnTest is TestConnection's outcome. Step is "ok", or the step that
// failed: "proxy", "tcp", "ssh" (negotiation), "hostKey" or "auth", or for
// DatabaseTest "database" (signed in, but the database refused). Error
// carries the code and details the UI acts on: a HostKeyUnknown's
// requestId for TrustHostKey, a HostKeyChanged's removeId, a
// SecretNeeded's kind.
type ConnTest struct {
	Step    string        `json:"step"`
	Message string        `json:"message,omitempty"`
	Error   *apperr.Error `json:"error,omitempty"`
}

type Folder struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	ParentID *string `json:"parentId,omitempty"`
	Order    int     `json:"order,omitempty"`
}

type Script struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Script string `json:"script"`
}

type ExecResult struct {
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	Code   *int   `json:"code"`
}

type DirEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"isDir"`
	Size  uint64 `json:"size"`
	Mode  uint32 `json:"mode"`
	Mtime int64  `json:"mtime"`
	Owner uint32 `json:"owner"`
	Group uint32 `json:"group"`
}

type DockerContainer struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Image  string `json:"image"`
	Status string `json:"status"`
	State  string `json:"state"`
	Ports  string `json:"ports"`
}

type DockerImage struct {
	ID         string `json:"id"`
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	Size       string `json:"size"`
	Created    string `json:"created"`
}

type DockerStat struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	CPU     string `json:"cpu"`
	Mem     string `json:"mem"`
	MemPerc string `json:"memPerc"`
}

type ComposeProject struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// ContainerEngine is one choice in the Containers tab's engine picker.
type ContainerEngine struct {
	ID    string `json:"id"` // the kind, or "wsl:<distro>:<kind>" for an engine inside WSL
	Label string `json:"label"`
	Path  string `json:"path,omitempty"` // a binary on this computer
}

// ContainerInfo is the engine a session's Containers tab drives (KTD27):
// on this computer, its binary, context and socket.
type ContainerInfo struct {
	Engine    string            `json:"engine"` // a ContainerEngine ID
	Kind      string            `json:"kind"`
	Namespace string            `json:"namespace"`
	Local     bool              `json:"local"`
	Path      string            `json:"path"`
	Context   string            `json:"context"`
	Endpoint  string            `json:"endpoint"`
	Caps      container.Caps    `json:"caps"`
	Engines   []ContainerEngine `json:"engines"`
}

type SystemdUnit struct {
	Unit        string `json:"unit"`
	Load        string `json:"load"`
	Active      string `json:"active"`
	Sub         string `json:"sub"`
	Description string `json:"description"`
}

type DashboardInfo struct {
	OS                  string `json:"os"`
	Uptime              string `json:"uptime"`
	Load                string `json:"load"`
	Disk                string `json:"disk"`
	NginxInstalled      bool   `json:"nginxInstalled"`
	DockerInstalled     bool   `json:"dockerInstalled"`
	DockerPermission    bool   `json:"dockerPermission"`
	PodmanInstalled     bool   `json:"podmanInstalled"`
	NerdctlInstalled    bool   `json:"nerdctlInstalled"`
	AzureAgentInstalled bool   `json:"azureAgentInstalled"`
}

type IisSite struct {
	Name         string `json:"name"`
	ID           int    `json:"id"`
	State        string `json:"state"`
	PhysicalPath string `json:"physicalPath"`
	Bindings     string `json:"bindings"`
	AppPool      string `json:"appPool"`
}

type IisPool struct {
	Name    string `json:"name"`
	State   string `json:"state"`
	Runtime string `json:"runtime"`
}

type WinDashboard struct {
	OS           string `json:"os"`
	Uptime       string `json:"uptime"`
	CPU          string `json:"cpu"`
	Memory       string `json:"memory"`
	Disk         string `json:"disk"`
	IisInstalled bool   `json:"iisInstalled"`
}
