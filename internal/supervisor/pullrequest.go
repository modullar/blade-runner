package supervisor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/modullar/blade-runner/internal/admit"
	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/provider"
	"github.com/modullar/blade-runner/internal/trust"
)

// Pull requests: every commit verified (decision 0007).
//
// A pull_request job runs GITHUB_SHA, GitHub's synthetic merge of the pull request head into the
// base branch, and the workflow file comes from that merge. The merge commit is signed by nobody,
// so it cannot be verified itself. What can be verified is what it is made of: it is admitted
// only when it has exactly the two parents "the base tip" and "the pull request head" and when
// the base tip and EVERY commit of the pull request are signed by a key in the trust store. The
// tree of the merge is then the merge of two verified trees.

// prWorkers bounds how many commits of one pull request are fetched and verified at once.
const prWorkers = 4

// maxAuditedCommits is how many verified commits a launch record lists; the rest are counted.
const maxAuditedCommits = 50

// prVerdict is the outcome of verifying one pull request run.
type prVerdict struct {
	kind     outcomeKind
	code     string
	reason   string
	subject  admit.Subject // the head commit, in the head repository
	verdict  trust.Verdict // the head commit's
	merge    string
	verified []VerifiedCommit
}

func (v prVerdict) judgement(o observed) judgement {
	return judgement{Obs: o, Kind: v.kind, Code: v.code, Reason: v.reason, Subject: v.subject, Verdict: v.verdict, Merge: v.merge, Verified: v.verified}
}

func prRefused(subj admit.Subject, format string, args ...any) prVerdict {
	return prVerdict{kind: refused, code: diag.CodeJobRefused, reason: fmt.Sprintf(format, args...), subject: subj}
}

func prTransient(subj admit.Subject, format string, args ...any) prVerdict {
	return prVerdict{kind: transient, code: diag.CodeQueueUnreadable, reason: fmt.Sprintf(format, args...), subject: subj}
}

// judgePullRequest judges a pull_request run. The result is remembered for the rest of this
// assessment, so a run with many jobs reads the pull request once.
func (j *judger) judgePullRequest(ctx context.Context, o observed) judgement {
	run := o.Run
	subj := admit.Subject{Repository: run.HeadRepository, SHA: run.HeadSHA}
	if len(run.PullRequests) != 1 {
		return refuseAt(o, subj, fmt.Sprintf("a pull_request run must name exactly one pull request, and this one names %d: which pull request, and so which merge commit, is unclear", len(run.PullRequests)))
	}
	number := run.PullRequests[0].Number
	key := fmt.Sprintf("%d@%s", number, run.HeadSHA)
	j.mu.Lock()
	if j.prs == nil {
		j.prs = map[string]prVerdict{}
	}
	v, ok := j.prs[key]
	j.mu.Unlock()
	if !ok {
		v = j.verifyPullRequest(ctx, run, number)
		j.mu.Lock()
		j.prs[key] = v
		j.mu.Unlock()
	}
	return v.judgement(o)
}

func refuseAt(o observed, subj admit.Subject, reason string) judgement {
	jd := refuse(o, diag.CodeJobRefused, reason)
	jd.Subject = subj
	return jd
}

