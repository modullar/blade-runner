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

// The key risk (decision 0007): a just-in-time runner takes ANY waiting job whose labels match,
// not the job it was started for. These tests are the mitigation, layer by layer. They do not
// prove the race is closed (only GitHub can): the last test group states what is left.

// stranger queues a pull request from mallory's fork: signed by a key nobody trusted.
func (r *rig) stranger(run, job int64) {
	r.t.Helper()
	r.queue(q{run: run, job: job, event: "pull_request", who: "mallory", headRepo: fork})
}

func TestNoRunnerStartsWhileAnUnadmittedJobCouldBeTakenByIt(t *testing.T) {
	r := newRig(t)
	r.queue(q{run: 1, job: 10}) // the owner's: admitted
	r.stranger(2, 20)           // a stranger's: same labels, so the owner's runner could take it
	r.rt.OnRun = r.takes(repo, 10, 1)

	for i := 0; i < 3; i++ {
		out, err := r.sup.Tick(ctx)
		if err != nil || !out.Withheld || out.Launched || len(out.Refused) != 1 || out.Refused[0].JobID != 20 {
			t.Fatalf("cycle %d: outcome = %+v, err = %v", i, out, err)
		}
	}
	r.mustStartNothing()
	if len(r.cancelRequests()) != 0 {
		t.Errorf("a run was cancelled although cancelling was not asked for: %v", r.cancelRequests())
	}
	w := r.entriesOfKind(supervisor.KindWithheld)
	if len(w) != 1 || w[0].Code != diag.CodeLaunchWithheld || w[0].JobID != 20 {
		t.Errorf("withheld entries = %+v", w)
	}
	if !strings.Contains(r.log.String(), diag.CodeLaunchWithheld) {
		t.Errorf("the log must name %s:\n%s", diag.CodeLaunchWithheld, r.log.String())
	}

	// The moment the stranger is trusted (their key is added), both are admitted and run in order.
	r.trustKey("mallory")
	r.srv.UpdateJob(repo, 20, func(j *provider.Job) { j.Status = "completed" }) // not part of this check
	out, err := r.sup.Tick(ctx)
	if err != nil || !out.Launched || out.JobID != 10 {
		t.Fatalf("after trusting the stranger: %+v, %v", out, err)
	}
}

func TestUnadmittedJobThatIsOnlyWaitingOnAnotherJobStillBlocks(t *testing.T) {
	for _, jobStatus := range []string{"waiting", "pending", "requested", "some-status-nobody-has-heard-of"} {
		t.Run(jobStatus, func(t *testing.T) {
			r := newRig(t)
			r.queue(q{run: 1, job: 10})
			// The stranger's run is already in progress (a hosted job is running), but a second
			// job of it, for this runner's labels, is not queued yet: it will be, any moment.
			r.queue(q{run: 2, job: 20, event: "pull_request", who: "mallory", headRepo: fork, runState: "in_progress", status: jobStatus})
			out, err := r.sup.Tick(ctx)
			if err != nil || !out.Withheld || out.Launched {
				t.Fatalf("outcome = %+v, err = %v", out, err)
			}
			r.mustStartNothing()
		})
	}
}

func TestStrangersJobAlreadyRunningElsewhereDoesNotBlock(t *testing.T) {
	r := newRig(t)
	r.queue(q{run: 1, job: 10})
	r.queue(q{run: 2, job: 20, event: "pull_request", who: "mallory", headRepo: fork, runState: "in_progress", status: "in_progress"})
	r.queue(q{run: 3, job: 30, event: "pull_request", who: "mallory", headRepo: fork, runState: "completed", status: "completed"})
	r.rt.OnRun = r.takes(repo, 10, 1)
	if out, err := r.sup.Tick(ctx); err != nil || !out.Launched {
		t.Fatalf("outcome = %+v, err = %v: a job that is running or finished cannot be taken", out, err)
	}
}

