package supervisor_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/isolation"
	"github.com/modullar/blade-runner/internal/provider"
	"github.com/modullar/blade-runner/internal/provider/github/githubtest"
	"github.com/modullar/blade-runner/internal/supervisor"
)

// Pull requests: every commit verified (docs/decisions/0007-supervisor.md). A pull_request job
// runs GitHub's merge of the head into the base, so it is admitted only when that merge is of
// the verified base tip and the verified head, and every commit of the pull request is signed by
// a key in the trust store. All commits here are real: made by git, signed by ssh-keygen.

// mustRefuse asserts the one outcome of a refused pull request: one refusal with this code, which
// names every string in contains, and nothing started anywhere.
func mustRefuse(t *testing.T, p *prWorld, contains ...string) supervisor.Refusal {
	t.Helper()
	out, err := p.sup.Tick(ctx)
	if err != nil {
		t.Fatalf("a refusal is correct behaviour, not a failure: %v", err)
	}
	if len(out.Refused) != 1 || out.Refused[0].Code != diag.CodeJobRefused || !out.Withheld || out.Launched {
		t.Fatalf("outcome = %+v, want one BR-E072 refusal", out)
	}
	for _, c := range contains {
		if !strings.Contains(out.Refused[0].Reason, c) {
			t.Errorf("reason %q should mention %q", out.Refused[0].Reason, c)
		}
	}
	p.mustStartNothing()
	if got := p.entriesOfKind(supervisor.KindRefused); len(got) != 1 || got[0].Code != diag.CodeJobRefused || got[0].JobID != 10 {
		t.Errorf("audit refusals = %+v", got)
	}
	if got := p.entriesOfKind(supervisor.KindLaunching); len(got) != 0 {
		t.Errorf("a launch was recorded for a refused job: %+v", got)
	}
	return out.Refused[0]
}

func mustWithhold(t *testing.T, p *prWorld, contains string) {
	t.Helper()
	out, err := p.sup.Tick(ctx)
	if diag.CodeOf(err) != diag.CodeQueueUnreadable || !out.Withheld || out.Launched || len(out.Refused) != 0 {
		t.Fatalf("outcome = %+v, err = %v: want a transient withhold (BR-E076), never a refusal", out, err)
	}
	if !strings.Contains(err.Error(), contains) {
		t.Errorf("error %q should mention %q", err.Error(), contains)
	}
	p.mustStartNothing()
	if got := p.entriesOfKind(supervisor.KindRefused); len(got) != 0 {
		t.Errorf("a transient state was recorded as a refusal: %+v", got)
	}
}

func mustLaunch(t *testing.T, p *prWorld) {
	t.Helper()
	p.rt.OnRun = p.takes(repo, 10, 1)
	out, err := p.sup.Tick(ctx)
	if err != nil || !out.Launched || out.JobID != 10 {
		t.Fatalf("outcome = %+v, err = %v: want the runner launched", out, err)
	}
}

func TestPullRequestWithTheBaseTipAndEveryCommitTrustedIsAdmittedAndAudited(t *testing.T) {
	p := newPRWorld(t, prSpec{})
	p.queuePR(1, 10)
	mustLaunch(t, p)

	if n := len(p.rt.Specs()); n != 1 {
		t.Fatalf("containers started = %d, want 1", n)
	}
	launching := p.entriesOfKind(supervisor.KindLaunching)
	if len(launching) != 1 {
		t.Fatalf("launching entries = %+v", launching)
	}
	e := launching[0]
	if e.MergeSHA != p.merge.SHA || e.VerifiedTotal != 4 || len(e.Verified) != 4 {
		t.Fatalf("launching entry = %+v: want the merge commit and base tip + 3 pull request commits", e)
	}
	want := []supervisor.VerifiedCommit{
		{Role: "base-tip", Repository: repo, SHA: p.baseTip.SHA, Signer: "maint", Fingerprint: p.fingerprint("maint")},
		{Role: "pr-commit", Repository: repo, SHA: p.commits[0].SHA, Signer: "alice", Fingerprint: p.fingerprint("alice")},
		{Role: "pr-commit", Repository: repo, SHA: p.commits[1].SHA, Signer: "bob", Fingerprint: p.fingerprint("bob")},
		{Role: "pr-commit", Repository: repo, SHA: p.commits[2].SHA, Signer: "alice", Fingerprint: p.fingerprint("alice")},
	}
	for i, w := range want {
		if e.Verified[i] != w {
			t.Errorf("verified[%d] = %+v, want %+v", i, e.Verified[i], w)
		}
	}
	// The head commit's signer is who the entry names as having vouched.
	if e.Signer != "alice" || e.SHA != p.head().SHA || e.Repository != repo {
		t.Errorf("entry names %s %s in %s", e.Signer, short12(e.SHA), e.Repository)
	}
	if !strings.Contains(e.Message, p.merge.SHA) {
		t.Errorf("message %q should name the merge commit", e.Message)
	}
	if n, err := supervisor.VerifyAuditLog(p.auditPath); err != nil {
		t.Errorf("audit chain: %d, %v", n, err)
	}
}

