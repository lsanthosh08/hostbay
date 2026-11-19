package database

import (
	"errors"
	"slices"
	"strings"
)

// Explainer is the optional capability of showing a statement's plan
// without running it (R10). Explain returns the statement that shows the
// plan of text; CanExplain reports whether this engine's dialect has one.
type Explainer interface {
	CanExplain() bool
	Explain(text string) (string, error)
}

// explainLead are the statements whose plan Explain shows (KTD8). Any
// other first word is refused, so the user's own EXPLAIN ANALYZE, or an
// ANALYZE, which runs the statement, never reaches the server.
var explainLead = []string{"SELECT", "WITH", "VALUES", "TABLE", "INSERT", "UPDATE", "DELETE", "MERGE"}

func (e *sqlEngine) CanExplain() bool { return e.d.explain != "" }

// Explain adds the dialect's fixed prefix, which plans and does not run,
// to text that the lexer reads as exactly one statement: a second
// statement after a semicolon would run for real.
func (e *sqlEngine) Explain(text string) (string, error) {
	if e.d.explain == "" {
		return "", errors.New("this database cannot show a plan here")
	}
	if strings.Contains(text, "/*!") {
		return "", &Error{Category: "syntax", Err: errors.New("a MySQL /*! comment cannot be explained")}
	}
	stmts, ok := statements(text, e.d)
	if !ok || len(stmts) != 1 {
		return "", &Error{Category: "syntax", Err: errors.New("select exactly one statement to explain")}
	}
	if !slices.Contains(explainLead, stmts[0][0]) {
		return "", &Error{Category: "syntax", Err: errors.New("only SELECT, WITH, VALUES, TABLE, INSERT, UPDATE, DELETE or MERGE can be explained")}
	}
	return e.d.explain + strings.TrimSpace(text), nil
}
