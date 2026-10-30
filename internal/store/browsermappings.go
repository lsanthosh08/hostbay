package store

// BrowserMapping launches one browser profile through one VPN profile
// (R33). A browser profile has at most one mapping.
type BrowserMapping struct {
	Browser string `json:"browser"` // chrome, chromium, edge or brave
	Profile string `json:"profile"` // the profile's folder, what --profile-directory takes
	VPN     string `json:"vpn"`     // the VPN profile's ID
}
