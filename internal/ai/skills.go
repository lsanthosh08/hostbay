package ai

import (
	"strings"

	"hostbay/internal/store"
)

// Placeholder is where a template puts the text typed after its /name.
// A template without it gets that text after a blank line.
const Placeholder = "{{input}}"

// Expand is the user's message as it is sent (R61). Saved names at the
// start, each written /name, are read in order: a skill's body is attached
// in its own block ahead of the message, and a template replaces the
// message with its body around the rest of the text, ending the names. A
// /word that names nothing saved stays as text, so /etc/hosts is a path.
// Only what the user named is sent; no skill is listed to the model.
func Expand(text string, skill func(name string) *store.Skill) []Block {
	var out []Block
	msg, rest := text, strings.TrimLeft(text, " \t\n")
	for skill != nil && strings.HasPrefix(rest, "/") {
		end := strings.IndexAny(rest, " \t\n")
		if end < 0 {
			end = len(rest)
		}
		s := skill(rest[1:end])
		if s == nil {
			break
		}
		rest = strings.TrimLeft(rest[end:], " \t\n")
		if s.Kind == store.TemplateKind {
			msg = fill(s.Body, strings.TrimSpace(rest))
			break
		}
		out = append(out, Block{Type: "text", Text: skillText(*s)})
	}
	return append(out, Block{Type: "text", Text: msg})
}

func fill(body, input string) string {
	if strings.Contains(body, Placeholder) {
		return strings.ReplaceAll(body, Placeholder, input)
	}
	if input == "" {
		return body
	}
	return body + "\n\n" + input
}

// skillText is an attached skill as the model reads it.
func skillText(s store.Skill) string {
	return "The user attached the skill /" + s.Name + ". Follow it for this request:\n" + s.Body
}
