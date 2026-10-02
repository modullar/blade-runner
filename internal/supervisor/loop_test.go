package supervisor_test

import (
	"context"
	"testing"
	"time"

	"github.com/modullar/blade-runner/internal/isolation"
	"github.com/modullar/blade-runner/internal/provider"
	"github.com/modullar/blade-runner/internal/supervisor"
)

// strangerEachTime scripts a runner whose container is always stopped by the watcher: a stranger's
// job turns up while it waits (so the watcher stops it), and is gone again before the next cycle,
// so the admitted job is the only thing left waiting every time.
func (r *rig) strangerEachTime(launches *int, after func(n int)) func(context.Context, isolation.Spec) (isolation.Result, error) {
	return func(c context.Context, spec isolation.Spec) (isolation.Result, error) {
		*launches++
		n := int64(*launches)
		r.stranger(100+n, 1000+n)
		select {
		case <-c.Done():
		case <-time.After(5 * time.Second):
		}
		r.srv.UpdateJob(repo, 1000+n, func(j *provider.Job) { j.Status = "completed" })
		r.srv.SetRunStatus(repo, 100+n, "completed")
		if after != nil {
			after(*launches)
		}
		return isolation.Result{}, nil
	}
}

func TestARunnerTheWatcherKeepsStoppingIsGivenUpOnAndTheLoopBacksOff(t *testing.T) {
	r := newRig(t)
	r.queue(q{run: 1, job: 10})
	c, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	launches, sleeps := 0, 0
	var sleepsAfterLaunch []int
	r.rt.OnRun = r.strangerEachTime(&launches, func(n int) {
		sleepsAfterLaunch = append(sleepsAfterLaunch, sleeps)
		if n >= 12 { // a runaway: the old loop never gives up and never sleeps
			cancel()
		}
	})
	r.cfg.Sleep = func(context.Context, time.Duration) {
		sleeps++
		if sleeps == launches+3 { // the loop idled for a few cycles after the last launch
			cancel()
		}
	}
	if err := r.newSupervisor().Run(c); err != nil {
		t.Fatal(err)
	}
	// Three stops are refunded; the fourth gives the job up until a restart.
	if launches != 4 {
		t.Errorf("%d runners were started for a job whose runner is stopped every time, want 4 (three refunds, then give up)", launches)
	}
	if g := r.entriesOfKind(supervisor.KindGaveUp); len(g) != 1 || g[0].JobID != 10 {
		t.Errorf("gave_up entries = %+v", g)
	}
	// Each of those launches was followed by a sleep, not by an immediate next cycle.
	for i := 1; i < len(sleepsAfterLaunch); i++ {
		if sleepsAfterLaunch[i] <= sleepsAfterLaunch[i-1] {
			t.Errorf("no sleep between launch %d and launch %d (sleeps seen at the end of each launch: %v): a withheld or stopped launch must back off by the poll interval", i, i+1, sleepsAfterLaunch)
			break
		}
	}
	if rs := r.srv.Runners(repo); len(rs) != 0 {
		t.Errorf("registrations left behind: %+v", rs)
	}
}

func TestARunnerThatRanItsJobStartsTheNextCycleAtOnce(t *testing.T) {
	// The backoff is only for launches that did not do their job: a queue still drains quickly.
	r := newRig(t)
	r.queue(q{run: 1, job: 10})
	r.queue(q{run: 2, job: 20})
	r.rt.OnRun = func(c context.Context, spec isolation.Spec) (isolation.Result, error) {
		id, run := int64(10), int64(1)
		if len(r.rt.Specs()) == 2 {
			id, run = 20, 2
		}
		return r.takes(repo, id, run)(c, spec)
	}
	c, cancel := context.WithCancel(ctx)
	defer cancel()
	sleepsAtSecondLaunch, sleeps := -1, 0
	r.cfg.Sleep = func(context.Context, time.Duration) {
		sleeps++
		if sleeps == 1 {
			cancel()
		}
	}
	prev := r.rt.OnRun
	r.rt.OnRun = func(c context.Context, spec isolation.Spec) (isolation.Result, error) {
		if len(r.rt.Specs()) == 2 {
			sleepsAtSecondLaunch = sleeps
		}
		return prev(c, spec)
	}
	if err := r.newSupervisor().Run(c); err != nil {
		t.Fatal(err)
	}
	if len(r.rt.Specs()) != 2 || sleepsAtSecondLaunch != 0 {
		t.Errorf("containers %d, sleeps before the second launch %d: a runner that ran its job must be followed by the next cycle at once", len(r.rt.Specs()), sleepsAtSecondLaunch)
	}
}

func TestCalledOffLaunchesAreRefundedOnlyThreeTimesInARow(t *testing.T) {
	hp := &hookedProvider{}
	r := newRig(t, withProvider(func(p provider.Provider) provider.Provider { hp.Provider = p; return hp }))
	r.queue(q{run: 1, job: 10})
	r.rt.OnRun = r.takes(repo, 10, 1)
	stranger := int64(1)
	hp.afterJIT = func() { // a stranger's job turns up while the runner is being registered
		stranger++
		r.stranger(stranger, stranger*10)
	}
	for i := 0; i < 4; i++ {
		if out, err := r.sup.Tick(ctx); err != nil || !out.Withheld || out.Launched {
			t.Fatalf("round %d: %+v, %v", i, out, err)
		}
		r.srv.UpdateJob(repo, stranger*10, func(j *provider.Job) { j.Status = "completed" })
		r.srv.SetRunStatus(repo, stranger, "completed")
	}
	hp.afterJIT = nil
	out, err := r.sup.Tick(ctx)
	if err != nil || out.Launched || !out.Idle {
		t.Fatalf("the fourth called-off launch must give the job up until a restart: %+v, %v", out, err)
	}
	if g := r.entriesOfKind(supervisor.KindGaveUp); len(g) != 1 {
		t.Errorf("gave_up entries = %+v", g)
	}
	// A restart forgets.
	if out, err := r.newSupervisor().Tick(ctx); err != nil || !out.Launched {
		t.Fatalf("after a restart: %+v, %v", out, err)
	}
}
