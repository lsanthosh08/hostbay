// Package update finds, verifies and installs Hostbay's own updates from
// GitHub Releases (R55, KTD21). Every release carries one JSON manifest and
// an ed25519 signature over its exact bytes; nothing is downloaded unless the
// manifest verifies against a key built into this binary.
package update

// Release asset names of the signed manifest and its signature.
const (
	ManifestName  = "hostbay-manifest.json"
	SignatureName = ManifestName + ".sig"
)

// Manifest lists every installable asset of one release.
type Manifest struct {
	Version string  `json:"version"`
	Assets  []Asset `json:"assets"`
}

// Asset is one release file. OS and Arch use Go's names; the macOS app is
// one universal bundle, so its Arch is "universal".
type Asset struct {
	Name   string `json:"name"`
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"` // lower-case hex
}