func (j *judger) verifyPullRequest(ctx context.Context, run provider.Run, number int) prVerdict {
	base := j.s.cfg.Scope.Repository
	prov := j.s.cfg.Provider
	subj := admit.Subject{Repository: run.HeadRepository, SHA: run.HeadSHA}
	pr := fmt.Sprintf("pull request #%d", number)

	// 1. The pull request itself: open, still at the head the run is for, mergeable.
	info, err := prov.PullRequest(ctx, base, number)
	if err != nil {
		return prTransient(subj, "cannot read %s: %s", pr, what(err))
	}
	switch {
	case !strings.EqualFold(info.BaseRepository, base):
		return prRefused(subj, "%s is against %q, not against %s", pr, info.BaseRepository, base)
	case info.State != "open":
		return prRefused(subj, "%s is %q, not open: its merge commit is no longer maintained", pr, info.State)
	case info.HeadSHA != run.HeadSHA:
		return prRefused(subj, "%s is now at head %s but the run is for %s: the run is stale", pr, short(info.HeadSHA), short(run.HeadSHA))
	case info.HeadRepository == "":
		return prRefused(subj, "%s no longer says which repository its head lives in", pr)
	case !strings.EqualFold(info.HeadRepository, run.HeadRepository):
		return prRefused(subj, "%s says its head lives in %s but the run says %s", pr, info.HeadRepository, run.HeadRepository)
	case !fullSHA.MatchString(info.BaseSHA):
		return prRefused(subj, "%s has no usable base commit id (%q)", pr, info.BaseSHA)
	case info.Mergeable == nil:
		return prTransient(subj, "GitHub has not yet computed whether %s can be merged (mergeable is null)", pr)
	case !*info.Mergeable:
		return prRefused(subj, "%s cannot be merged cleanly into %s, so it has no merge commit to run", pr, info.BaseRef)
	case !fullSHA.MatchString(info.MergeCommitSHA):
		return prRefused(subj, "%s is mergeable but has no usable merge commit id (%q)", pr, info.MergeCommitSHA)
	}

	// 2. The merge commit, fetched from the base repository: exactly two parents, the base tip
	// first and the pull request head second. This is what binds "what runs" to "what is verified".
	merge, err := prov.Commit(ctx, base, info.MergeCommitSHA)
	if err != nil {
		return prTransient(subj, "cannot fetch the merge commit %s of %s: %s", info.MergeCommitSHA, pr, what(err))
	}
	switch {
	case len(merge.Parents) != 2:
		return prRefused(subj, "the merge commit %s of %s has %d parents, not exactly two (the base tip and the head)", info.MergeCommitSHA, pr, len(merge.Parents))
	case merge.Parents[0] != info.BaseSHA:
		return prRefused(subj, "the first parent of the merge commit %s of %s is %s, not the base tip %s: the merge is not of the branch it claims", info.MergeCommitSHA, pr, merge.Parents[0], info.BaseSHA)
	case merge.Parents[1] != info.HeadSHA:
		return prRefused(subj, "the second parent of the merge commit %s of %s is %s, not the pull request head %s: the merge is not of the code that is verified", info.MergeCommitSHA, pr, merge.Parents[1], info.HeadSHA)
	}

	// 3. The base tip. History before it is not individually verified: the signed tip commits to
	// its whole parent chain by hash (decision 0007).
	var verified []VerifiedCommit
	tip := j.admitFrom(ctx, []string{base}, info.BaseSHA)
	switch tip.class {
	case classRefused:
		return prRefused(subj, "%s: the tip of the base branch %s, commit %s, is not admitted: %s", pr, info.BaseRef, info.BaseSHA, what(tip.err))
	case classUnavailable:
		return prTransient(subj, "%s: the tip of the base branch %s, commit %s, cannot be fetched: %s", pr, info.BaseRef, info.BaseSHA, what(tip.err))
	}
	verified = append(verified, tip.verified("base-tip", info.BaseSHA))

	// 4. Every commit of the pull request. A list that GitHub cut short is never verified as if it
	// were whole.
	list, err := prov.ListPullRequestCommits(ctx, base, number)
	if err != nil {
		return prTransient(subj, "cannot list the commits of %s: %s", pr, what(err))
	}
	if len(list) >= provider.PullRequestCommitCap || info.Commits > len(list) {
		return prRefused(subj, "%s has too many commits to verify: GitHub lists at most %d of them, this list holds %d and the pull request has %d, so a subset would be all that could be checked",
			pr, provider.PullRequestCommitCap, len(list), info.Commits)
	}
	seen := map[string]bool{}
	var commits []provider.PullRequestCommit
	hasHead := false
	for _, c := range list {
		if !fullSHA.MatchString(c.SHA) {
			return prRefused(subj, "%s lists a commit with the unusable id %q", pr, c.SHA)
		}
		if seen[c.SHA] {
			continue
		}
		seen[c.SHA] = true
		hasHead = hasHead || c.SHA == info.HeadSHA
		commits = append(commits, c)
	}
	if !hasHead {
		return prRefused(subj, "the head commit %s is not among the %d commits GitHub lists for %s", info.HeadSHA, len(commits), pr)
	}
	repos := []string{base}
	if !strings.EqualFold(run.HeadRepository, base) {
		repos = append(repos, run.HeadRepository) // a fork's commits: the base repository is asked first
	}
	results := j.admitAll(ctx, repos, commits)
	// A definite refusal outranks a commit that could not be fetched: it is the answer whatever
	// the others say. Otherwise anything unfetched withholds the job.
	for i, r := range results {
		if r.class == classRefused {
			return prRefused(subj, "%s, commit %s (%d of %d, author %q as GitHub reports it): %s", pr, commits[i].SHA, i+1, len(commits), commits[i].Author, what(r.err))
		}
	}
	for i, r := range results {
		if r.class != classAdmitted {
			return prTransient(subj, "%s, commit %s (%d of %d) cannot be fetched from %s: %s", pr, commits[i].SHA, i+1, len(commits), strings.Join(repos, " or "), what(r.err))
		}
	}
	out := prVerdict{kind: admitted, subject: subj, merge: info.MergeCommitSHA}
	for i, r := range results {
		verified = append(verified, r.verified("pr-commit", commits[i].SHA))
		if commits[i].SHA == info.HeadSHA {
			out.verdict = r.verdict
		}
	}
	out.verified = verified
	return out
}