func short12(s string) string { return s[:12] }

func TestAForkPullRequestIsAdmittedWhenTheForkOnlyServesItsOwnCommits(t *testing.T) {
	// Every commit is fetched from the base repository first and from the fork when the base
	// cannot serve it (assumption C12: GitHub normally serves fork commits through the base).
	p := newPRWorld(t, prSpec{fork: true, forkOnly: true})
	p.queuePR(1, 10)
	mustLaunch(t, p)
	forkFetches := 0
	for _, f := range p.commitFetches() {
		if strings.Contains(f, "/repos/"+fork+"/") {
			forkFetches++
		}
	}
	if forkFetches < 3 {
		t.Errorf("fork fetches = %d, want the 3 commits that only the fork serves", forkFetches)
	}
}

func TestAForkPullRequestFromAContributorNobodyTrustedIsRefusedAndTouchesNothing(t *testing.T) {
	p := newPRWorld(t, prSpec{fork: true, authors: []string{"mallory-w"}})
	p.queuePR(1, 10)
	mustRefuse(t, p, "pull request #7", p.commits[0].SHA, "does not trust", p.fingerprint("mallory-w"), "1 of 1")
	for _, req := range p.srv.Requests() {
		if strings.Contains(req, "generate-jitconfig") {
			t.Errorf("a runner was registered: %s", req)
		}
	}
}

func TestOneUnsignedCommitInTheMiddleRefusesTheWholePullRequest(t *testing.T) {
	p := newPRWorld(t, prSpec{authors: []string{"alice", "-", "bob", "alice"}})
	p.queuePR(1, 10)
	mustRefuse(t, p, p.commits[1].SHA, "not signed", "2 of 4")
}

func TestAnUnsignedBaseTipRefusesThePullRequest(t *testing.T) {
	p := newPRWorld(t, prSpec{tipBy: "-"})
	p.queuePR(1, 10)
	mustRefuse(t, p, "tip of the base branch", p.baseTip.SHA, "not signed")
}

func TestABaseTipSignedByAStrangerRefusesThePullRequest(t *testing.T) {
	p := newPRWorld(t, prSpec{tipBy: "mallory-w"})
	p.queuePR(1, 10)
	mustRefuse(t, p, "tip of the base branch", p.baseTip.SHA, "does not trust")
}

func TestARevokedOrExpiredSignerOnAnyCommitRefusesThePullRequest(t *testing.T) {
	t.Run("a contributor is revoked", func(t *testing.T) {
		p := newPRWorld(t, prSpec{})
		if _, err := p.trust.Revoke("bob"); err != nil {
			t.Fatal(err)
		}
		p.queuePR(1, 10)
		mustRefuse(t, p, "revoked", `"bob"`, p.commits[1].SHA)
	})
	t.Run("the base tip's signer is revoked", func(t *testing.T) {
		p := newPRWorld(t, prSpec{})
		if _, err := p.trust.Revoke("maint"); err != nil {
			t.Fatal(err)
		}
		p.queuePR(1, 10)
		mustRefuse(t, p, "revoked", `"maint"`, "tip of the base branch")
	})
	t.Run("a contributor's grant has expired", func(t *testing.T) {
		p := newPRWorld(t, prSpec{trust: []string{"maint", "alice"}})
		if _, err := p.trust.Add("bob", p.pubKey("bob"), time.Now().Add(24*time.Hour)); err != nil {
			t.Fatal(err)
		}
		p.trust.Now = func() time.Time { return time.Now().Add(48 * time.Hour) }
		p.queuePR(1, 10)
		mustRefuse(t, p, "expired", `"bob"`, p.commits[1].SHA)
	})
}

func TestAMergeCommitThatIsNotOfTheBaseTipAndTheHeadIsRefused(t *testing.T) {
	other := strings.Repeat("e", 40)
	for name, tc := range map[string]struct {
		parents  func(p *prWorld) []string
		contains string
	}{
		"swapped":                       {func(p *prWorld) []string { return []string{p.head().SHA, p.baseTip.SHA} }, "first parent"},
		"first parent is not the tip":   {func(p *prWorld) []string { return []string{other, p.head().SHA} }, "first parent"},
		"second parent is not the head": {func(p *prWorld) []string { return []string{p.baseTip.SHA, other} }, "second parent"},
		"one parent":                    {func(p *prWorld) []string { return []string{p.baseTip.SHA} }, "1 parents"},
		"three parents":                 {func(p *prWorld) []string { return []string{p.baseTip.SHA, p.head().SHA, other} }, "3 parents"},
		"no parents":                    {func(p *prWorld) []string { return nil }, "0 parents"},
	} {
		t.Run(name, func(t *testing.T) {
			p := newPRWorld(t, prSpec{})
			p.srv.SetCommitParents(repo, p.merge.SHA, tc.parents(p))
			p.queuePR(1, 10)
			mustRefuse(t, p, tc.contains, p.merge.SHA)
		})
	}
}

