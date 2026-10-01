package supervisor

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/provider"
)

// Job statuses the supervisor understands. GitHub's wording is an assumption (C3); any other
// status is treated as "might still be taken", never as "finished", so a surprise fails closed.
const (
	statusQueued     = "queued"
	statusInProgress = "in_progress"
	statusCompleted  = "completed"
)

// pendingRunStatuses are the run statuses scanned for jobs a runner could still take, in REVERSE
// lifecycle order: a run only moves requested, pending, waiting, queued, in_progress, completed,
// so a run that advances while the statuses are being read moves into a status that has not been
// read yet, never into one already read. Read the other way round (queued first), a run that
// goes from waiting to queued between two listings is in neither, and an unadmitted job slips
// past the scan. A run that is in progress can still have a job that is not yet queued (it waits
// for another job), so "queued" alone would miss it as well.
var pendingRunStatuses = []string{"requested", "pending", "waiting", "queued", "in_progress"}

// scanPasses is how many times the pending statuses are read. The order above makes one pass
// consistent for runs that advance; a run CREATED after its status was read is only caught by
// reading again, and the union of the passes is the picture.
const scanPasses = 2

// observed is a job together with the run it belongs to.
type observed struct {
	Run provider.Run
	Job provider.Job
}

// completedRunsScanned bounds the post-run look at finished runs (newest first).
const completedRunsScanned = 30

// scan reads the whole picture from the provider: every run that is not finished, and every job
// of each. Nothing here is remembered between calls: state is reconciled from the provider each
// time, never trusted from disk. withCompleted adds the newest finished runs, to see which job a
// runner that has already exited took. Any provider failure is an error: a partial picture must
// never be mistaken for an empty queue.
//
// A listing is not an atomic snapshot (each status is a separate request), so the picture is the
// union of two passes read in reverse lifecycle order (see pendingRunStatuses). A run seen in the
// first pass is not read again in the second: only runs that are new then cost a jobs request.
func (s *Supervisor) scan(ctx context.Context, withCompleted bool) ([]observed, error) {
	seen := map[int64]bool{}
	var out []observed
	read := func(r provider.Run) error {
		if seen[r.ID] {
			return nil
		}
		seen[r.ID] = true
		jobs, err := s.cfg.Provider.ListJobs(ctx, s.cfg.Scope.Repository, r.ID)
		if err != nil {
			return queueErr(err, "cannot list the jobs of run "+itoa(r.ID))
		}
		for _, j := range jobs {
			out = append(out, observed{Run: r, Job: j})
		}
		return nil
	}
	for pass := 0; pass < scanPasses; pass++ {
		for _, st := range pendingRunStatuses {
			runs, err := s.cfg.Provider.ListRuns(ctx, s.cfg.Scope.Repository, st)
			if err != nil {
				return nil, queueErr(err, "cannot list "+st+" runs")
			}
			for _, r := range runs {
				if err := read(r); err != nil {
					return nil, err
				}
			}
		}
	}
	if withCompleted { // last: completed is the end of the lifecycle
		runs, err := s.cfg.Provider.ListRecentRuns(ctx, s.cfg.Scope.Repository, statusCompleted, completedRunsScanned)
		if err != nil {
			return nil, queueErr(err, "cannot list completed runs")
		}
		for _, r := range runs {
			if err := read(r); err != nil {
				return nil, err
			}
		}
	}
	// Oldest first, so the longest-waiting admitted job is served first and the order is stable.
	sort.SliceStable(out, func(i, k int) bool {
		if out[i].Run.ID != out[k].Run.ID {
			return out[i].Run.ID < out[k].Run.ID
		}
		return out[i].Job.ID < out[k].Job.ID
	})
	return out, nil
}

func queueErr(err error, what string) error {
	return diag.Wrap(err, diag.CodeQueueUnreadable, "the queue could not be read: "+what,
		"GitHub is unreachable, rate-limited or answered something unexpected", "nothing was started; the supervisor tries again at the next poll")
}

// labelSet lower-cases labels: GitHub compares them case-insensitively.
func labelSet(labels []string) map[string]bool {
	m := make(map[string]bool, len(labels))
	for _, l := range labels {
		m[strings.ToLower(strings.TrimSpace(l))] = true
	}
	return m
}

// couldTake reports whether a runner with these labels might be handed the job: GitHub gives a
// job to a runner that has ALL of the job's labels. A job that names no labels is counted as
// takeable too: the supervisor cannot rule it out, and a wrong "no" is the dangerous direction.
func couldTake(runner map[string]bool, job provider.Job) bool {
	for _, l := range job.Labels {
		if !runner[strings.ToLower(strings.TrimSpace(l))] {
			return false
		}
	}
	return true
}

// stillWaiting reports whether a job might yet be taken by a runner: anything not known to be
// running or finished, including statuses this code has never heard of.
func stillWaiting(j provider.Job) bool {
	return j.Status != statusInProgress && j.Status != statusCompleted
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