type admissionClass int

const (
	classAdmitted    admissionClass = iota
	classRefused                    // fetched and failed a check: a definite no
	classUnavailable                // could not be fetched from any repository asked: "not now"
	classSkipped                    // not tried, because another commit had already been refused
)

type commitResult struct {
	class   admissionClass
	verdict trust.Verdict
	err     error
	repo    string
}

func (r commitResult) verified(role, sha string) VerifiedCommit {
	return VerifiedCommit{Role: role, Repository: r.repo, SHA: sha, Signer: r.verdict.Signer.Name, Fingerprint: r.verdict.Signer.Fingerprint}
}

// admitFrom admits one commit, asking each repository in turn for it. A verification failure
// from a repository that served the commit is final (refused). A repository that could not serve
// it (not there, or unreachable) is skipped; if none can, the commit is unavailable, which is
// never a refusal.
func (j *judger) admitFrom(ctx context.Context, repos []string, sha string) commitResult {
	var last error
	for _, repo := range repos {
		a := j.admission(ctx, admit.Subject{Repository: repo, SHA: sha})
		switch {
		case a.err == nil:
			return commitResult{class: classAdmitted, verdict: a.verdict, repo: repo}
		case diag.CodeOf(a.err) == diag.CodeNotAdmitted && !errors.Is(a.err, provider.ErrNoSuchCommit):
			return commitResult{class: classRefused, err: a.err, repo: repo}
		default:
			last = a.err
		}
	}
	return commitResult{class: classUnavailable, err: last}
}

// admitAll admits every commit with at most prWorkers at a time. After the first refusal the
// commits not yet started are skipped: the job is refused whatever they hold, and an API call
// saved is one less against the rate limit at every poll.
func (j *judger) admitAll(ctx context.Context, repos []string, commits []provider.PullRequestCommit) []commitResult {
	results := make([]commitResult, len(commits))
	var next atomic.Int64
	var stop atomic.Bool
	var wg sync.WaitGroup
	for w := 0; w < min(prWorkers, len(commits)); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= len(commits) {
					return
				}
				if stop.Load() {
					results[i] = commitResult{class: classSkipped}
					continue
				}
				results[i] = j.admitFrom(ctx, repos, commits[i].SHA)
				if results[i].class == classRefused {
					stop.Store(true)
				}
			}
		}()
	}
	wg.Wait()
	return results
}

// admission is Admit through the per-assessment cache (safe for concurrent use).
func (j *judger) admission(ctx context.Context, subj admit.Subject) admission {
	key := strings.ToLower(subj.Repository) + "@" + subj.SHA
	j.mu.Lock()
	a, ok := j.cache[key]
	j.mu.Unlock()
	if ok {
		return a
	}
	v, err := j.s.cfg.Admitter.Admit(ctx, subj)
	a = admission{verdict: v, err: err}
	j.mu.Lock()
	j.cache[key] = a
	j.mu.Unlock()
	return a
}

// auditedCommits bounds the commits a launch record lists and says how many there were.
func auditedCommits(v []VerifiedCommit) (listed []VerifiedCommit, total int) {
	if len(v) > maxAuditedCommits {
		return v[:maxAuditedCommits], len(v)
	}
	return v, len(v)
}
