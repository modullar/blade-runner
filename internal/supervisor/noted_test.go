package supervisor_test

import (
	"context"
	"sync"
	"testing"

	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/isolation"
	"github.com/modullar/blade-runner/internal/provider"
	"github.com/modullar/blade-runner/internal/supervisor"
)

// countingLog is an audit Recorder that only counts, so thousands of entries cost no fsync.
type countingLog struct {
	mu      sync.Mutex
	refused map[int64]int
	entries int
}

func (c *countingLog) Record(e supervisor.Entry) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries++
	if e.Kind == supervisor.KindRefused {
		c.refused[e.JobID]++
	}
	return nil
}

func TestAQueueLargerThanTheBookkeepingBoundStillRecordsEachRefusalOnce(t *testing.T) {
	// 5000 jobs (five runs of a thousand: GitHub lists no more per run), every one refused, polled over and over. When the "already recorded"
	// set filled up it used to be emptied, so every refusal fired again on the very next poll.
	log := &countingLog{refused: map[int64]int{}}
	r := newRig(t, withConfig(func(c *supervisor.Config) { c.Audit = log }))
	const n = 5000
	sha := r.commits["mallory"].SHA
	for run := int64(1); run <= 5; run++ {
		r.srv.AddRun(repo, provider.Run{ID: run, HeadSHA: sha, Event: "push", Status: "queued", HeadRepository: repo, Actor: "mallory"})
	}
	for i := int64(1); i <= n; i++ {
		r.srv.AddJob(repo, provider.Job{ID: i, RunID: (i-1)/1000 + 1, Status: "queued", Labels: []string{"self-hosted", "gpu"}, HeadSHA: sha})
	}
	for poll := 0; poll < 4; poll++ {
		out, err := r.sup.Tick(ctx)
		if err != nil || len(out.Refused) != n || !out.Withheld {
			t.Fatalf("poll %d: %d refused, withheld %v, err %v", poll, len(out.Refused), out.Withheld, err)
		}
	}
	if len(log.refused) != n {
		t.Fatalf("%d jobs have a refusal on record, want %d", len(log.refused), n)
	}
	for id, c := range log.refused {
		if c != 1 {
			t.Fatalf("job %d was recorded %d times over 4 polls, want once", id, c)
		}
	}
	// Anything that is not a per-job refusal is a handful of entries, not one per poll per job.
	if log.entries > n+10 {
		t.Errorf("%d audit entries for %d refusals", log.entries, n)
	}
}

func TestAGaveUpJobIsNotGivenFreshAttemptsByOneScanThatMissesIt(t *testing.T) {
	r := newRig(t)
	r.queue(q{run: 1, job: 10})
	r.rt.OnRun = func(context.Context, isolation.Spec) (isolation.Result, error) {
		return isolation.Result{}, diag.New(diag.CodeIsolation, "docker said no", "x", "y")
	}
	for i := 0; i < 3; i++ {
		if _, err := r.sup.Tick(ctx); err == nil {
			t.Fatalf("attempt %d should have failed", i+1)
		}
	}
	if out, err := r.sup.Tick(ctx); err != nil || !out.Idle || len(r.rt.Specs()) != 3 {
		t.Fatalf("after giving up: %+v, %v, containers %d", out, err, len(r.rt.Specs()))
	}
	// One scan in which the job is not listed (the run looks completed for a moment) ...
	r.srv.SetRunStatus(repo, 1, "completed")
	if out, err := r.sup.Tick(ctx); err != nil || !out.Idle {
		t.Fatalf("while it is missing: %+v, %v", out, err)
	}
	r.srv.SetRunStatus(repo, 1, "queued")
	// ... must not hand it a fresh set of three attempts.
	for i := 0; i < 3; i++ {
		if out, err := r.sup.Tick(ctx); err != nil || out.Launched || !out.Idle {
			t.Fatalf("round %d: %+v, %v: a job that was given up on was started again after a single missing scan", i, out, err)
		}
	}
	if len(r.rt.Specs()) != 3 {
		t.Errorf("%d containers, want 3", len(r.rt.Specs()))
	}
	// Gone for good (absent for many scans), it is forgotten, and an id that comes back is news.
	r.srv.SetRunStatus(repo, 1, "completed")
	for i := 0; i < 6; i++ {
		_, _ = r.sup.Tick(ctx)
	}
	r.srv.SetRunStatus(repo, 1, "queued")
	r.rt.OnRun = nil
	if out, err := r.sup.Tick(ctx); err != nil || !out.Launched {
		t.Fatalf("after being absent for good: %+v, %v", out, err)
	}
}
