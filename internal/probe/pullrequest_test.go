package probe_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/modullar/blade-runner/internal/probe"
	"github.com/modullar/blade-runner/internal/provider"
	"github.com/modullar/blade-runner/internal/provider/github/githubtest"
	"github.com/modullar/blade-runner/internal/testrig"
)

func sha(c byte) string { return strings.Repeat(string(c), 40) }

// prWorld is a repository with a pull request that behaves the way the supervisor assumes:
// open, mergeable, a merge commit of [base tip, head], and the pull request's commits readable.
type prSetup struct {
	r      *testrig.Rig
	number int
}

func addPR(t *testing.T, r *testrig.Rig, number int, headRepo string, mutate func(*githubtest.PullRequest)) prSetup {
	t.Helper()
	yes := true
	base, head, merge, mid := sha('a'), sha('b'), sha('c'), sha('d')
	pr := githubtest.PullRequest{
		Info: provider.PullRequestInfo{Number: number, State: "open", BaseRepository: testrig.Target, BaseRef: "main", BaseSHA: base,
			HeadRepository: headRepo, HeadSHA: head, MergeCommitSHA: merge, Mergeable: &yes},
		Commits: []provider.PullRequestCommit{{SHA: mid, Author: "alice"}, {SHA: head, Author: "alice"}},
	}
	if mutate != nil {
		mutate(&pr)
	}
	r.Srv.SetPullRequest(testrig.Target, pr)
	r.Srv.AddCommit(testrig.Target, merge, fmt.Sprintf("tree x\nparent %s\nparent %s\n\nmerge\n", base, head), "")
	for _, c := range pr.Commits {
		r.Srv.AddCommit(testrig.Target, c.SHA, "tree y\n\nc\n", "")
	}
	r.Srv.AddRun(testrig.Target, provider.Run{ID: int64(100 + number), HeadSHA: head, Event: "pull_request", Status: "completed", HeadRepository: headRepo, Actor: "alice",
		PullRequests: []provider.PullRequest{{Number: number, HeadSHA: head, HeadRepository: headRepo}}})
	return prSetup{r, number}
}

func prFindings(t *testing.T, r *testrig.Rig) []probe.Finding {
	t.Helper()
	return probe.Run(ctx, r.Env.Provider, probe.Options{Repository: testrig.Target, SkipJIT: true, Wait: func(_ context.Context) {}})
}

