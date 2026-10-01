package supervisor

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/modullar/blade-runner/internal/admit"
	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/trust"
)

// allowedEvents are the triggers whose commit is the code that runs. For the others the code is
// not what was verified, or a stranger chooses the moment: pull_request_target and workflow_run
// run the base branch's workflow with a stranger's input, issue_comment runs on the default
// branch whatever a commenter typed, and so on. They are refused, not guessed at.
var allowedEvents = map[string]bool{
	"push":              true,
	"pull_request":      true,
	"workflow_dispatch": true,
	"schedule":          true,
}

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

type outcomeKind int

const (
	admitted  outcomeKind = iota // the commit is signed by a trusted key
	refused                      // definitively not allowed to run here
	transient                    // could not be judged (provider error): treated as "not now"
)

// judgement is the decision about one waiting job.
type judgement struct {
	Obs     observed
	Kind    outcomeKind
	Code    string // diag code: BR-E072/073/074 (refused) or the error's own (transient)
	Reason  string
	Subject admit.Subject
	Verdict trust.Verdict // set when admitted
	// Merge and Verified are set for an admitted pull request: the merge commit the job runs, and
	// every commit that was verified for it. A runner is only kept while Merge stays the same.
	Merge    string
	Verified []VerifiedCommit
}

type admission struct {
	verdict trust.Verdict
	err     error
}

// judger judges jobs within one assessment, fetching each commit at most once.
type judger struct {
	s     *Supervisor
	mu    sync.Mutex // the commits of a pull request are verified concurrently
	cache map[string]admission
	prs   map[string]prVerdict
}

func what(err error) string {
	var de *diag.Error
	if errors.As(err, &de) {
		return de.What
	}
	return err.Error()
}

func refuse(o observed, code, reason string) judgement {
	return judgement{Obs: o, Kind: refused, Code: code, Reason: reason}
}

// judge decides one job. Order matters: first that the description is complete and consistent
// (a job that cannot be described cannot be vouched for), then that the event is one whose
// commit is the code, then the cryptographic check on the commit the run is for.
func (j *judger) judge(ctx context.Context, o observed) judgement {
	run, job := o.Run, o.Job
	switch {
	case run.ID == 0 || job.ID == 0:
		return refuse(o, diag.CodeJobAmbiguous, "the job or its run has no id")
	case job.RunID != run.ID:
		return refuse(o, diag.CodeJobAmbiguous, fmt.Sprintf("job %d claims run %d but was listed under run %d", job.ID, job.RunID, run.ID))
	case job.Status == "":
		return refuse(o, diag.CodeJobAmbiguous, "the job has no status")
	case len(job.Labels) == 0:
		return refuse(o, diag.CodeJobAmbiguous, "the job names no labels, so what may run it is unknown")
	case run.Event == "":
		return refuse(o, diag.CodeJobAmbiguous, "the run has no event")
	case !fullSHA.MatchString(run.HeadSHA):
		return refuse(o, diag.CodeJobAmbiguous, fmt.Sprintf("the run's head commit %q is not a full 40-digit commit id", run.HeadSHA))
	case run.HeadRepository == "":
		return refuse(o, diag.CodeJobAmbiguous, "the run does not say which repository its commit lives in")
	}
	if job.HeadSHA != "" && job.HeadSHA != run.HeadSHA {
		return refuse(o, diag.CodeJobAmbiguous, fmt.Sprintf("the job's commit %s differs from its run's commit %s", short(job.HeadSHA), short(run.HeadSHA)))
	}
	for _, pr := range run.PullRequests {
		if pr.HeadSHA != run.HeadSHA {
			return refuse(o, diag.CodeJobAmbiguous, fmt.Sprintf("pull request #%d has head commit %s but the run is for %s: the commit to verify is unclear", pr.Number, short(pr.HeadSHA), short(run.HeadSHA)))
		}
		if pr.HeadRepository != "" && !strings.EqualFold(pr.HeadRepository, run.HeadRepository) {
			return refuse(o, diag.CodeJobAmbiguous, fmt.Sprintf("pull request #%d is from %s but the run says %s", pr.Number, pr.HeadRepository, run.HeadRepository))
		}
	}
	if !allowedEvents[run.Event] {
		return refuse(o, diag.CodeJobEventRefused, fmt.Sprintf("the event %q is not one whose commit is the code that runs", run.Event))
	}
	if run.Event == "pull_request" {
		// What runs is GitHub's merge of the head into the base: every commit that can reach the
		// job is verified (pullrequest.go, decision 0007 "Pull requests: every commit verified").
		return j.judgePullRequest(ctx, o)
	}
	// The commit must live in this repository: a push-like run whose commit is somewhere else
	// has no reason to exist and nothing vouches for it.
	if !strings.EqualFold(run.HeadRepository, j.s.cfg.Scope.Repository) {
		return refuse(o, diag.CodeJobAmbiguous, fmt.Sprintf("a %s run's commit lives in %s, not in %s", run.Event, run.HeadRepository, j.s.cfg.Scope.Repository))
	}

	subj := admit.Subject{Repository: run.HeadRepository, SHA: run.HeadSHA}
	a := j.admission(ctx, subj)
	switch {
	case a.err == nil:
		return judgement{Obs: o, Kind: admitted, Subject: subj, Verdict: a.verdict}
	case diag.CodeOf(a.err) == diag.CodeNotAdmitted:
		// A real "no", told by its code and nothing else: the commit was read and failed a check,
		// or GitHub says it does not exist (404/422), which no retry will change.
		return judgement{Obs: o, Kind: refused, Code: diag.CodeJobRefused, Reason: what(a.err), Subject: subj}
	default:
		// The commit could not be read at all (provider error), or the answer is not a verdict.
		// That is "not now", not "never": nothing starts, nothing is recorded as a refusal.
		return judgement{Obs: o, Kind: transient, Code: diag.CodeQueueUnreadable, Reason: what(a.err), Subject: subj}
	}
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
