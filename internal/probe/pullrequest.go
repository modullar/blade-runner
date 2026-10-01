package probe

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/modullar/blade-runner/internal/provider"
)

// C10 to C12b check what admission of a pull_request run leans on (decision 0007, "Pull requests:
// every commit verified"). They need a pull request behind a recent pull_request run; without
// one they are SKIPPED with the reason, never passed: a check that had nothing to look at has
// proved nothing.

const (
	// prCandidates bounds how many pull requests are read to find an open one and a fork's.
	prCandidates = 5
	// mergeableTries is how often an open pull request's null "mergeable" is read again: GitHub
	// computes it lazily, after the first request that asks.
	mergeableTries = 3
)

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

var (
	idC10  = Finding{ID: "C10", Assumption: "the pull request endpoint gives state, base and head commits, mergeable (null until computed) and merge_commit_sha (the commit a pull_request job runs)"}
	idC11  = Finding{ID: "C11", Assumption: "the pull request's commit list holds every commit of an open pull request, head included, up to a cap of 250"}
	idC12  = Finding{ID: "C12", Assumption: "the merge commit has exactly two parents: parents[0] is the base tip (base.sha) and parents[1] is the pull request head"}
	idC12b = Finding{ID: "C12b", Assumption: "the commits of a pull request from a fork are readable through the base repository"}
)

func skipAll(reason string) []Finding {
	out := []Finding{idC10, idC11, idC12, idC12b}
	for i := range out {
		out[i].Status, out[i].Detail = Skip, reason
	}
	return out
}

type prSeen struct {
	info provider.PullRequestInfo
}

func (o Options) wait(ctx context.Context) {
	if o.Wait != nil {
		o.Wait(ctx)
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
	}
}

func checkPullRequests(ctx context.Context, p provider.Provider, o Options) []Finding {
	repo := o.Repository
	runs, err := p.ListRecentRuns(ctx, repo, "", 30)
	if err != nil {
		return skipAll("cannot list runs, so no pull request could be chosen: " + describe(err))
	}
	var numbers []int
	seen := map[int]bool{}
	prRuns, unnamed := 0, 0
	for _, r := range runs {
		if r.Event != "pull_request" {
			continue
		}
		prRuns++
		if len(r.PullRequests) == 0 {
			unnamed++
		}
		for _, pr := range r.PullRequests {
			if !seen[pr.Number] && len(numbers) < prCandidates {
				seen[pr.Number] = true
				numbers = append(numbers, pr.Number)
			}
		}
	}
	switch {
	case prRuns == 0:
		return skipAll("no recent run has the event pull_request: open a pull request from the br0-probe branch into main (docs/BR-0-runbook.md, step 3b), let its workflow start, and run this again")
	case len(numbers) == 0:
		return skipAll(fmt.Sprintf("%d pull_request run(s) exist but none names a pull request (pull_requests[] is empty, as GitHub leaves it for runs from forks): the supervisor refuses such a run, because it cannot tell which pull request, and so which merge commit, it is for; open a pull request from a branch of THIS repository and run this again", prRuns))
	}

	var infos []prSeen
	var firstErr string
	for _, n := range numbers {
		info, err := p.PullRequest(ctx, repo, n)
		if err != nil {
			if firstErr == "" {
				firstErr = fmt.Sprintf("pull request #%d: %s", n, describe(err))
			}
			continue
		}
		infos = append(infos, prSeen{info})
	}
	if len(infos) == 0 {
		f := idC10
		f.Status, f.Detail = Fail, "the pull request endpoint failed for every candidate: "+firstErr
		return append([]Finding{f}, skipAll("the pull request endpoint failed (see C10)")[1:]...)
	}

	var open *provider.PullRequestInfo
	var fork *provider.PullRequestInfo
	for i := range infos {
		in := &infos[i].info
		if open == nil && in.State == "open" {
			open = in
		}
		if fork == nil && in.HeadRepository != "" && !strings.EqualFold(in.HeadRepository, repo) {
			fork = in
		}
	}

	var out []Finding
	if open == nil {
		reason := fmt.Sprintf("the %d pull request(s) behind recent pull_request runs are all closed, and a closed one says nothing reliable about its merge commit: open one (docs/BR-0-runbook.md, step 3b), run this again, close it afterwards", len(infos))
		out = append(out, withStatus(idC10, Skip, reason), withStatus(idC11, Skip, reason), withStatus(idC12, Skip, reason))
	} else {
		c10, info := checkPRFields(ctx, p, o, *open)
		out = append(out, c10)
		if c10.Status == Fail || c10.Status == Skip {
			why := "C10 did not pass, so the merge commit and commit list of pull request #" + fmt.Sprint(open.Number) + " cannot be judged (see C10)"
			out = append(out, withStatus(idC11, Skip, why), withStatus(idC12, Skip, why))
		} else {
			out = append(out, checkPRCommitList(ctx, p, repo, info), checkMergeParents(ctx, p, repo, info))
		}
	}
	out = append(out, checkForkReadable(ctx, p, repo, fork, len(infos)))
	return out
}