func TestAMergeThatIsNotYetComputedIsWithheldThenAdmittedOnceItIs(t *testing.T) {
	p := newPRWorld(t, prSpec{})
	p.editPR(func(i *provider.PullRequestInfo) { i.Mergeable, i.MergeCommitSHA = nil, "" })
	p.queuePR(1, 10)
	mustWithhold(t, p, "mergeable is null")
	if n := len(p.commitFetches()); n != 0 {
		t.Errorf("%d commits were fetched while the merge did not exist", n)
	}
	yes := true
	p.editPR(func(i *provider.PullRequestInfo) { i.Mergeable, i.MergeCommitSHA = &yes, p.merge.SHA })
	mustLaunch(t, p)
}

func TestAPullRequestThatCannotBeMergedIsRefused(t *testing.T) {
	p := newPRWorld(t, prSpec{})
	no := false
	p.editPR(func(i *provider.PullRequestInfo) { i.Mergeable = &no })
	p.queuePR(1, 10)
	mustRefuse(t, p, "cannot be merged")
}

func TestAMergeableDeclarationWithoutAMergeCommitIsRefused(t *testing.T) {
	p := newPRWorld(t, prSpec{})
	p.editPR(func(i *provider.PullRequestInfo) { i.MergeCommitSHA = "" })
	p.queuePR(1, 10)
	mustRefuse(t, p, "no usable merge commit")
}

func TestTheMergeCommitNotBeingFetchableWithholdsTheJob(t *testing.T) {
	p := newPRWorld(t, prSpec{})
	p.srv.SetCommitStatus(p.merge.SHA, 503)
	p.queuePR(1, 10)
	mustWithhold(t, p, p.merge.SHA)
	p.srv.SetCommitStatus(p.merge.SHA, 0)
	mustLaunch(t, p)
}

func TestMoreCommitsThanGitHubWillListIsRefusedNotVerifiedAsASubset(t *testing.T) {
	fake := func(n int) []provider.PullRequestCommit {
		var l []provider.PullRequestCommit
		for i := 0; i < n; i++ {
			l = append(l, provider.PullRequestCommit{SHA: fmt.Sprintf("%040x", i+1)})
		}
		return l
	}
	t.Run("300 commits: GitHub lists 250", func(t *testing.T) {
		p := newPRWorld(t, prSpec{})
		list := fake(299)
		list = append(list, provider.PullRequestCommit{SHA: p.head().SHA})
		p.srv.UpdatePullRequest(repo, p.number, func(pr *githubtest.PullRequest) { pr.Commits = list })
		p.queuePR(1, 10)
		mustRefuse(t, p, "too many commits to verify", "250", "300")
		if n := p.requestsContaining("/git/commits/" + fmt.Sprintf("%040x", 1)); n != 0 {
			t.Errorf("a subset of the commits was fetched anyway")
		}
	})
	t.Run("exactly 250 commits is the cap itself: a list that may have been cut", func(t *testing.T) {
		p := newPRWorld(t, prSpec{})
		list := append(fake(249), provider.PullRequestCommit{SHA: p.head().SHA})
		p.srv.UpdatePullRequest(repo, p.number, func(pr *githubtest.PullRequest) { pr.Commits = list })
		p.queuePR(1, 10)
		mustRefuse(t, p, "too many commits to verify")
	})
	t.Run("a listing shorter than the pull request's own count", func(t *testing.T) {
		p := newPRWorld(t, prSpec{})
		p.srv.UpdatePullRequest(repo, p.number, func(pr *githubtest.PullRequest) { pr.ListLimit = 2 })
		p.queuePR(1, 10)
		mustRefuse(t, p, "too many commits to verify", "holds 2", "has 3")
	})
}

func TestAPullRequestCommitThatCannotBeFetchedIsWithheldNotRefused(t *testing.T) {
	p := newPRWorld(t, prSpec{})
	missing := p.commits[1].SHA
	p.srv.SetCommitStatus(missing, 503)
	p.queuePR(1, 10)
	mustWithhold(t, p, missing)

	// Not there at all (404 while the repository reads fine): still "not now".
	p.srv.SetCommitStatus(missing, 404)
	p.srv.SetRepoStatus(repo, 0)
	mustWithhold(t, p, missing)

	p.srv.SetCommitStatus(missing, 0)
	mustLaunch(t, p)
}

func TestARefusalOutranksACommitThatCouldNotBeFetched(t *testing.T) {
	p := newPRWorld(t, prSpec{authors: []string{"alice", "bob", "mallory-w"}})
	p.srv.SetCommitStatus(p.commits[0].SHA, 503)
	p.queuePR(1, 10)
	mustRefuse(t, p, p.commits[2].SHA, "does not trust")
}

func TestARunWhoseHeadIsNotThePullRequestsHeadIsRefused(t *testing.T) {
	p := newPRWorld(t, prSpec{})
	p.editPR(func(i *provider.PullRequestInfo) { i.HeadSHA = strings.Repeat("d", 40) })
	p.queuePR(1, 10)
	mustRefuse(t, p, "now at head", "stale")
}

