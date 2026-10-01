// Package admit decides whether a commit may run on this machine. It is the join between the
// provider (which supplies a commit's signed bytes) and internal/trust (which verifies them
// against keys the owner chose): the machine, not GitHub, makes the decision.
package admit

import (
	"context"

	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/provider"
	"github.com/modullar/blade-runner/internal/trust"
)

// Subject is what would run: a repository at a commit. For a push it is the pushed commit; for a
// pull request it is the pull request's HEAD commit (never GitHub's synthetic merge commit,
// which nobody signed).
type Subject struct {
	Repository string
	SHA        string
}

// Admitter checks commits against the trust store.
type Admitter struct {
	Provider provider.Provider
	Verifier *trust.Verifier
}

// Admit returns who vouched for the commit, or a BR-E067 error saying why it is not admitted.
func (a *Admitter) Admit(ctx context.Context, s Subject) (trust.Verdict, error) {
	c, err := a.Provider.Commit(ctx, s.Repository, s.SHA)
	if err != nil {
		if diag.CodeOf(err) == "" {
			return trust.Verdict{}, diag.Wrap(err, diag.CodeNotAdmitted, "cannot fetch commit "+s.SHA, "GitHub could not be reached", "re-run")
		}
		return trust.Verdict{}, err
	}
	// The id is what was asked for, never what the server claims: a server that answers with a
	// different commit must not be able to choose what is verified.
	return a.Verifier.Verify(trust.Commit{SHA: s.SHA, Payload: c.Payload, Signature: c.Signature})
}
