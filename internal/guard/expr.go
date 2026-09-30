package guard

import (
	"regexp"
	"strings"
)

// The only job-level `if:` that keeps someone else's code off the local runner is an AND of
// clauses that includes:
//
//  1. an actor clause: github.actor == 'you' (or a parenthesized OR of trusted actors), and
//  2. when the workflow runs on pull_request, a same-repository clause
//     (github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository).
//
// Anything else ANDed on only narrows it further. A top-level `||` would widen it, so it is
// refused. The check is deliberately strict and textual: a guard that cannot be proven is
// treated as absent, and the error prints the exact line to write.

const prGuardClause = "github.event_name!='pull_request'||github.event.pull_request.head.repo.full_name==github.repository"

// CanonicalIf is the `if:` expression that satisfies the policy; `generate` (BR-3) emits it.
func CanonicalIf(actors []string, prGuard bool) string {
	var actor string
	if len(actors) == 1 {
		actor = "github.actor == '" + actors[0] + "'"
	} else {
		parts := make([]string, len(actors))
		for i, a := range actors {
			parts[i] = "github.actor == '" + a + "'"
		}
		actor = "(" + strings.Join(parts, " || ") + ")"
	}
	if !prGuard {
		return actor
	}
	return actor + " && (github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository)"
}

// normalize drops ${{ }}, whitespace and quote style so equivalent spellings compare equal.
func normalize(expr string) string {
	s := strings.TrimSpace(expr)
	if len(s) >= 2 && (s[0] == '\'' || s[0] == '"') && s[len(s)-1] == s[0] && !strings.Contains(s[1:len(s)-1], string(s[0])) {
		s = s[1 : len(s)-1]
	}
	s = strings.ReplaceAll(s, "${{", "")
	s = strings.ReplaceAll(s, "}}", "")
	s = strings.ReplaceAll(s, `"`, `'`)
	return strings.Join(strings.Fields(s), "")
}

// splitTop splits s on op at parenthesis depth 0, outside quotes.
func splitTop(s, op string) []string {
	var parts []string
	depth, start := 0, 0
	inQuote := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\'':
			inQuote = !inQuote
		case inQuote:
		case c == '(':
			depth++
		case c == ')':
			depth--
		case depth == 0 && strings.HasPrefix(s[i:], op):
			parts = append(parts, s[start:i])
			start = i + len(op)
			i += len(op) - 1
		}
	}
	return append(parts, s[start:])
}

// stripParens removes parentheses that wrap the whole clause.
func stripParens(s string) string {
	for len(s) >= 2 && s[0] == '(' && s[len(s)-1] == ')' {
		depth := 0
		wrapsAll := true
		for i := 0; i < len(s)-1; i++ {
			switch s[i] {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					wrapsAll = false
				}
			}
			if !wrapsAll {
				break
			}
		}
		if !wrapsAll {
			return s
		}
		s = s[1 : len(s)-1]
	}
	return s
}

var actorRe = regexp.MustCompile(`^github\.actor=='([^']+)'$`)

// actorClause reports whether clause allows only trusted actors: one equality, or an OR of them.
func actorClause(clause string, trusted []string) bool {
	alts := splitTop(clause, "||")
	for _, alt := range alts {
		m := actorRe.FindStringSubmatch(stripParens(alt))
		if m == nil {
			return false
		}
		ok := false
		for _, t := range trusted {
			if strings.EqualFold(m[1], t) { // GitHub logins are case-insensitive
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	return len(alts) > 0
}

// CheckIf judges a job's `if:` against the policy. needPR says the workflow can run on
// pull_request. It returns "" when the expression is acceptable, else the reason.
func CheckIf(expr string, trusted []string, needPR bool) string {
	if strings.TrimSpace(expr) == "" {
		return "the job has no `if:` condition"
	}
	n := normalize(expr)
	if len(splitTop(n, "||")) > 1 {
		return "the `if:` has a top-level `||`, which can let other actors through; wrap any OR in parentheses and AND it with the actor check"
	}
	var hasActor, hasPR bool
	for _, clause := range splitTop(n, "&&") {
		c := stripParens(clause)
		if actorClause(c, trusted) {
			hasActor = true
		}
		if c == prGuardClause {
			hasPR = true
		}
	}
	switch {
	case !hasActor:
		return "the `if:` does not restrict the job to a trusted actor (" + strings.Join(trusted, ", ") + ")"
	case needPR && !hasPR:
		return "the workflow runs on pull_request but the `if:` lacks the same-repository clause, so a PR updated by you from someone else's fork would run their code"
	}
	return ""
}