func withStatus(f Finding, st Status, detail string) Finding {
	f.Status, f.Detail = st, detail
	return f
}

// checkPRFields reads the pull request again until "mergeable" is known (it is null until GitHub
// has computed it) and checks the fields that admission depends on.
func checkPRFields(ctx context.Context, p provider.Provider, o Options, info provider.PullRequestInfo) (Finding, provider.PullRequestInfo) {
	f := idC10
	for try := 1; info.Mergeable == nil && try < mergeableTries; try++ {
		o.wait(ctx)
		again, err := p.PullRequest(ctx, o.Repository, info.Number)
		if err != nil {
			f.Status, f.Detail = Fail, fmt.Sprintf("pull request #%d: %s", info.Number, describe(err))
			return f, info
		}
		info = again
	}
	pr := fmt.Sprintf("pull request #%d", info.Number)
	var missing []string
	if !fullSHA.MatchString(info.BaseSHA) {
		missing = append(missing, "base.sha")
	}
	if !fullSHA.MatchString(info.HeadSHA) {
		missing = append(missing, "head.sha")
	}
	if info.BaseRepository == "" {
		missing = append(missing, "base.repo.full_name")
	}
	if info.HeadRepository == "" {
		missing = append(missing, "head.repo.full_name")
	}
	if info.BaseRef == "" {
		missing = append(missing, "base.ref")
	}
	switch {
	case len(missing) > 0:
		f.Status, f.Detail = Fail, fmt.Sprintf("%s: missing or malformed: %s", pr, strings.Join(missing, ", "))
	case info.Mergeable == nil:
		f.Status, f.Detail = Skip, fmt.Sprintf("%s: mergeable was still null after %d reads, %ds apart: GitHub had not computed the merge. Wait a minute, open the pull request's page once, and run this again", pr, mergeableTries, 3)
	case !*info.Mergeable:
		f.Status, f.Detail = Skip, fmt.Sprintf("%s: mergeable is false (it has a conflict), so it has no merge commit to check: resolve the conflict or open another pull request", pr)
	case !fullSHA.MatchString(info.MergeCommitSHA):
		f.Status, f.Detail = Fail, fmt.Sprintf("%s: mergeable is true but merge_commit_sha is %q: the supervisor refuses such a pull request", pr, info.MergeCommitSHA)
	default:
		f.Status, f.Detail = Pass, fmt.Sprintf("%s (%s into %s:%s): state %s, mergeable true, merge_commit_sha %s, base.sha %s, head.sha %s, %d commit(s)",
			pr, info.HeadRepository, info.BaseRepository, info.BaseRef, info.State, short(info.MergeCommitSHA), short(info.BaseSHA), short(info.HeadSHA), info.Commits)
	}
	return f, info
}