func TestAPullRequestThatIsClosedOrAgainstAnotherRepositoryIsRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		edit     func(*provider.PullRequestInfo)
		contains string
	}{
		"closed":                  {func(i *provider.PullRequestInfo) { i.State = "closed" }, "not open"},
		"another base":            {func(i *provider.PullRequestInfo) { i.BaseRepository = "other/repo" }, "against"},
		"another head repository": {func(i *provider.PullRequestInfo) { i.HeadRepository = "other/repo" }, "other/repo"},
		"head repository gone":    {func(i *provider.PullRequestInfo) { i.HeadRepository = "" }, "no longer says"},
		"unusable base":           {func(i *provider.PullRequestInfo) { i.BaseSHA = "abc" }, "base commit id"},
	} {
		t.Run(name, func(t *testing.T) {
			p := newPRWorld(t, prSpec{})
			p.editPR(tc.edit)
			p.queuePR(1, 10)
			mustRefuse(t, p, tc.contains)
		})
	}
}

func TestAPullRequestRunMustNameExactlyOnePullRequest(t *testing.T) {
	t.Run("none", func(t *testing.T) {
		p := newPRWorld(t, prSpec{})
		p.queuePR(1, 10, func(r *provider.Run) { r.PullRequests = nil })
		mustRefuse(t, p, "exactly one pull request", "names 0")
		if n := p.requestsContaining("/pulls/"); n != 0 {
			t.Errorf("%d pull request calls for a run that names none", n)
		}
	})
	t.Run("two", func(t *testing.T) {
		p := newPRWorld(t, prSpec{})
		p.queuePR(1, 10, func(r *provider.Run) {
			r.PullRequests = append(r.PullRequests, provider.PullRequest{Number: 8, HeadSHA: r.HeadSHA, HeadRepository: r.HeadRepository})
		})
		mustRefuse(t, p, "exactly one pull request", "names 2")
	})
}

func TestTheHeadMustBeAmongTheCommitsGitHubLists(t *testing.T) {
	p := newPRWorld(t, prSpec{})
	p.srv.UpdatePullRequest(repo, p.number, func(pr *githubtest.PullRequest) { pr.Commits = pr.Commits[:2] })
	p.queuePR(1, 10)
	mustRefuse(t, p, "not among the")
}

func TestPullRequestTargetIsStillRefusedWhateverIsInTheRepository(t *testing.T) {
	p := newPRWorld(t, prSpec{})
	p.queuePR(1, 10, func(r *provider.Run) { r.Event = "pull_request_target" })
	out, err := p.sup.Tick(ctx)
	if err != nil || len(out.Refused) != 1 || out.Refused[0].Code != diag.CodeJobEventRefused || !strings.Contains(out.Refused[0].Reason, "pull_request_target") {
		t.Fatalf("outcome = %+v, err = %v", out, err)
	}
	p.mustStartNothing()
	if n := p.requestsContaining("/pulls/"); n != 0 {
		t.Errorf("a refused event still cost %d pull request calls", n)
	}
}

func TestAMatrixOfJobsReadsThePullRequestOnce(t *testing.T) {
	p := newPRWorld(t, prSpec{})
	p.queuePR(1, 10)
	p.srv.AddJob(repo, provider.Job{ID: 11, RunID: 1, Status: "waiting", Labels: []string{"self-hosted", "gpu"}, HeadSHA: p.head().SHA})
	p.srv.AddJob(repo, provider.Job{ID: 12, RunID: 1, Status: "waiting", Labels: []string{"self-hosted", "gpu"}, HeadSHA: p.head().SHA})
	// (waiting jobs are judged but not started: this is one assessment and nothing more)
	p.srv.UpdateJob(repo, 10, func(j *provider.Job) { j.Status = "waiting" })
	out, err := p.sup.Tick(ctx)
	if err != nil || !out.Idle {
		t.Fatalf("%+v, %v", out, err)
	}
	if n := p.requestsContaining("/pulls/7"); n != 2 { // the pull request, and its commit list
		t.Errorf("pull request calls = %d, want 2 (the pull request and its commits, once for three jobs)", n)
	}
}

func TestTheApiCostOfOnePullRequestIsThreePlusItsCommits(t *testing.T) {
	p := newPRWorld(t, prSpec{})
	p.queuePR(1, 10)
	p.srv.UpdateJob(repo, 10, func(j *provider.Job) { j.Status = "waiting" })
	if out, err := p.sup.Tick(ctx); err != nil || !out.Idle {
		t.Fatalf("%+v, %v", out, err)
	}
	// 1 pull request + 1 merge commit + 1 base tip + 3 commits = 6 requests for commits and the
	// pull request, plus one page of the commit list.
	if n, want := len(p.commitFetches()), 1+1+3; n != want {
		t.Errorf("commit fetches = %d, want %d (merge commit, base tip, 3 commits)", n, want)
	}
	if n := p.requestsContaining("/pulls/7"); n != 2 {
		t.Errorf("pull request calls = %d, want 2", n)
	}
}

