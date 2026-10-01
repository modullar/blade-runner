package supervisor_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/isolation"
	"github.com/modullar/blade-runner/internal/provider"
	"github.com/modullar/blade-runner/internal/supervisor"
)

func TestAKeyRevokedWhileTheRunnerWaitsStopsTheRunner(t *testing.T) {
	r := newRig(t)
	r.queue(q{run: 1, job: 10}) // admitted when the runner starts
	stopped := false
	r.rt.OnRun = func(c context.Context, spec isolation.Spec) (isolation.Result, error) {
		// The owner revokes the key that vouched for this job while the runner is still waiting
		// for GitHub to hand it one.
		if _, err := r.trust.Revoke("owner"); err != nil {
			t.Error(err)
		}
		select {
		case <-c.Done():
			stopped = true
		case <-time.After(3 * time.Second):
		}
		return isolation.Result{}, nil
	}
	out, err := r.sup.Tick(ctx)
	if err != nil {
		t.Fatalf("a runner stopped before it had a job is not an error: %v", err)
	}
	if !stopped || !out.Withheld {
		t.Fatalf("stopped = %v, outcome = %+v: a job whose signer was revoked must not be left for the runner to take", stopped, out)
	}
	f := r.entriesOfKind(supervisor.KindFinished)
	if len(f) != 1 || f[0].Code != diag.CodeLaunchWithheld || !strings.Contains(f[0].Message, "no longer admitted") || !strings.Contains(f[0].Message, "job 10") {
		t.Errorf("finished = %+v", f)
	}
	if rs := r.srv.Runners(repo); len(rs) != 0 {
		t.Errorf("registration left behind: %+v", rs)
	}
	if len(r.entriesOfKind(supervisor.KindAlarm)) != 0 {
		t.Error("nothing was handed to the runner: this is not an alarm")
	}
}

func TestOnceTheRunnerHasItsJobARevokedKeyDoesNotKillIt(t *testing.T) {
	r := newRig(t)
	r.queue(q{run: 1, job: 10})
	stopped := false
	r.rt.OnRun = func(c context.Context, spec isolation.Spec) (isolation.Result, error) {
		r.srv.UpdateJob(repo, 10, func(j *provider.Job) { j.Status, j.RunnerName = "in_progress", spec.Name })
		_, _ = r.trust.Revoke("owner")
		select {
		case <-c.Done():
			stopped = true
		case <-time.After(150 * time.Millisecond): // many watcher ticks
		}
		r.srv.UpdateJob(repo, 10, func(j *provider.Job) { j.Status = "completed" })
		return isolation.Result{}, nil
	}
	if out, err := r.sup.Tick(ctx); err != nil || !out.Launched {
		t.Fatalf("%+v, %v", out, err)
	}
	if stopped {
		t.Error("a running job was killed because its signer was revoked after it started")
	}
}

func TestACommitTheWatcherCannotReJudgeEventuallyStopsAWaitingRunner(t *testing.T) {
	r := newRig(t)
	r.queue(q{run: 1, job: 10})
	stopped := false
	r.rt.OnRun = func(c context.Context, spec isolation.Spec) (isolation.Result, error) {
		r.srv.SetCommitStatus(r.commits["owner"].SHA, 503) // GitHub stops serving the commit
		select {
		case <-c.Done():
			stopped = true
		case <-time.After(3 * time.Second):
		}
		r.srv.SetCommitStatus(r.commits["owner"].SHA, 0)
		return isolation.Result{}, nil
	}
	if _, err := r.sup.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if !stopped {
		t.Fatal("a waiting runner whose job can no longer be judged must be stopped, like one whose queue cannot be read")
	}
	if f := r.entriesOfKind(supervisor.KindFinished); len(f) != 1 || f[0].Code != diag.CodeQueueUnreadable {
		t.Errorf("finished = %+v", f)
	}
}

func TestATransientFailureNeverKillsAnAlreadyAdmittedRunner(t *testing.T) {
	// While the runner waits, GitHub starts answering 404 for its job's commit AND for the
	// repository itself (a token that cannot see it, or a hiccup). Many polls go by. The runner
	// was admitted; "cannot tell" is not a reason to kill it.
	r := newRig(t)
	r.queue(q{run: 1, job: 10})
	sha := r.commits["owner"].SHA
	stopped := false
	r.rt.OnRun = func(c context.Context, spec isolation.Spec) (isolation.Result, error) {
		r.srv.SetCommitStatus(sha, 404)
		r.srv.SetRepoStatus(repo, 404)
		select {
		case <-c.Done():
			stopped = true
		case <-time.After(300 * time.Millisecond): // thirty polls at 10ms
		}
		r.srv.SetCommitStatus(sha, 0)
		r.srv.SetRepoStatus(repo, 0)
		return r.takes(repo, 10, 1)(c, spec)
	}
	out, err := r.sup.Tick(ctx)
	if err != nil || !out.Launched || out.Withheld {
		t.Fatalf("outcome = %+v, err = %v", out, err)
	}
	if stopped {
		t.Fatal("an admitted runner was stopped over failures that say nothing about its commit")
	}
	if f := r.entriesOfKind(supervisor.KindFinished); len(f) != 1 || len(f[0].JobsRun) != 1 {
		t.Errorf("finished = %+v", f)
	}
}

func TestACommitThatIsGoneFromAReadableRepositoryStopsAWaitingRunner(t *testing.T) {
	// The other side: the repository reads fine and GitHub has no such commit any more (force
	// pushed away): the job cannot be vouched for, so the runner that has no job yet is stopped.
	r := newRig(t)
	r.queue(q{run: 1, job: 10})
	stopped := false
	r.rt.OnRun = func(c context.Context, spec isolation.Spec) (isolation.Result, error) {
		r.srv.SetCommitStatus(r.commits["owner"].SHA, 404)
		select {
		case <-c.Done():
			stopped = true
		case <-time.After(3 * time.Second):
		}
		return isolation.Result{}, nil
	}
	out, err := r.sup.Tick(ctx)
	if err != nil || !stopped || !out.Withheld {
		t.Fatalf("stopped = %v, outcome = %+v, err = %v", stopped, out, err)
	}
}
