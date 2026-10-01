package diag

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestEveryCodeIsUniqueWellFormedAndDocumented(t *testing.T) {
	doc, err := os.ReadFile("../../docs/errors/README.md")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`^BR-E\d{3}$`)
	seen := map[string]bool{}
	for _, code := range All() {
		if !re.MatchString(code) {
			t.Errorf("code %q is not BR-Exxx", code)
		}
		if seen[code] {
			t.Errorf("code %q is declared twice", code)
		}
		seen[code] = true
		if !strings.Contains(string(doc), "\n### "+code+"\n") {
			t.Errorf("code %s has no section in docs/errors/README.md", code)
		}
	}
	// And no documented code is missing from All(): a page for a code nobody can produce.
	for _, m := range regexp.MustCompile(`(?m)^### (BR-E\d{3})$`).FindAllStringSubmatch(string(doc), -1) {
		if !seen[m[1]] {
			t.Errorf("docs describe %s, but diag.All() does not declare it", m[1])
		}
	}
}

func TestDocsURLAnchorsMatchTheHeadings(t *testing.T) {
	if got := DocsURL("BR-E021"); !strings.HasSuffix(got, "#br-e021") {
		t.Errorf("DocsURL = %s", got)
	}
}

func TestErrorMessageHasWhatCauseFixAndDocs(t *testing.T) {
	e := Wrap(errors.New("boom"), CodeTokenRejected, "GitHub rejected the token", "it expired", "make a new one")
	msg := e.Error()
	for _, want := range []string{
		"BR-E021: GitHub rejected the token",
		"likely cause: it expired",
		"fix: make a new one",
		"detail: boom",
		"docs: " + DocsURL(CodeTokenRejected),
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
	// The same error survives wrapping by callers.
	wrapped := fmt.Errorf("apply: %w", e)
	if CodeOf(wrapped) != CodeTokenRejected {
		t.Errorf("CodeOf through fmt.Errorf %%w = %q", CodeOf(wrapped))
	}
	if !errors.Is(wrapped, e.Err) {
		t.Error("the underlying cause must stay reachable with errors.Is")
	}
	if CodeOf(errors.New("plain")) != "" {
		t.Error("CodeOf a plain error must be empty")
	}
}

func TestOptionalPartsAreOmitted(t *testing.T) {
	msg := New(CodeStateCorrupt, "state is bad", "", "").Error()
	if strings.Contains(msg, "likely cause") || strings.Contains(msg, "fix:") || strings.Contains(msg, "detail:") {
		t.Errorf("empty parts should not print:\n%s", msg)
	}
}
