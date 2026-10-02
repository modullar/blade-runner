package supervisor_test

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modullar/blade-runner/internal/provider"
	"github.com/modullar/blade-runner/internal/supervisor"
)

// listHook lets a test change the world between two run listings, which is how a queue that is
// read one status at a time can lose a run: it moves from a status not yet read to one already
// read. Everything else is the real client talking to the fake GitHub.
type listHook struct {
	provider.Provider
	mu    sync.Mutex
	calls int
	after func(call int, status string)
	// afterJobs runs after the jobs of a run were read (and before the scan sees the answer's
	// consequences): the world changes between two readings of the same run.
	afterJobs func(runID int64)
}

func (h *listHook) ListJobs(c context.Context, repoName string, runID int64) ([]provider.Job, error) {
	jobs, err := h.Provider.ListJobs(c, repoName, runID)
	h.mu.Lock()
	f := h.afterJobs
	h.mu.Unlock()
	if f != nil {
		f(runID)
	}
	return jobs, err
}

func (h *listHook) tick(status string) {
	h.mu.Lock()
	h.calls++
	n := h.calls
	f := h.after
	h.mu.Unlock()
	if f != nil {
		f(n, status)
	}
}

func (h *listHook) ListRuns(c context.Context, repoName, status string) ([]provider.Run, error) {
	runs, err := h.Provider.ListRuns(c, repoName, status)
	h.tick(status)
	return runs, err
}

func (h *listHook) ListRecentRuns(c context.Context, repoName, status string, limit int) ([]provider.Run, error) {
	runs, err := h.Provider.ListRecentRuns(c, repoName, status, limit)
	h.tick(status)
	return runs, err
}

func hooked(h *listHook) option {
	return withProvider(func(p provider.Provider) provider.Provider { h.Provider = p; return h })
}

func TestARunThatAdvancesBetweenListingsIsNotLostFromTheScan(t *testing.T) {
	// A stranger's run waits (say, for an environment approval) and becomes queued right after
	// the "queued" listing was read. Read in the old order (queued first, waiting later) it is in
	// neither list; read in reverse lifecycle order it is caught by one of them.
	h := &listHook{}
	r := newRig(t, hooked(h))
	r.queue(q{run: 1, job: 10})
	r.queue(q{run: 2, job: 20, event: "pull_request", who: "mallory", headRepo: fork, runState: "waiting", status: "waiting"})
	var once sync.Once
	h.after = func(_ int, status string) {
		if status == "queued" {
			once.Do(func() {
				r.srv.SetRunStatus(repo, 2, "queued")
				r.srv.UpdateJob(repo, 20, func(j *provider.Job) { j.Status = "queued" })
			})
		}
	}
	out, err := r.sup.Tick(ctx)
	if err != nil || out.Launched || !out.Withheld || len(out.Refused) != 1 || out.Refused[0].JobID != 20 {
		t.Fatalf("outcome = %+v, err = %v: the stranger's job slipped between two listings", out, err)
	}
	r.mustStartNothing()
}

func TestARunThatAppearsAfterTheFirstPassIsCaughtByTheSecond(t *testing.T) {
	// The first pass ends; then a stranger's run is created. Only a second pass over the same
	// statuses can see it, and it must be seen before anything is registered.
	h := &listHook{}
	r := newRig(t, hooked(h))
	r.queue(q{run: 1, job: 10})
	var once sync.Once
	h.after = func(call int, _ string) {
		if call == 5 { // the last listing of the first pass
			once.Do(func() {
				r.queue(q{run: 2, job: 20, event: "pull_request", who: "mallory", headRepo: fork, runState: "requested", status: "requested"})
			})
		}
	}
	out, err := r.sup.Tick(ctx)
	if err != nil || out.Launched || len(out.Refused) != 1 || out.Refused[0].JobID != 20 {
		t.Fatalf("outcome = %+v, err = %v", out, err)
	}
	r.mustStartNothing()
}

func TestThePostRunScanDoesNotReadTheWholeHistoryToKeepThirtyRuns(t *testing.T) {
	r := newRig(t, withConfig(func(c *supervisor.Config) { c.WatchInterval = time.Hour }))
	r.queue(q{run: 1, job: 10})
	for i := int64(0); i < 250; i++ {
		r.queue(q{run: 100 + i, job: 1000 + i, runState: "completed", status: "completed"})
	}
	r.rt.OnRun = r.takes(repo, 10, 1)
	if out, err := r.sup.Tick(ctx); err != nil || !out.Launched {
		t.Fatalf("%+v, %v", out, err)
	}
	var completed []string
	for _, u := range r.srv.URIs() {
		if strings.HasPrefix(u, "GET /repos/"+repo+"/actions/runs?") && strings.Contains(u, "status=completed") {
			completed = append(completed, u)
		}
	}
	if len(completed) != 1 {
		t.Fatalf("%d requests for finished runs to keep the newest 30 of 251: %v", len(completed), completed)
	}
	u, _ := url.Parse(strings.TrimPrefix(completed[0], "GET "))
	if u.Query().Get("page") != "1" || u.Query().Get("per_page") != "30" {
		t.Errorf("finished runs were read with %s", u.RawQuery)
	}
}