func TestAnAuditRecordListsAtMostFiftyCommitsAndCountsTheRest(t *testing.T) {
	authors := make([]string, 55)
	for i := range authors {
		authors[i] = "alice"
	}
	p := newPRWorld(t, prSpec{authors: authors})
	p.queuePR(1, 10)
	mustLaunch(t, p)
	e := p.entriesOfKind(supervisor.KindLaunching)[0]
	if len(e.Verified) != 50 || e.VerifiedTotal != 56 || e.MergeSHA != p.merge.SHA {
		t.Errorf("listed %d of %d, merge %s", len(e.Verified), e.VerifiedTotal, e.MergeSHA)
	}
	if e.Verified[0].Role != "base-tip" {
		t.Errorf("the first listed commit is %+v", e.Verified[0])
	}
}

// ---- the merge commit moving after admission -------------------------------------------------

func TestTheBaseMovingBetweenJudgementAndLaunchStartsNothingAndIsVerifiedAgain(t *testing.T) {
	var p *prWorld
	hook := &afterJIT{hook: func() { p.moveBase() }}
	p = newPRWorld(t, prSpec{}, withProvider(func(pr provider.Provider) provider.Provider { hook.Provider = pr; return hook }))
	p.queuePR(1, 10)
	oldMerge := p.merge.SHA
	p.rt.OnRun = p.takes(repo, 10, 1)

	out, err := p.sup.Tick(ctx)
	if err != nil || out.Launched || !out.Withheld {
		t.Fatalf("outcome = %+v, err = %v: the merge commit changed after admission, nothing may run", out, err)
	}
	if n := len(p.rt.Specs()); n != 0 {
		t.Fatalf("%d container(s) started on a merge commit that was not the one verified", n)
	}
	if rs := p.srv.Runners(repo); len(rs) != 0 {
		t.Errorf("registration left behind: %+v", rs)
	}
	w := p.entriesOfKind(supervisor.KindWithheld)
	if len(w) != 1 || !strings.Contains(w[0].Message, "merge commit") {
		t.Errorf("withheld entries = %+v", w)
	}
	if p.merge.SHA == oldMerge {
		t.Fatal("the test did not move the merge commit")
	}

	// The next poll judges the new merge commit from scratch (new tip, new merge) and runs it.
	out, err = p.sup.Tick(ctx)
	if err != nil || !out.Launched {
		t.Fatalf("after the new merge commit was verified: %+v, %v", out, err)
	}
	launches := p.entriesOfKind(supervisor.KindLaunching)
	last := launches[len(launches)-1]
	if last.MergeSHA != p.merge.SHA || last.Verified[0].SHA != p.baseTip.SHA {
		t.Errorf("last launch = merge %s, first verified %s; want the new merge %s and new tip %s", last.MergeSHA, last.Verified[0].SHA, p.merge.SHA, p.baseTip.SHA)
	}
}

func TestTheBaseMovingToAnUnsignedTipWhileTheRunnerWaitsStopsTheRunner(t *testing.T) {
	p := newPRWorld(t, prSpec{})
	p.queuePR(1, 10)
	stopped := false
	p.rt.OnRun = func(c context.Context, spec isolation.Spec) (isolation.Result, error) {
		// Someone pushes an UNSIGNED commit to the base branch and GitHub merges the head onto it.
		p.w.Detach(p.baseTip.SHA)
		tip := p.w.Commit("", "a.txt", "evil", "unsigned push to the base")
		p.srv.AddCommit(repo, tip.SHA, tip.Payload, tip.Signature)
		p.baseTip = tip
		merge := p.w.MergeCommit(p.head().SHA)
		p.srv.AddCommit(repo, merge.SHA, merge.Payload, merge.Signature)
		p.srv.UpdatePullRequest(repo, p.number, func(pr *githubtest.PullRequest) {
			pr.Info.BaseSHA, pr.Info.MergeCommitSHA = tip.SHA, merge.SHA
		})
		select {
		case <-c.Done():
			stopped = true
		case <-time.After(3 * time.Second):
		}
		return isolation.Result{}, nil
	}
	out, err := p.sup.Tick(ctx)
	if err != nil || !stopped || !out.Withheld {
		t.Fatalf("stopped = %v, outcome = %+v, err = %v: a runner waiting for a job whose merge now holds an unsigned commit must be stopped", stopped, out, err)
	}
	f := p.entriesOfKind(supervisor.KindFinished)
	if len(f) != 1 || f[0].Code != diag.CodeLaunchWithheld || !strings.Contains(f[0].Message, "no longer admitted") || !strings.Contains(f[0].Message, "not signed") {
		t.Errorf("finished = %+v", f)
	}
}