func TestCancellingRefusedRunsClearsTheQueue(t *testing.T) {
	r := newRig(t, withConfig(func(c *supervisor.Config) { c.CancelUnadmitted = true }))
	r.queue(q{run: 1, job: 10})
	r.stranger(2, 20)
	r.rt.OnRun = r.takes(repo, 10, 1)

	out, err := r.sup.Tick(ctx)
	if err != nil || !out.Withheld || out.Launched {
		t.Fatalf("first cycle: %+v, %v: the cancel is only a request, nothing starts yet", out, err)
	}
	if got := r.cancelRequests(); len(got) != 1 || got[0] != "POST /repos/"+repo+"/actions/runs/2/cancel" {
		t.Errorf("cancel requests = %v: only the refused run, never the admitted one", got)
	}
	r.mustStartNothing()
	if c := r.entriesOfKind(supervisor.KindCancel); len(c) != 1 || c[0].RunID != 2 {
		t.Errorf("cancel entries = %+v", c)
	}

	// The queue is read again: GitHub has cancelled the stranger's run, so the owner's job runs.
	out, err = r.sup.Tick(ctx)
	if err != nil || !out.Launched || out.JobID != 10 {
		t.Fatalf("second cycle: %+v, %v", out, err)
	}
}

func TestACancelThatDoesNotTakeEffectNeverUnblocksTheQueue(t *testing.T) {
	r := newRig(t, withConfig(func(c *supervisor.Config) { c.CancelUnadmitted = true }))
	r.srv.CancelIgnored = true // GitHub says 202 and does nothing (or is slow)
	r.queue(q{run: 1, job: 10})
	r.stranger(2, 20)
	for i := 0; i < 4; i++ {
		out, err := r.sup.Tick(ctx)
		if err != nil || out.Launched || !out.Withheld {
			t.Fatalf("cycle %d: %+v, %v", i, out, err)
		}
	}
	r.mustStartNothing()
	if n := len(r.cancelRequests()); n != 4 {
		t.Errorf("cancel requested %d times, want once per cycle until it takes effect", n)
	}
	if n := len(r.entriesOfKind(supervisor.KindCancel)); n != 1 {
		t.Errorf("%d cancel records: the audit log must not grow every poll", n)
	}
}

func TestACancelThatFailsIsRecordedAndStillStartsNothing(t *testing.T) {
	r := newRig(t, withConfig(func(c *supervisor.Config) { c.CancelUnadmitted = true }))
	r.srv.CancelUnsupported = true
	r.queue(q{run: 1, job: 10})
	r.stranger(2, 20)
	out, err := r.sup.Tick(ctx)
	if err != nil || out.Launched {
		t.Fatalf("%+v, %v", out, err)
	}
	c := r.entriesOfKind(supervisor.KindCancel)
	if len(c) != 1 || c[0].Code != diag.CodeLaunchFailed {
		t.Errorf("cancel entries = %+v", c)
	}
	r.mustStartNothing()
}

// hookedProvider lets a test change the world at the moment the runner is registered, which is
// when the real race is widest. Everything else is the real client talking to the fake GitHub.
type hookedProvider struct {
	provider.Provider
	afterJIT func()
}

func (h *hookedProvider) GenerateJITConfig(c context.Context, s provider.Scope, name string, labels []string) (provider.JITConfig, error) {
	cfg, err := h.Provider.GenerateJITConfig(c, s, name, labels)
	if err == nil && h.afterJIT != nil {
		h.afterJIT()
	}
	return cfg, err
}