func TestARunReadWhileEmptyThatLaterHoldsARealJobIsJudgedOnThatJob(t *testing.T) {
	// A stranger's run is listed as "waiting" before GitHub has created its job, so the jobs
	// request answers nothing. By the time the run is listed as "queued" its job exists. Reading
	// each run once and remembering its id for the rest of the scan would keep the empty answer and
	// let the owner's job start with the stranger's job waiting for the same runner.
	h := &listHook{}
	r := newRig(t, hooked(h))
	r.queue(q{run: 1, job: 10})
	r.srv.AddRun(repo, provider.Run{ID: 2, HeadSHA: r.commits["mallory"].SHA, Event: "pull_request", Status: "waiting", HeadRepository: fork, Actor: "mallory"})
	var once sync.Once
	h.afterJobs = func(run int64) {
		if run == 2 {
			once.Do(func() { // the run advances, and its job appears, right after the empty reading
				r.srv.SetRunStatus(repo, 2, "queued")
				r.srv.AddJob(repo, provider.Job{ID: 20, RunID: 2, Status: "queued", Labels: []string{"self-hosted", "gpu"}, HeadSHA: r.commits["mallory"].SHA})
			})
		}
	}
	out, err := r.sup.Tick(ctx)
	if err != nil || out.Launched || !out.Withheld || len(out.Refused) != 1 || out.Refused[0].JobID != 20 {
		t.Fatalf("outcome = %+v, err = %v: the job that appeared after the empty reading was never judged", out, err)
	}
	r.mustStartNothing()
}

func TestARunWhoseFirstReadingWasEmptyIsReadOnceMoreInTheSecondPass(t *testing.T) {
	// Same status the whole time, but the job is created between the first pass and the second.
	h := &listHook{}
	r := newRig(t, hooked(h))
	r.queue(q{run: 1, job: 10})
	r.srv.AddRun(repo, provider.Run{ID: 2, HeadSHA: r.commits["mallory"].SHA, Event: "pull_request", Status: "queued", HeadRepository: fork, Actor: "mallory"})
	var once sync.Once
	h.after = func(call int, _ string) {
		if call == 5 { // the last listing of the first pass
			once.Do(func() {
				r.srv.AddJob(repo, provider.Job{ID: 20, RunID: 2, Status: "queued", Labels: []string{"self-hosted", "gpu"}, HeadSHA: r.commits["mallory"].SHA})
			})
		}
	}
	out, err := r.sup.Tick(ctx)
	if err != nil || out.Launched || len(out.Refused) != 1 || out.Refused[0].JobID != 20 {
		t.Fatalf("outcome = %+v, err = %v", out, err)
	}
	r.mustStartNothing()
}

func TestARunThatIsNotEmptyIsNotReadAgainInTheSecondPass(t *testing.T) {
	// The cost stays: a run with jobs, listed in one status in both passes, costs one jobs request.
	r := newRig(t)
	r.queue(q{run: 1, job: 10})
	r.rt.OnRun = r.takes(repo, 10, 1)
	if _, err := r.sup.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	jobsReads := 0
	for _, u := range r.srv.URIs() {
		if strings.Contains(u, "/runs/1/jobs") {
			jobsReads++
		}
	}
	// assess + re-check after registering + post-run scan = 3 scans, one read of run 1 each.
	if jobsReads != 3 {
		t.Errorf("%d jobs reads of run 1 for 3 scans: a run seen in both passes must be read once per scan", jobsReads)
	}
}

func TestTheRunStatusesAreReadInReverseLifecycleOrderInTwoPasses(t *testing.T) {
	// The order is the guarantee that a run advancing between two listings lands in a status not
	// read yet: requested, pending, waiting, queued, in_progress, then the same again for runs
	// created after their status was read. Two passes would hide a reversed first pass from a
	// behavioural test (the second pass catches what the first missed), so the order itself is
	// pinned, as GitHub sees it.
	r := newRig(t)
	r.queue(q{run: 1, job: 10})
	if _, err := r.sup.Tick(ctx); err != nil { // withholds nothing, starts a runner: only the first scan matters
		t.Fatal(err)
	}
	var seq []string
	for _, u := range r.srv.URIs() {
		if !strings.HasPrefix(u, "GET /repos/"+repo+"/actions/runs?") {
			continue
		}
		parsed, _ := url.Parse(strings.TrimPrefix(u, "GET "))
		seq = append(seq, parsed.Query().Get("status"))
	}
	want := []string{"requested", "pending", "waiting", "queued", "in_progress", "requested", "pending", "waiting", "queued", "in_progress"}
	if len(seq) < len(want) {
		t.Fatalf("listings = %v", seq)
	}
	for i, st := range want {
		if seq[i] != st {
			t.Fatalf("first scan read the statuses in the order %v, want %v: a run that advances between listings can then be missed", seq[:len(want)], want)
		}
	}
}