func TestTheMergeCommitMovingToAnotherVerifiedOneWhileTheRunnerWaitsStopsTheRunner(t *testing.T) {
	// Everything on the new merge commit is signed too: the runner is still stopped, because the
	// job it may be handed is the merge that was verified when it started, not this one.
	p := newPRWorld(t, prSpec{})
	p.queuePR(1, 10)
	stopped := false
	p.rt.OnRun = func(c context.Context, spec isolation.Spec) (isolation.Result, error) {
		p.moveBase()
		select {
		case <-c.Done():
			stopped = true
		case <-time.After(3 * time.Second):
		}
		return isolation.Result{}, nil
	}
	out, err := p.sup.Tick(ctx)
	if err != nil || !stopped || !out.Withheld {
		t.Fatalf("stopped = %v, outcome = %+v, err = %v", stopped, out, err)
	}
	f := p.entriesOfKind(supervisor.KindFinished)
	if len(f) != 1 || f[0].Code != diag.CodeLaunchWithheld || !strings.Contains(f[0].Message, "moved") {
		t.Errorf("finished = %+v", f)
	}
}

func TestOnceTheRunnerHasItsJobTheMergeCommitMovingDoesNotKillIt(t *testing.T) {
	p := newPRWorld(t, prSpec{})
	p.queuePR(1, 10)
	stopped := false
	p.rt.OnRun = func(c context.Context, spec isolation.Spec) (isolation.Result, error) {
		p.srv.UpdateJob(repo, 10, func(j *provider.Job) { j.Status, j.RunnerName = "in_progress", spec.Name })
		p.moveBase()
		select {
		case <-c.Done():
			stopped = true
		case <-time.After(150 * time.Millisecond):
		}
		p.srv.UpdateJob(repo, 10, func(j *provider.Job) { j.Status = "completed" })
		return isolation.Result{}, nil
	}
	if out, err := p.sup.Tick(ctx); err != nil || !out.Launched {
		t.Fatalf("%+v, %v", out, err)
	}
	if stopped {
		t.Error("a job that already had its runner was killed because the base moved")
	}
}

// ---- cost ---------------------------------------------------------------------------------------

func TestAdmittingTheSamePullRequestAgainCostsNoCommitFetchesButTheMergeCommit(t *testing.T) {
	// A commit never changes under its id, so the CLI's admitter remembers what it fetched; who is
	// trusted is still read from the trust store at every poll.
	p := newPRWorld(t, prSpec{})
	p.cfg.Admitter = cachedAdmitter(p)
	p.sup = p.newSupervisor()
	p.queuePR(1, 10)
	p.srv.UpdateJob(repo, 10, func(j *provider.Job) { j.Status = "waiting" })
	for i := 0; i < 2; i++ {
		if out, err := p.sup.Tick(ctx); err != nil || !out.Idle {
			t.Fatalf("%+v, %v", out, err)
		}
	}
	if n, want := len(p.commitFetches()), (1+1+3)+1; n != want {
		t.Errorf("commit fetches over two polls = %d, want %d (everything once, then only the merge commit)", n, want)
	}
	// Revocation still takes effect at the very next poll, cache or not.
	if _, err := p.trust.Revoke("bob"); err != nil {
		t.Fatal(err)
	}
	p.srv.UpdateJob(repo, 10, func(j *provider.Job) { j.Status = "queued" })
	mustRefuse(t, p, "revoked", `"bob"`)
}

func TestTheBaseTipOrTheCommitListNotBeingReadableWithholdsTheJob(t *testing.T) {
	t.Run("the base tip cannot be fetched", func(t *testing.T) {
		p := newPRWorld(t, prSpec{})
		p.srv.SetCommitStatus(p.baseTip.SHA, 503)
		p.queuePR(1, 10)
		mustWithhold(t, p, p.baseTip.SHA)
		p.srv.SetCommitStatus(p.baseTip.SHA, 0)
		mustLaunch(t, p)
	})
	t.Run("the commit list cannot be read", func(t *testing.T) {
		p := newPRWorld(t, prSpec{})
		p.srv.SetFailPath("/pulls/7/commits")
		p.queuePR(1, 10)
		mustWithhold(t, p, "cannot list the commits")
		p.srv.SetFailPath("")
		mustLaunch(t, p)
	})
	t.Run("the pull request cannot be read", func(t *testing.T) {
		p := newPRWorld(t, prSpec{})
		p.srv.SetFailPath("/pulls/7")
		p.queuePR(1, 10)
		mustWithhold(t, p, "cannot read pull request #7")
		p.srv.SetFailPath("")
		mustLaunch(t, p)
	})
}

func TestACommitListWithAnUnusableIdIsRefusedBeforeAnythingIsFetchedForIt(t *testing.T) {
	p := newPRWorld(t, prSpec{})
	p.srv.UpdatePullRequest(repo, p.number, func(pr *githubtest.PullRequest) {
		pr.Commits = append([]provider.PullRequestCommit{{SHA: "not-a-sha"}}, pr.Commits...)
	})
	p.queuePR(1, 10)
	mustRefuse(t, p, "unusable id", "not-a-sha")
}

