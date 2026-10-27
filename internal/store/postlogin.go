package store

import "errors"

// PostLoginRule runs on each new shell of its host (R70, KTD39): a list of
// steps, each an optional expected prompt and a reply. Answer is the one
// custom secret the rule may send, sealed under KTD6 and bound to
// PostLoginAAD, so it opens only for the host it was saved for.
type PostLoginRule struct {
	Meta
	HostID string          `json:"hostId"`
	Steps  []PostLoginStep `json:"steps"`
	Answer string          `json:"answer,omitempty"`
	// Held stands in for a rule that sync created or changed and this
	// computer has not confirmed: it does not run (KTD13). Sync clears it
	// before a merge and never uploads it.
	Held bool `json:"held,omitempty"`
}

// PostLoginStep waits for Expect at the end of the last output line, or for
// quiet when Expect is empty, then sends its reply and Enter.
type PostLoginStep struct {
	Expect string `json:"expect,omitempty"`
	Reply  string `json:"reply,omitempty"` // the text sent, when Kind is ReplyText
	Kind   string `json:"kind,omitempty"`
}

// Reply kinds: plain text, the host's saved password by reference, or the
// rule's custom secret.
const (
	ReplyText     = ""
	ReplyPassword = "password"
	ReplySecret   = "secret"
)

// Secret reports a step whose reply is a secret.
func (s PostLoginStep) Secret() bool { return s.Kind == ReplyPassword || s.Kind == ReplySecret }

// PostLoginAAD binds a rule's custom secret to the rule and to where its
// host points (KTD39): after the host is edited to point elsewhere the
// secret no longer opens.
func PostLoginAAD(ruleID string, h *Host) []byte {
	return append([]byte("hostbay postlogin\x00"+ruleID+"\x00"), h.SecretAAD()...)
}

// PostLoginRule returns the rule with id, or nil.
func (d *Data) PostLoginRule(id string) *PostLoginRule {
	if i := find(d.PostLogin.Items, id); i >= 0 {
		return &d.PostLogin.Items[i]
	}
	return nil
}

// PostLoginRules returns hostID's rules, in order.
func (d *Data) PostLoginRules(hostID string) []PostLoginRule {
	var out []PostLoginRule
	for _, r := range d.PostLogin.Items {
		if r.HostID == hostID {
			out = append(out, r)
		}
	}
	return out
}

// PutPostLogin saves r as a local change, which confirms it.
func (d *Data) PutPostLogin(r PostLoginRule) PostLoginRule {
	r.Held = false
	return put(d, &d.PostLogin, r)
}

// AddPostLogin adds r, keeping any Meta it carries.
func (d *Data) AddPostLogin(r PostLoginRule) PostLoginRule { return add(d, &d.PostLogin, r) }

// PurgePostLogin removes a rule, leaving a tombstone for sync.
func (d *Data) PurgePostLogin(id string) error {
	if !purge(d, &d.PostLogin, id) {
		return errors.New("post-login rule not found")
	}
	return nil
}