func TestAJobThatArrivesWhileTheRunnerIsBeingRegisteredIsCaught(t *testing.T) {
	hp := &hookedProvider{}
	r := newRig(t, withProvider(func(p provider.Provider) provider.Provider { hp.Provider = p; return hp }))
	r.queue(q{run: 1, job: 10})
	hp.afterJIT = func() { r.stranger(2, 20) }

	out, err := r.sup.Tick(ctx)
	if err != nil || out.Launched || !out.Withheld {
		t.Fatalf("outcome = %+v, err = %v", out, err)
	}
	if n := len(r.rt.Specs()); n != 0 {
		t.Errorf("%d container(s) started although a stranger's job arrived before the runner could start", n)
	}
	if rs := r.srv.Runners(repo); len(rs) != 0 {
		t.Errorf("the registration was left behind: %+v", rs)
	}
	w := r.entriesOfKind(supervisor.KindWithheld)
	if len(w) != 1 || w[0].Code != diag.CodeLaunchWithheld || w[0].Runner == "" {
		t.Errorf("withheld entries = %+v", w)
	}
	if len(r.entriesOfKind(supervisor.KindLaunching)) != 1 {
		t.Error("the intent to launch is recorded before the registration")
	}
}

func TestAJobThatArrivesWhileTheRunnerWaitsStopsTheRunner(t *testing.T) {
	r := newRig(t)
	r.queue(q{run: 1, job: 10})
	stopped := false
	r.rt.OnRun = func(c context.Context, spec isolation.Spec) (isolation.Result, error) {
		r.stranger(2, 20) // arrives before this runner has been given any job
		select {
		case <-c.Done():
			stopped = true
		case <-time.After(10 * time.Second):
		}
		return isolation.Result{}, nil
	}
	out, err := r.sup.Tick(ctx)
	if err != nil {
		t.Fatalf("a withheld runner is not an error: %v", err)
	}
	if !stopped || !out.Withheld {
		t.Fatalf("stopped = %v, outcome = %+v: the runner must be stopped when a job it could take is not admitted", stopped, out)
	}
	if rs := r.srv.Runners(repo); len(rs) != 0 {
		t.Errorf("registration left behind: %+v", rs)
	}
	f := r.entriesOfKind(supervisor.KindFinished)
	if len(f) != 1 || f[0].Code != diag.CodeLaunchWithheld || !strings.Contains(f[0].Message, "job 20") {
		t.Errorf("finished = %+v", f)
	}
	if len(r.entriesOfKind(supervisor.KindAlarm)) != 0 {
		t.Error("nothing was handed to the runner: this is not an alarm")
	}
}

func TestOnceTheRunnerHasItsJobNewJobsCannotReachIt(t *testing.T) {
	r := newRig(t)
	r.queue(q{run: 1, job: 10})
	stopped := false
	r.rt.OnRun = func(c context.Context, spec isolation.Spec) (isolation.Result, error) {
		// The runner is handed job 10 (still running), then a stranger's job arrives. A
		// single-use runner takes one job only, so the supervisor must not kill a running job.
		r.srv.UpdateJob(repo, 10, func(j *provider.Job) { j.Status, j.RunnerName = "in_progress", spec.Name })
		r.stranger(2, 20)
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
		t.Error("a running admitted job was killed because another job arrived")
	}
}

func TestARunnerHandedAnUnadmittedJobIsStoppedAndAnAlarmIsRaised(t *testing.T) {
	r := newRig(t)
	r.queue(q{run: 1, job: 10})
	stopped := false
	r.rt.OnRun = func(c context.Context, spec isolation.Spec) (isolation.Result, error) {
		// The race lost: the stranger's job appeared and GitHub gave it to OUR runner.
		r.stranger(2, 20)
		r.srv.UpdateJob(repo, 20, func(j *provider.Job) { j.Status, j.RunnerName = "in_progress", spec.Name })
		select {
		case <-c.Done():
			stopped = true
		case <-time.After(10 * time.Second):
		}
		return isolation.Result{}, nil
	}
	_, err := r.sup.Tick(ctx)
	if diag.CodeOf(err) != diag.CodeUnexpectedJob {
		t.Fatalf("err = %v, want BR-E077", err)
	}
	if !stopped {
		t.Error("the container was not stopped")
	}
	a := r.entriesOfKind(supervisor.KindAlarm)
	if len(a) != 1 || a[0].Code != diag.CodeUnexpectedJob || !strings.Contains(a[0].Message, "ALARM") {
		t.Errorf("alarm entries = %+v", a)
	}
	if rs := r.srv.Runners(repo); len(rs) != 0 {
		t.Errorf("registration left behind: %+v", rs)
	}
}

