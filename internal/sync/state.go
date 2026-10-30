package sync

import (
	"errors"
	"time"

	"hostbay/internal/store"
)

// stateFile is this computer's sync settings and progress, kept in the
// store folder like every other Hostbay document.
const stateFile = "sync.json"

// State is persisted sync configuration and progress. Credential and
// SyncKey are sealed under the master key, bound to TargetID and the
// target (KTD6); nothing in it is a readable secret.
type State struct {
	Enabled    bool                  `json:"enabled"`
	Target     Target                `json:"target"`
	TargetID   string                `json:"targetId,omitempty"`
	Credential string                `json:"credential,omitempty"`
	SyncKey    string                `json:"syncKey,omitempty"`
	KeyID      string                `json:"keyId,omitempty"`
	KDF        KDF                   `json:"kdf"`
	Generation uint64                `json:"generation"`
	Seen       map[string]uint64     `json:"seen,omitempty"` // newest generation seen per device
	Devices    map[string]DeviceMark `json:"devices,omitempty"`
	// Base is the Meta of every item as of the last sync, per collection:
	// the common ancestor of the three-way merge.
	Base      map[string]map[string]store.Meta `json:"base,omitempty"`
	Version   string                           `json:"version,omitempty"` // the target's version after the last sync
	Conflicts []Conflict                       `json:"conflicts,omitempty"`
	Holds     []Hold                           `json:"holds,omitempty"`
	Synced    bool                             `json:"synced"` // a first sync has completed
	Paused    string                           `json:"paused,omitempty"`
	LastSync  time.Time                        `json:"lastSync"`
	LastError string                           `json:"lastError,omitempty"`
}

// Hold is a synced security setting not yet confirmed on this computer: a
// tunnel set to listen beyond loopback, which locally stays on loopback; a
// host (Kind "host") set to connect unencrypted, forward the agent or X11,
// offer insecure algorithms or use another proxy or VPN, which locally keeps what this
// computer confirmed; a proxy (Kind "proxy") pointed elsewhere, or a VPN
// profile (Kind "vpn") pointed at another gateway or switched to another
// type or tier, which locally keeps its old one; or an AI execution mode (Kind "aimode",
// ID the ai group) weaker than this computer's, which locally keeps its
// mode; or a post-login rule (Kind "postlogin") created or changed on
// another computer, which locally does not run. The other fields are the synced values waiting. A kind this build
// does not know stays held and listed, and is never resolved here.
type Hold struct {
	ID   string     `json:"id"`
	Kind string     `json:"kind,omitempty"` // "" for a tunnel, "host", "proxy", "vpn", "aimode" or "postlogin"
	Name string     `json:"name"`
	Meta store.Meta `json:"meta"`

	Plaintext    bool              `json:"plaintext,omitempty"`
	ForwardAgent bool              `json:"forwardAgent,omitempty"`
	X11          bool              `json:"x11,omitempty"`
	Algorithms   *store.Algorithms `json:"algorithms,omitempty"`
	ProxyID      *string           `json:"proxyId,omitempty"`  // "" removes the host's proxy
	VPNID        *string           `json:"vpnId,omitempty"`    // "" removes the host's VPN
	Endpoint     *Endpoint         `json:"endpoint,omitempty"` // Kind "proxy" or "vpn"
	Cluster      *ClusterTrust     `json:"cluster,omitempty"`  // a Kubernetes host's synced server and trust anchor
	Mode         string            `json:"mode,omitempty"`     // Kind "aimode": the synced execution mode
}

// ClusterTrust is where a Kubernetes host connects and what it trusts
// there (KTD28). Hostname and Port follow URL.
type ClusterTrust struct {
	URL           string `json:"url"`
	Hostname      string `json:"hostname"`
	Port          int    `json:"port"`
	CAData        string `json:"caData,omitempty"`
	TLSServerName string `json:"tlsServerName,omitempty"`
}

// Endpoint is where a proxy or VPN profile points. Host is a profile's
// gateway.
type Endpoint struct {
	Type string `json:"type"`
	Host string `json:"host"`
	Port int    `json:"port"`
	Tier string `json:"tier,omitempty"` // a VPN profile's
}

// Report is one sync's outcome.
type Report struct {
	Uploaded  bool     `json:"uploaded"`
	Received  int      `json:"received"` // items that came from other computers
	Conflicts int      `json:"conflicts"`
	Retries   int      `json:"retries"`
	Notes     []string `json:"notes"`
}

// Pause codes: sync stops until the user acts.
const (
	CodeDecrypt          = "decrypt"
	CodeUpdateNeeded     = "updateNeeded"
	CodeRollback         = "rollback"
	CodePassphraseChange = "passphraseChanged"
	CodeHostKeyChanged   = "hostKeyChanged"
	CodeAuth             = "auth"
	CodeClock            = "clock"
)

var (
	ErrAuth              = errors.New("the sync target refused the sign-in")
	ErrClock             = errors.New("this computer's clock is too far from the sync target's; correct the clock, then sync again")
	ErrChoiceNeeded      = errors.New("this computer and the sync target both hold data: choose to use the target's data or merge")
	ErrPassphraseChanged = errors.New("the sync passphrase was changed on another computer; enter the new passphrase")
)

func LoadState(st *store.Store) (*State, error) {
	s, err := store.Load[State](st, stateFile)
	return &s, err
}

func SaveState(st *store.Store, s *State) error { return store.Save(st, stateFile, *s) }
