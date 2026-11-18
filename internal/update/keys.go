package update

// releaseKeys are the base64 ed25519 public keys a release manifest may be
// signed with, newest last. More than one lets the key rotate: a release
// signed by the old key ships the new one first (docs/release-signing.md).
var releaseKeys = []string{
	"wR876VoZfZsYK2Eso29CID6AdjPH/r3iuQABbvhVqC0=",
}