func checkPRCommitList(ctx context.Context, p provider.Provider, repo string, info provider.PullRequestInfo) Finding {
	f := idC11
	pr := fmt.Sprintf("pull request #%d", info.Number)
	list, err := p.ListPullRequestCommits(ctx, repo, info.Number)
	if err != nil {
		f.Status, f.Detail = Fail, pr+": "+describe(err)
		return f
	}
	hasHead, bad := false, 0
	for _, c := range list {
		hasHead = hasHead || c.SHA == info.HeadSHA
		if !fullSHA.MatchString(c.SHA) {
			bad++
		}
	}
	switch {
	case bad > 0:
		f.Status, f.Detail = Fail, fmt.Sprintf("%s: %d commit id(s) in the list are not 40 hex digits", pr, bad)
	case len(list) >= provider.PullRequestCommitCap:
		f.Status, f.Detail = Skip, fmt.Sprintf("%s: the list holds %d commits, the cap: whether it is complete cannot be judged (the supervisor refuses such a pull request)", pr, len(list))
	case !hasHead:
		f.Status, f.Detail = Fail, fmt.Sprintf("%s: the head %s is not among the %d listed commits", pr, short(info.HeadSHA), len(list))
	case info.Commits != len(list):
		f.Status, f.Detail = Fail, fmt.Sprintf("%s: the pull request says it has %d commits but the list holds %d", pr, info.Commits, len(list))
	default:
		f.Status, f.Detail = Pass, fmt.Sprintf("%s: %d commit(s) listed, the head among them, matching the pull request's own count", pr, len(list))
	}
	return f
}

func checkMergeParents(ctx context.Context, p provider.Provider, repo string, info provider.PullRequestInfo) Finding {
	f := idC12
	pr := fmt.Sprintf("pull request #%d", info.Number)
	c, err := p.Commit(ctx, repo, info.MergeCommitSHA)
	if err != nil {
		f.Status, f.Detail = Fail, fmt.Sprintf("%s: cannot fetch its merge commit %s: %s", pr, short(info.MergeCommitSHA), describe(err))
		return f
	}
	switch {
	case len(c.Parents) != 2:
		f.Status, f.Detail = Fail, fmt.Sprintf("%s: the merge commit %s has %d parent(s), not 2: %v", pr, short(c.SHA), len(c.Parents), shorts(c.Parents))
	case c.Parents[0] != info.BaseSHA || c.Parents[1] != info.HeadSHA:
		f.Status, f.Detail = Fail, fmt.Sprintf("%s: the merge commit %s has parents %v, expected [base.sha %s, head.sha %s]: the order or the commits differ from the assumption",
			pr, short(c.SHA), shorts(c.Parents), short(info.BaseSHA), short(info.HeadSHA))
	default:
		f.Status, f.Detail = Pass, fmt.Sprintf("%s: merge commit %s has parents [%s (base tip), %s (head)], in that order", pr, short(c.SHA), short(c.Parents[0]), short(c.Parents[1]))
	}
	return f
}

func checkForkReadable(ctx context.Context, p provider.Provider, repo string, fork *provider.PullRequestInfo, read int) Finding {
	f := idC12b
	if fork == nil {
		f.Status, f.Detail = Skip, fmt.Sprintf("none of the %d pull request(s) read comes from a fork: a pull request from another repository (a second account's fork) is needed; a same-repository one proves nothing about this", read)
		return f
	}
	pr := fmt.Sprintf("pull request #%d (head in %s)", fork.Number, fork.HeadRepository)
	list, err := p.ListPullRequestCommits(ctx, repo, fork.Number)
	if err != nil {
		f.Status, f.Detail = Fail, pr+": cannot list its commits: "+describe(err)
		return f
	}
	if len(list) == 0 {
		f.Status, f.Detail = Fail, pr+": GitHub lists no commits for it"
		return f
	}
	const sample = 5
	checked, unreadable := 0, 0
	var first string
	for i, c := range list {
		if i >= sample && c.SHA != fork.HeadSHA {
			continue
		}
		checked++
		if _, err := p.Commit(ctx, repo, c.SHA); err != nil {
			unreadable++
			if first == "" {
				first = fmt.Sprintf("%s: %s", short(c.SHA), describe(err))
			}
		}
	}
	if unreadable > 0 {
		f.Status, f.Detail = Fail, fmt.Sprintf("%s: %d of %d examined commit(s) are not readable through %s (%s): the supervisor then asks the fork, and withholds the job if neither serves it", pr, unreadable, checked, repo, first)
		return f
	}
	f.Status, f.Detail = Pass, fmt.Sprintf("%s: %d of %d commit(s) examined (the head among them) were all readable through %s", pr, checked, len(list), repo)
	return f
}

func shorts(shas []string) []string {
	out := make([]string, len(shas))
	for i, s := range shas {
		out[i] = short(s)
	}
	return out
}