func TestWithoutAPullRequestToLookAtEveryPullRequestCheckIsSkippedNeverPassed(t *testing.T) {
	for name, build := range map[string]func(*testrig.Rig){
		"no runs at all": func(*testrig.Rig) {},
		"only push runs": func(r *testrig.Rig) {
			r.Srv.AddRun(testrig.Target, provider.Run{ID: 1, HeadSHA: sha('a'), Event: "push", Status: "completed", HeadRepository: testrig.Target, Actor: "acme"})
		},
		"pull_request runs that name no pull request (a fork's)": func(r *testrig.Rig) {
			r.Srv.AddRun(testrig.Target, provider.Run{ID: 1, HeadSHA: sha('a'), Event: "pull_request", Status: "completed", HeadRepository: "mallory/widgets", Actor: "mallory"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := testrig.New(t, testrig.BaseConfig)
			build(r)
			fs := prFindings(t, r)
			for _, id := range []string{"C10", "C11", "C12", "C12b"} {
				f := find(fs, id)
				if f.Status != probe.Skip || f.Detail == "" {
					t.Errorf("%s = %s %q, want SKIP with a reason", id, f.Status, f.Detail)
				}
			}
			if probe.Failed(fs) {
				t.Errorf("having nothing to look at is not a failure: %s", probe.Render(fs))
			}
		})
	}
	// The reasons differ and say what to do.
	r := testrig.New(t, testrig.BaseConfig)
	want(t, prFindings(t, r), "C10", probe.Skip, "br0-probe branch")
	r.Srv.AddRun(testrig.Target, provider.Run{ID: 1, HeadSHA: sha('a'), Event: "pull_request", Status: "completed", HeadRepository: "mallory/widgets"})
	want(t, prFindings(t, r), "C10", probe.Skip, "names a pull request")
}

func TestAnOpenSameRepositoryPullRequestPassesC10ToC12AndLeavesTheForkCheckSkipped(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	addPR(t, r, 7, testrig.Target, nil)
	fs := prFindings(t, r)
	want(t, fs, "C10", probe.Pass, "mergeable true")
	want(t, fs, "C10", probe.Pass, "merge_commit_sha cccccccccc")
	want(t, fs, "C11", probe.Pass, "2 commit(s) listed")
	want(t, fs, "C12", probe.Pass, "[aaaaaaaaaa (base tip), bbbbbbbbbb (head)]")
	want(t, fs, "C12b", probe.Skip, "fork")
	if probe.Failed(fs) {
		t.Error(probe.Render(fs))
	}
}

// signAsServed re-serves the pull request's commits with a signature and its payload, as GitHub
// does for a signed commit (the bytes are not a real signature: C12b only asks that they arrive).
func signAsServed(r *testrig.Rig, shas ...string) {
	for _, s := range shas {
		r.Srv.AddCommit(testrig.Target, s, "tree y\n\nc\n", "-----BEGIN SSH SIGNATURE-----\nx\n-----END SSH SIGNATURE-----")
	}
}

func TestAForkPullRequestWhoseCommitsTheBaseRepositoryServesPassesC12b(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	addPR(t, r, 8, "mallory/widgets", nil)
	signAsServed(r, sha('d'), sha('b'))
	want(t, prFindings(t, r), "C12b", probe.Pass, "readable through acme/widgets")
	want(t, prFindings(t, r), "C12b", probe.Pass, "both signature and signed payload")
}

func TestAForkCommitThatArrivesUnsignedProvesNothingAboutTheSignatureAndIsSkipped(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	addPR(t, r, 8, "mallory/widgets", nil) // served readable, but with no signature and no payload
	want(t, prFindings(t, r), "C12b", probe.Skip, "none carries a signature and payload")
}

func TestAForkCommitWhoseSignatureComesWithoutItsPayloadFailsC12b(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	addPR(t, r, 8, "mallory/widgets", nil)
	signAsServed(r, sha('b'))
	r.Srv.AddCommit(testrig.Target, sha('d'), "", "-----BEGIN SSH SIGNATURE-----\nx\n-----END SSH SIGNATURE-----")
	want(t, prFindings(t, r), "C12b", probe.Fail, "signature but no payload")
}

func TestC10DoesNotClaimThatTheMergeCommitIsWhatAJobRuns(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	addPR(t, r, 7, testrig.Target, nil)
	f := find(prFindings(t, r), "C10")
	if f.Status != probe.Pass {
		t.Fatalf("C10 = %s %q", f.Status, f.Detail)
	}
	if strings.Contains(f.Assumption, "the commit a pull_request job runs") {
		t.Errorf("the assumption still claims what a job runs: %q", f.Assumption)
	}
	if !strings.Contains(f.Detail, "NOT observed") || !strings.Contains(f.Detail, "GITHUB_SHA") {
		t.Errorf("C10 passes on what was observed and must say what was not: %q", f.Detail)
	}
}

func TestSkippedPullRequestChecksAreNamed(t *testing.T) {
	fs := []probe.Finding{{ID: "C1", Status: probe.Skip}, {ID: "C10", Status: probe.Skip}, {ID: "C11", Status: probe.Pass}, {ID: "C12b", Status: probe.Skip}}
	if got := strings.Join(probe.SkippedPullRequestChecks(fs), ","); got != "C10,C12b" {
		t.Errorf("skipped = %q: C1 is not a pull request check and C11 passed", got)
	}
	if got := probe.SkippedPullRequestChecks([]probe.Finding{{ID: "C10", Status: probe.Pass}}); len(got) != 0 {
		t.Errorf("skipped = %v", got)
	}
}

func TestAForkPullRequestWhoseCommitsTheBaseRepositoryCannotServeFailsC12b(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	addPR(t, r, 8, "mallory/widgets", nil)
	r.Srv.SetCommitStatus(sha('d'), 404)
	fs := prFindings(t, r)
	want(t, fs, "C12b", probe.Fail, "not readable through acme/widgets")
	if !probe.Failed(fs) {
		t.Error("Failed() must be true")
	}
}

func TestAMergeCommitWithTheParentsTheOtherWayRoundFailsC12(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	addPR(t, r, 7, testrig.Target, nil)
	r.Srv.SetCommitParents(testrig.Target, sha('c'), []string{sha('b'), sha('a')})
	want(t, prFindings(t, r), "C12", probe.Fail, "expected [base.sha")
	r.Srv.SetCommitParents(testrig.Target, sha('c'), []string{sha('a')})
	want(t, prFindings(t, r), "C12", probe.Fail, "1 parent(s)")
}

func TestAMergeableThatNeverBecomesKnownIsSkippedAfterRereading(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	addPR(t, r, 7, testrig.Target, func(pr *githubtest.PullRequest) { pr.Info.Mergeable = nil })
	waits := 0
	fs := probe.Run(ctx, r.Env.Provider, probe.Options{Repository: testrig.Target, SkipJIT: true, Wait: func(_ context.Context) { waits++ }})
	want(t, fs, "C10", probe.Skip, "still null")
	want(t, fs, "C11", probe.Skip, "C10 did not pass")
	want(t, fs, "C12", probe.Skip, "C10 did not pass")
	if waits != 2 {
		t.Errorf("waited %d times, want 2 (three reads)", waits)
	}
	reads := 0
	for _, req := range r.Srv.Requests() {
		if req == "GET /repos/acme/widgets/pulls/7" {
			reads++
		}
	}
	// Two probe passes happened above (one from want's argument evaluation is not one: Run once).
	if reads != 1+2 {
		t.Errorf("pull request reads = %d, want 3 (one, then two re-reads)", reads)
	}
}

func TestC10FailsWhenAMergeableCommitHasNoMergeCommitId(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	addPR(t, r, 7, testrig.Target, func(pr *githubtest.PullRequest) { pr.Info.MergeCommitSHA = "" })
	want(t, prFindings(t, r), "C10", probe.Fail, "merge_commit_sha")
}

func TestAClosedPullRequestIsNotTrustedForTheMergeChecks(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	addPR(t, r, 7, testrig.Target, func(pr *githubtest.PullRequest) { pr.Info.State = "closed" })
	fs := prFindings(t, r)
	want(t, fs, "C10", probe.Skip, "all closed")
	want(t, fs, "C12", probe.Skip, "all closed")
}

func TestC11FailsWhenTheListMissesTheHeadOrDisagreesWithTheCount(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	addPR(t, r, 7, testrig.Target, func(pr *githubtest.PullRequest) { pr.Commits = pr.Commits[:1] }) // only the first commit
	want(t, prFindings(t, r), "C11", probe.Fail, "not among")

	r2 := testrig.New(t, testrig.BaseConfig)
	addPR(t, r2, 7, testrig.Target, func(pr *githubtest.PullRequest) { pr.ListLimit = 1 })
	want(t, prFindings(t, r2), "C11", probe.Fail, "not among")
}

func TestC11IsSkippedWhenTheListIsAtGitHubsCap(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	addPR(t, r, 7, testrig.Target, func(pr *githubtest.PullRequest) {
		var l []provider.PullRequestCommit
		for i := 0; i < 299; i++ {
			l = append(l, provider.PullRequestCommit{SHA: fmt.Sprintf("%040x", i+1)})
		}
		pr.Commits = append(l, provider.PullRequestCommit{SHA: sha('b')})
	})
	want(t, prFindings(t, r), "C11", probe.Skip, "cap")
}

func TestThePullRequestChecksOnlyRead(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	addPR(t, r, 7, testrig.Target, nil)
	prFindings(t, r)
	for _, req := range r.Srv.Requests() {
		if !strings.HasPrefix(req, "GET ") {
			t.Errorf("a changing request: %s", req)
		}
	}
}