// ---- the run's own pull_requests[] entry against the pull request that was fetched --------------

func TestARunListingAPullRequestAgainstAnotherBranchIsRefused(t *testing.T) {
	// Two open pull requests with the very same head commit, one into main and one into release.
	// The run lists #7 but was created for the release one: the merge commit it runs is not the
	// merge of #7 into main, so what is verified for #7 says nothing about it.
	p := newPRWorld(t, prSpec{})
	yes := true
	p.srv.SetPullRequest(repo, githubtest.PullRequest{
		Info: provider.PullRequestInfo{Number: 8, State: "open", BaseRepository: repo, BaseRef: "release", BaseSHA: p.baseTip.SHA,
			HeadRepository: p.headRep, HeadSHA: p.head().SHA, MergeCommitSHA: p.merge.SHA, Mergeable: &yes},
		Commits: []provider.PullRequestCommit{{SHA: p.commits[0].SHA}, {SHA: p.commits[1].SHA}, {SHA: p.head().SHA}},
	})
	p.queuePR(1, 10, func(r *provider.Run) { r.PullRequests[0].BaseRef = "release" })
	mustRefuse(t, p, `"main"`, `"release"`, "against branch")
}

func TestAPullRequestWhoseBaseMovedSinceTheRunWasCreatedIsWithheldNotRefused(t *testing.T) {
	p := newPRWorld(t, prSpec{})
	old := p.baseTip.SHA
	p.moveBase() // GitHub merged the head onto a new, signed tip
	p.queuePR(1, 10, func(r *provider.Run) { r.PullRequests[0].BaseSHA = old })
	mustWithhold(t, p, "moved from "+short12(old))
	// Judged again at every poll; the run's own record is what it is, so it stays withheld, and
	// once the run records the base the pull request has now, it is admitted.
	mustWithhold(t, p, "moved")
	p.srv.UpdateRun(repo, 1, func(r *provider.Run) { r.PullRequests[0].BaseSHA = p.baseTip.SHA })
	mustLaunch(t, p)
}

func TestAnAnswerForAnotherPullRequestNumberIsRefused(t *testing.T) {
	p := newPRWorld(t, prSpec{})
	p.editPR(func(i *provider.PullRequestInfo) { i.Number = 99 })
	p.queuePR(1, 10)
	mustRefuse(t, p, "#99", "#7")
}

// ---- the verdict is per run, not per pull request number -----------------------------------------

func TestTwoRunsOfOnePullRequestNumberAreEachJudgedOnTheirOwnHeadAndRepository(t *testing.T) {
	// One assessment, three runs that all say "pull request #7": the current one, a stale one for
	// an older head commit, and one claiming another repository for the same head commit. The
	// verdict on the first must not be handed to the others (or theirs to it).
	p := newPRWorld(t, prSpec{})
	old := p.commits[1].SHA
	p.queuePR(1, 10)
	p.queuePR(2, 20, func(r *provider.Run) {
		r.HeadSHA = old
		r.PullRequests[0].HeadSHA = old
	})
	p.queuePR(3, 30, func(r *provider.Run) {
		r.HeadRepository = fork
		r.PullRequests[0].HeadRepository = fork
	})
	out, err := p.sup.Tick(ctx)
	if err != nil || out.Launched || !out.Withheld {
		t.Fatalf("outcome = %+v, err = %v", out, err)
	}
	got := map[int64]string{}
	for _, r := range out.Refused {
		got[r.RunID] = r.Reason
	}
	if len(got) != 2 || !strings.Contains(got[2], "stale") || !strings.Contains(got[3], "says its head lives in") {
		t.Errorf("refusals = %v: run 2 (stale head) and run 3 (other head repository) must each be refused on their own, and run 1 not at all", got)
	}
}

// ---- gaps found by mutation ------------------------------------------------------------------

func TestADuplicatedShaInTheCommitListIsVerifiedAndAuditedOnce(t *testing.T) {
	p := newPRWorld(t, prSpec{})
	p.srv.UpdatePullRequest(repo, p.number, func(pr *githubtest.PullRequest) {
		var dup []provider.PullRequestCommit
		for _, c := range pr.Commits {
			dup = append(dup, c, c)
		}
		pr.Commits = dup
	})
	p.queuePR(1, 10)
	p.srv.UpdateJob(repo, 10, func(j *provider.Job) { j.Status = "waiting" }) // one assessment, no launch
	if out, err := p.sup.Tick(ctx); err != nil || !out.Idle {
		t.Fatalf("%+v, %v", out, err)
	}
	for _, c := range p.commits {
		if n := p.requestsContaining("/git/commits/" + c.SHA); n != 1 {
			t.Errorf("commit %s fetched %d times", short12(c.SHA), n)
		}
	}
	p.srv.UpdateJob(repo, 10, func(j *provider.Job) { j.Status = "queued" })
	mustLaunch(t, p)
	e := p.entriesOfKind(supervisor.KindLaunching)[0]
	if len(e.Verified) != 4 || e.VerifiedTotal != 4 { // base tip + 3 distinct commits
		t.Errorf("listed %d of %d: each distinct commit is verified and listed once", len(e.Verified), e.VerifiedTotal)
	}
}