func TestAnUnadmittedJobTakenByAFastRunnerIsStillDetectedAfterwards(t *testing.T) {
	r := newRig(t)
	r.queue(q{run: 1, job: 10})
	r.rt.OnRun = func(c context.Context, spec isolation.Spec) (isolation.Result, error) {
		// Over before the watcher's first tick: the runner took a stranger's job and finished.
		r.stranger(2, 20)
		r.srv.UpdateJob(repo, 20, func(j *provider.Job) { j.Status, j.RunnerName = "completed", spec.Name })
		r.srv.SetRunStatus(repo, 2, "completed")
		return isolation.Result{}, nil
	}
	r.cfg.WatchInterval = time.Hour
	s := r.newSupervisor()
	_, err := s.Tick(ctx)
	if diag.CodeOf(err) != diag.CodeUnexpectedJob {
		t.Fatalf("err = %v, want BR-E077: it must at least be known", err)
	}
	a := r.entriesOfKind(supervisor.KindAlarm)
	if len(a) != 1 {
		t.Fatalf("alarm entries = %+v", a)
	}
	if f := r.entriesOfKind(supervisor.KindFinished); len(f) != 1 || len(f[0].JobsRun) != 1 || f[0].JobsRun[0] != 20 {
		t.Errorf("finished = %+v: the record must say which job the runner really took", f)
	}
}

func TestWatcherStopsAWaitingRunnerWhenItCanNoLongerSeeTheQueue(t *testing.T) {
	r := newRig(t)
	r.queue(q{run: 1, job: 10})
	stopped := false
	r.rt.OnRun = func(c context.Context, spec isolation.Spec) (isolation.Result, error) {
		r.srv.SetFailPath("/actions/runs") // GitHub stops answering while the runner waits
		select {
		case <-c.Done():
			stopped = true
		case <-time.After(10 * time.Second):
		}
		r.srv.SetFailPath("")
		return isolation.Result{}, nil
	}
	if _, err := r.sup.Tick(ctx); err != nil {
		t.Fatalf("%v", err)
	}
	if !stopped {
		t.Fatal("a runner that has no job, and whose exposure cannot be checked, must be stopped")
	}
	f := r.entriesOfKind(supervisor.KindFinished)
	if len(f) != 1 || f[0].Code != diag.CodeQueueUnreadable {
		t.Errorf("finished = %+v", f)
	}
}

// What is NOT closed, as a test that documents it (see docs/decisions/0007-supervisor.md): a
// stranger's job queued AFTER the last check and taken at once cannot be prevented by anything
// the supervisor does, only detected (previous tests). Here the runner is handed it with no
// chance to be stopped, and the supervisor's honest outcome is the alarm.
func TestKnownLimit_TheRaceBetweenTheLastCheckAndTheRunnerBeingHandedAJob(t *testing.T) {
	r := newRig(t)
	r.queue(q{run: 1, job: 10})
	ran := false
	r.rt.OnRun = func(c context.Context, spec isolation.Spec) (isolation.Result, error) {
		ran = true // code may start here, inside the container
		r.stranger(2, 20)
		r.srv.UpdateJob(repo, 20, func(j *provider.Job) { j.Status, j.RunnerName = "in_progress", spec.Name })
		r.srv.SetRunStatus(repo, 2, "completed")
		return isolation.Result{}, nil
	}
	r.cfg.WatchInterval = time.Hour // the job is handed over before any check can see it
	s := r.newSupervisor()
	_, err := s.Tick(ctx)
	if !ran || diag.CodeOf(err) != diag.CodeUnexpectedJob {
		t.Fatalf("ran = %v, err = %v: the limit is that this is detected, not prevented", ran, err)
	}
}
