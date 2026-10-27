package store

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// Skill kinds (R61). A skill's body joins an AI request when the user
// attaches it with /name; a template's body becomes the message, around
// the text typed after /name.
const (
	SkillKind    = "skill"
	TemplateKind = "template"
)

// Skill is a saved skill or prompt template. Skills are a synced
// collection with no secret.
type Skill struct {
	Meta
	Name        string `json:"name"` // what follows / in the AI sidebar
	Description string `json:"description,omitempty"`
	Body        string `json:"body"`
	Kind        string `json:"kind"` // SkillKind or TemplateKind
	// AgentAuthored marks a skill the AI agent proposed and the user
	// approved (KTD33).
	AgentAuthored bool `json:"agentAuthored,omitempty"`
}

// MaxSkillBody is the largest body a skill may have, in bytes.
const MaxSkillBody = 64 << 10

var skillName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// CheckSkill refuses a skill that /name could not attach, or whose text
// holds a character a card would not show as it is stored: any control
// character but newline and tab in the body, any at all in the name or
// description, and bidi controls or zero-width characters anywhere.
func CheckSkill(s Skill) error {
	switch {
	case !skillName.MatchString(s.Name):
		return errors.New("a name is up to 64 lower-case letters, digits, - and _, such as deploy-check")
	case s.Kind != SkillKind && s.Kind != TemplateKind:
		return fmt.Errorf("unknown kind %q", s.Kind)
	case strings.TrimSpace(s.Body) == "":
		return errors.New("write the skill's text")
	case len(s.Body) > MaxSkillBody:
		return errors.New("the text is longer than 64 KiB")
	case len(s.Description) > 500:
		return errors.New("the description is longer than 500 bytes")
	}
	if err := visible(s.Description, false); err != nil {
		return fmt.Errorf("the description %w", err)
	}
	if err := visible(s.Body, true); err != nil {
		return fmt.Errorf("the text %w", err)
	}
	return nil
}

// CheckVisible refuses text that would not show as it is: a control
// character other than newline and tab, a bidi control or a zero-width
// character.
func CheckVisible(s string) error { return visible(s, true) }

// Invisible reports whether r is a bidi control or a zero-width character,
// which would make text show differently from what it holds.
func Invisible(r rune) bool {
	return unicode.Is(unicode.Bidi_Control, r) || r == 0x200b || r == 0x200c || r == 0x200d || r == 0x2060 || r == 0xfeff || r == 0x00ad
}

func visible(s string, lines bool) error {
	for _, r := range s {
		switch {
		case lines && (r == '\n' || r == '\t'):
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			return fmt.Errorf("holds the control character U+%04X", r)
		case Invisible(r):
			return fmt.Errorf("holds the invisible character U+%04X", r)
		}
	}
	return nil
}

// Skill returns the skill with id, or nil.
func (d *Data) Skill(id string) *Skill {
	if i := find(d.Skills.Items, id); i >= 0 {
		return &d.Skills.Items[i]
	}
	return nil
}

// SkillNamed returns the skill /name attaches, or nil. A sync merge can
// leave two with one name; the lowest ID wins, on every computer.
func (d *Data) SkillNamed(name string) *Skill {
	var out *Skill
	for i := range d.Skills.Items {
		s := &d.Skills.Items[i]
		if s.Name == name && (out == nil || s.ID < out.ID) {
			out = s
		}
	}
	return out
}

// PutSkill saves s as a local change and returns it with its new Meta.
func (d *Data) PutSkill(s Skill) Skill { return put(d, &d.Skills, s) }

// AddSkill adds s, keeping any Meta it carries.
func (d *Data) AddSkill(s Skill) Skill { return add(d, &d.Skills, s) }

// PurgeSkill removes a skill, leaving a tombstone for sync.
func (d *Data) PurgeSkill(id string) error {
	if !purge(d, &d.Skills, id) {
		return errors.New("skill not found")
	}
	return nil
}