func TestAfterTheFirstRefusalTheRestOfALongPullRequestIsNotFetched(t *testing.T) {
	authors := []string{"-"} // the oldest commit is unsigned
	for i := 0; i < 40; i++ {
		authors = append(authors, "alice")
	}
	p := newPRWorld(t, prSpec{authors: authors})
	p.queuePR(1, 10)
	mustRefuse(t, p, p.commits[0].SHA, "not signed")
	// merge commit + base tip are two of the fetches; four workers may have a commit in flight
	// each when the refusal lands, but 40 more must not be read for a job that is refused anyway.
	if n := len(p.commitFetches()) - 2; n > 2*4 {
		t.Errorf("%d pull request commits fetched after the first one was refused, want at most %d of 41", n, 2*4)
	}
}

func TestTheRefusalTextNamesTheLowestBadCommitEveryTime(t *testing.T) {
	// Commits 1 and 3 are unsigned, with workers racing over them: the audit log must not flap
	// between two texts for the same job.
	p := newPRWorld(t, prSpec{authors: []string{"alice", "-", "alice", "-", "alice", "alice", "alice", "alice"}})
	p.queuePR(1, 10)
	for i := 0; i < 40; i++ {
		out, err := p.sup.Tick(ctx)
		if err != nil || len(out.Refused) != 1 {
			t.Fatalf("tick %d: %+v, %v", i, out, err)
		}
		if r := out.Refused[0].Reason; !strings.Contains(r, p.commits[1].SHA) || strings.Contains(r, p.commits[3].SHA) {
			t.Fatalf("tick %d: reason %q must name commit 1 (%s), the lowest bad one", i, r, p.commits[1].SHA)
		}
	}
}

// ---- the watcher and a pull request it cannot judge: which side is unreadable ---------------

func TestAWaitingRunnerIsNotStoppedWhenTheBaseRepositoryItselfCannotBeRead(t *testing.T) {
	// A fork's pull request. The pull request cannot be read (base side) and the base repository
	// does not read either, while the fork does: that says nothing about the pull request.
	p := newPRWorld(t, prSpec{fork: true})
	p.queuePR(1, 10)
	stopped := false
	p.rt.OnRun = func(c context.Context, spec isolation.Spec) (isolation.Result, error) {
		p.srv.SetFailPath("/pulls/7")
		p.srv.SetRepoStatus(repo, 404)
		select {
		case <-c.Done():
			stopped = true
		case <-time.After(300 * time.Millisecond): // thirty polls
		}
		p.srv.SetFailPath("")
		p.srv.SetRepoStatus(repo, 0)
		return p.takes(repo, 10, 1)(c, spec)
	}
	out, err := p.sup.Tick(ctx)
	if err != nil || !out.Launched || stopped {
		t.Fatalf("stopped = %v, outcome = %+v, err = %v: an unreadable base repository must not kill an admitted runner", stopped, out, err)
	}
}

func TestAWaitingRunnerIsStoppedWhenTheBaseSideFailsAlthoughTheBaseReadsFine(t *testing.T) {
	// The pull request cannot be read for three polls while the base repository reads fine. The
	// fork is gone, but the failure is on the base side: it is the base that is asked.
	p := newPRWorld(t, prSpec{fork: true})
	p.queuePR(1, 10)
	stopped := false
	p.rt.OnRun = func(c context.Context, spec isolation.Spec) (isolation.Result, error) {
		p.srv.SetFailPath("/pulls/7")
		p.srv.SetRepoStatus(fork, 404)
		select {
		case <-c.Done():
			stopped = true
		case <-time.After(3 * time.Second):
		}
		return isolation.Result{}, nil
	}
	out, err := p.sup.Tick(ctx)
	if err != nil || !stopped || !out.Withheld {
		t.Fatalf("stopped = %v, outcome = %+v, err = %v", stopped, out, err)
	}
}

func TestACommitGitHubCouldNotVerifyRightNowIsWithheldNotRefusedAsUnsigned(t *testing.T) {
	// The commit is served with no signature and the reason of a verification outage (C13, the
	// reason name is unverified): not "unsigned", so not a refusal.
	p := newPRWorld(t, prSpec{})
	c := p.commits[1]
	p.srv.AddCommit(repo, c.SHA, c.Payload, "")
	p.srv.SetCommitReason(repo, c.SHA, "gpgverify_unavailable")
	p.queuePR(1, 10)
	mustWithhold(t, p, c.SHA)
	p.srv.AddCommit(repo, c.SHA, c.Payload, c.Signature)
	mustLaunch(t, p)
	// ... while a commit that is plainly unsigned is refused as before.
	q := newPRWorld(t, prSpec{authors: []string{"alice", "-", "alice"}})
	q.queuePR(1, 10)
	mustRefuse(t, q, q.commits[1].SHA, "not signed")
}
