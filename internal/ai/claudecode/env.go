// Package claudecode runs the person's own signed-in Claude Code program as
// an inference-only model backend (KTD1). Hostbay never reads a Claude
// credential, never starts a sign-in and gives the program no tools: it
// sends a prompt, reads text back, and its own agent loop and approval gate
// decide what runs.
package claudecode

import (
	"strings"
)

// Env is the environment a call to the program runs in: the current one
// minus everything that carries a credential or switches the provider
// (KTD5). It is a removal list, not an allowlist, because the program finds
// its own sign-in through variables such as HOME and USER, and the person's
// proxy and certificate settings must keep working. GOOS only decides
// whether names compare case-insensitively.
func Env(environ []string, goos string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if name == "" && strings.HasPrefix(kv, "=") { // a Windows "=C:=C:\dir" entry
			out = append(out, kv)
			continue
		}
		if goos == "windows" {
			name = strings.ToUpper(name)
		}
		if !dropped(name) {
			out = append(out, kv)
		}
	}
	return out
}

// exactDropped are single variables that hold or point at a credential, or
// mark the process as a child of another Claude Code session.
var exactDropped = map[string]bool{
	"AWS_BEARER_TOKEN_BEDROCK":             true,
	"CLAUDECODE":                           true,
	"CLAUDE_BRIDGE_OAUTH_TOKEN":            true,
	"CLAUDE_SESSION_INGRESS_TOKEN_FILE":    true,
	"CLAUDE_TRUSTED_DEVICE_TOKEN":          true,
	"CLAUDE_BG_SOCKET_TOKENS_PATH":         true,
	"CLAUDE_CODE_ENTRYPOINT":               true,
	"CLAUDE_CODE_CHILD_SESSION":            true,
	"CLAUDE_CODE_EXECPATH":                 true,
	"CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST": true,
	"CLAUDE_CODE_ACCOUNT_UUID":             true,
	"CLAUDE_CODE_ORGANIZATION_UUID":        true,
	"CLAUDE_CODE_USER_EMAIL":               true,
}

var droppedPrefixes = []string{
	"ANTHROPIC_",
	"CLAUDE_CODE_HOST_",
	"CLAUDE_CODE_MESSAGING_",
	"CLAUDE_CODE_SESSION_",
	"CLAUDE_CODE_SDK_",
	"CLAUDE_CODE_CCR_",
}

// droppedParts are fragments that, in a CLAUDE_CODE_ name, mark a
// credential, a provider switch or an endpoint override.
var droppedParts = []string{
	"OAUTH", "API_KEY", "CLIENT_KEY", "GATEWAY", "BASE_URL",
	"USE_BEDROCK", "USE_VERTEX", "USE_FOUNDRY", "USE_MANTLE",
	"SECRET", "PASSWORD", "PASSPHRASE", "CREDS", "_AUTH_FILE",
}

func dropped(name string) bool {
	if exactDropped[name] {
		return true
	}
	for _, p := range droppedPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	if !strings.HasPrefix(name, "CLAUDE_CODE_") {
		return false
	}
	// TOKENS is a size limit (CLAUDE_CODE_MAX_OUTPUT_TOKENS), not a credential.
	if (strings.Contains(name, "TOKEN") && !strings.Contains(name, "TOKENS")) ||
		(strings.HasPrefix(name, "CLAUDE_CODE_SKIP_") && strings.HasSuffix(name, "_AUTH")) {
		return true
	}
	for _, p := range droppedParts {
		if strings.Contains(name, p) {
			return true
		}
	}
	return false
}
