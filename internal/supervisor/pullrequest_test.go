package supervisor_test

import (
	"strings"
	"testing"

	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/provider"
	"github.com/modullar/blade-runner/internal/supervisor"
)

// See docs/decisions/0007-supervisor.md, "Open: PRs run the merge commit". For a pull_request
// run GitHub's head_sha is the PR head, but the job executes GITHUB_SHA, the synthetic merge of
// that head and the base branch, with the workflow file from the merge. Only the head is verified.

func TestPullRequestRunsAreRefusedUnlessTheOwnerOptsIn(t *testing.T) {
	r := newRig(t, withConfig(func(c *supervisor.Config) { c.AllowPullRequestMerge = false }))
	// The head commit is the owner's, correctly signed: admission of the head would pass. The
	// refusal is about what actually runs, not about who signed the head.
	r.queue(q{run: 2, job: 20, event: "pull_request", headRepo: repo,
		prs: []provider.PullRequest{{Number: 7, HeadSHA: r.commits["owner"].SHA, HeadRepository: repo}}})
	out, err := r.sup.Tick(ctx)
	if err != nil || out.Launched || !out.Withheld || len(out.Refused) != 1 || out.Refused[0].Code != diag.CodeJobEventRefused {
		t.Fatalf("outcome = %+v, err = %v", out, err)
	}
	if !strings.Contains(out.Refused[0].Reason, "allow_pull_request_merge") || !strings.Contains(out.Refused[0].Reason, "merge") {
		t.Errorf("the refusal must say what to change: %q", out.Refused[0].Reason)
	}
	r.mustStartNothing()
	for _, req := range r.srv.Requests() {
		if strings.Contains(req, "/git/commits/") {
			t.Errorf("a commit was fetched for a run that is refused by event alone: %s", req)
		}
	}
}

func TestPullRequestRunsAreJudgedOnTheirHeadWhenTheOwnerOptsIn(t *testing.T) {
	r := newRig(t) // the rig opts in
	r.queue(q{run: 2, job: 20, event: "pull_request", headRepo: repo,
		prs: []provider.PullRequest{{Number: 7, HeadSHA: r.commits["owner"].SHA, HeadRepository: repo}}})
	r.rt.OnRun = r.takes(repo, 20, 2)
	if out, err := r.sup.Tick(ctx); err != nil || !out.Launched {
		t.Fatalf("%+v, %v", out, err)
	}
}

// What is NOT closed even with the switch on, as a test that documents it: admission is a
// statement about the PR head. The merge commit the job really runs is never fetched, never
// verified, and contains whatever the base branch holds, signed or not.
func TestKnownLimit_AnOptedInPullRequestVerifiesTheHeadAndNeverTheMergeCommit(t *testing.T) {
	r := newRig(t)
	head := r.commits["owner"].SHA
	r.queue(q{run: 2, job: 20, event: "pull_request", headRepo: repo,
		prs: []provider.PullRequest{{Number: 7, HeadSHA: head, HeadRepository: repo}}})
	r.rt.OnRun = r.takes(repo, 20, 2)
	if out, err := r.sup.Tick(ctx); err != nil || !out.Launched {
		t.Fatalf("%+v, %v", out, err)
	}
	fetched := map[string]bool{}
	for _, req := range r.srv.Requests() {
		if strings.Contains(req, "/git/commits/") {
			fetched[req] = true
		}
	}
	if len(fetched) != 1 || !fetched["GET /repos/"+repo+"/git/commits/"+head] {
		t.Errorf("commits fetched = %v: the head is the only thing verified, and the merge commit it is merged into is not", fetched)
	}
}
