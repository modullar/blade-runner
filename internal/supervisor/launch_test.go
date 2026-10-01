package supervisor_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/isolation"
	"github.com/modullar/blade-runner/internal/provider"
	"github.com/modullar/blade-runner/internal/supervisor"
)

var errBoom = errors.New("disk full")

// failOn is the real audit log, except that writing the listed kinds of entry fails.
type failOn struct {
	supervisor.Recorder
	kinds map[string]bool
}

func (f failOn) Record(e supervisor.Entry) error {
	if f.kinds[e.Kind] {
		return errBoom
	}
	return f.Recorder.Record(e)
}

func failingToRecord(kinds ...string) option {
	return withConfig(func(c *supervisor.Config) {
		m := map[string]bool{}
		for _, k := range kinds {
			m[k] = true
		}
		c.Audit = failOn{Recorder: c.Audit, kinds: m}
	})
}

func TestARunnerWhoseJobsCannotBeConfirmedRaisesAnAlarmAndFails(t *testing.T) {
	r := newRig(t, withConfig(func(c *supervisor.Config) { c.WatchInterval = time.Hour }))
	r.queue(q{run: 1, job: 10})
	r.rt.OnRun = func(c context.Context, spec isolation.Spec) (isolation.Result, error) {
		// The runner ran and exited, and GitHub stops answering before the supervisor can ask
		// which job it took: it may have taken anything.
		r.srv.SetFailPath("/actions/runs")
		return isolation.Result{}, nil
	}
	_, err := r.sup.Tick(ctx)
	r.srv.SetFailPath("")
	if err == nil || diag.CodeOf(err) != diag.CodeQueueUnreadable {
		t.Fatalf("err = %v: a runner that may have taken an unadmitted job is not a success", err)
	}
	a := r.entriesOfKind(supervisor.KindAlarm)
	if len(a) != 1 || a[0].Code != diag.CodeQueueUnreadable || a[0].Runner == "" {
		t.Fatalf("alarm entries = %+v", a)
	}
	if f := r.entriesOfKind(supervisor.KindFinished); len(f) != 1 {
		t.Errorf("finished entries = %+v", f)
	}
	if rs := r.srv.Runners(repo); len(rs) != 0 {
		t.Errorf("registration left behind: %+v", rs)
	}
}

func TestAuditWriteFailuresOnTheAlarmFailedAndRecheckPathsAreNotDiscarded(t *testing.T) {
	t.Run("alarm", func(t *testing.T) {
		r := newRig(t, failingToRecord(supervisor.KindAlarm))
		r.queue(q{run: 1, job: 10})
		r.rt.OnRun = func(c context.Context, spec isolation.Spec) (isolation.Result, error) {
			r.stranger(2, 20)
			r.srv.UpdateJob(repo, 20, func(j *provider.Job) { j.Status, j.RunnerName = "completed", spec.Name })
			r.srv.SetRunStatus(repo, 2, "completed")
			return isolation.Result{}, nil
		}
		_, err := r.sup.Tick(ctx)
		if !errors.Is(err, errBoom) {
			t.Fatalf("err = %v: the alarm could not be recorded and nobody was told", err)
		}
		if diag.CodeOf(err) != diag.CodeUnexpectedJob {
			t.Errorf("the alarm itself must still be reported, got code %q", diag.CodeOf(err))
		}
	})
	t.Run("launch failed", func(t *testing.T) {
		r := newRig(t, failingToRecord(supervisor.KindError))
		r.queue(q{run: 1, job: 10})
		r.srv.JITUnsupported = true
		_, err := r.sup.Tick(ctx)
		if !errors.Is(err, errBoom) {
			t.Fatalf("err = %v: the failure could not be recorded and nobody was told", err)
		}
		if diag.CodeOf(err) != diag.CodeLaunchFailed {
			t.Errorf("the launch failure itself must still be reported, got code %q", diag.CodeOf(err))
		}
	})
	t.Run("recheck unreadable", func(t *testing.T) {
		hp := &hookedProvider{}
		r := newRig(t, failingToRecord(supervisor.KindWithheld), withProvider(func(p provider.Provider) provider.Provider { hp.Provider = p; return hp }))
		r.queue(q{run: 1, job: 10})
		hp.afterJIT = func() { r.srv.SetFailPath("/actions/runs") }
		_, err := r.sup.Tick(ctx)
		r.srv.SetFailPath("")
		if !errors.Is(err, errBoom) {
			t.Fatalf("err = %v", err)
		}
		if diag.CodeOf(err) != diag.CodeQueueUnreadable {
			t.Errorf("the unreadable queue must still be reported, got code %q", diag.CodeOf(err))
		}
	})
}

type auditDown struct{ supervisor.Recorder }

func (auditDown) Record(e supervisor.Entry) error {
	if e.Kind == supervisor.KindLaunching {
		return diag.New(diag.CodeAuditFailed, "cannot write to the audit log", "the disk is full", "free space")
	}
	return nil
}

func TestTheLoopStopsWhenTheAuditLogCannotBeWritten(t *testing.T) {
	r := newRig(t, withConfig(func(c *supervisor.Config) { c.Audit = auditDown{c.Audit} }))
	r.queue(q{run: 1, job: 10})
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := r.sup.Run(c)
	if diag.CodeOf(err) != diag.CodeAuditFailed || c.Err() != nil {
		t.Fatalf("Run = %v (context: %v): it must stop at once, not poll on without a log", err, c.Err())
	}
	r.mustStartNothing()
}

func TestALaunchThatWasOnlyWithheldDoesNotUseUpTheJobsAttempts(t *testing.T) {
	hp := &hookedProvider{}
	r := newRig(t, withProvider(func(p provider.Provider) provider.Provider { hp.Provider = p; return hp }))
	r.queue(q{run: 1, job: 10})
	r.rt.OnRun = r.takes(repo, 10, 1)
	stranger := int64(1)
	hp.afterJIT = func() { // a stranger's job turns up while the runner is being registered
		stranger++
		r.stranger(stranger, stranger*10)
	}
	// Launches in a row are called off at the re-check (up to the refund cap). None of them failed
	// the admitted job.
	for i := 0; i < 3; i++ {
		out, err := r.sup.Tick(ctx)
		if err != nil || !out.Withheld || out.Launched {
			t.Fatalf("round %d: %+v, %v", i, out, err)
		}
		r.srv.UpdateJob(repo, stranger*10, func(j *provider.Job) { j.Status = "completed" }) // gone again
		r.srv.SetRunStatus(repo, stranger, "completed")
	}
	hp.afterJIT = nil
	out, err := r.sup.Tick(ctx)
	if err != nil || !out.Launched || out.JobID != 10 {
		t.Fatalf("after three withheld launches the admitted job must still run: %+v, %v", out, err)
	}
	if n := len(r.entriesOfKind(supervisor.KindGaveUp)); n != 0 {
		t.Errorf("gave up on a job that never failed: %d entries", n)
	}
}
