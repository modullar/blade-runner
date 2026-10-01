package supervisor

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/isolation"
)

// launchResult says what launch did, for the cycle's Outcome.
type launchResult struct {
	Launched bool // a container ran
	Withheld bool // the launch was abandoned or cut short because something not admitted is waiting
}

// watchState is what the watcher concluded; it is written before done closes and read after.
type watchState struct {
	done    chan struct{}
	stopped bool   // the watcher stopped the container
	alarm   bool   // ... because the runner was handed a job that is not admitted
	code    string // diag code of the reason
	reason  string
}

// launch runs ONE ephemeral runner for the admitted candidate, in this order, each step failing
// closed:
//
//  1. record the intent (no record, no launch);
//  2. ask the provider for a single-use runner registration;
//  3. read the queue AGAIN: the registration took time, and a job may have arrived;
//  4. start the container, and while it has not been given a job, watch for new jobs it could take;
//  5. whatever happened, remove the container and the registration, and record which job(s) the
//     provider says the runner took.
func (s *Supervisor) launch(ctx context.Context, cand judgement, a assessment) (launchResult, error) {
	run, job := cand.Obs.Run, cand.Obs.Job
	name := s.runnerPrefix() + s.cfg.NewID()
	if !s.ownRunner(name) {
		// Reconcile only removes names of exactly this shape, so a runner named otherwise could
		// never be cleaned up after a crash.
		return launchResult{}, diag.New(diag.CodeConfigInvalid, "the generated runner name "+name+" is not of the form br-jit-<runner>-<12 hex digits>",
			"a bug in the caller's NewID", "nothing was started")
	}
	s.attempts[job.ID]++
	base := Entry{
		RunID: run.ID, JobID: job.ID, Repository: cand.Subject.Repository, SHA: cand.Subject.SHA, Event: run.Event, Actor: run.Actor,
		Signer: cand.Verdict.Signer.Name, Fingerprint: cand.Verdict.Signer.Fingerprint, Runner: name,
	}
	entry := func(kind, code, msg string) Entry {
		e := base
		e.Kind, e.Code, e.Message = kind, code, msg
		return e
	}

	intent := fmt.Sprintf("admitted: run %d job %d (%s, commit %s in %s) is signed by %s; starting runner %s",
		run.ID, job.ID, run.Event, short(run.HeadSHA), run.HeadRepository, cand.Verdict.Signer.Name, name)
	s.logf("%s", intent)
	if err := s.cfg.Audit.Record(entry(KindLaunching, "", intent)); err != nil {
		return launchResult{}, err // nothing unrecorded is ever started
	}

	jit, err := s.cfg.Provider.GenerateJITConfig(ctx, s.cfg.Scope, name, job.Labels)
	if err != nil {
		return launchResult{}, s.failed(entry, "cannot register a single-use runner", err)
	}
	// From here on a registration exists. It is removed exactly once, whatever happens.
	var once sync.Once
	var cleanupNote string
	cleanup := func() string {
		once.Do(func() { cleanupNote = s.deregister(name, jit.RunnerID) })
		return cleanupNote
	}
	defer cleanup()

	// Re-read the queue now that the registration exists, before anything can take a job.
	b, err := s.assess(ctx)
	if err != nil {
		s.refundAttempt(job.ID) // the queue could not be read: nothing was tried for this job
		return launchResult{}, s.recordFailure(entry(KindWithheld, diag.CodeOf(err), "runner not started: "+what(err)), err)
	}
	still, ok := b.Admitted[job.ID]
	if !b.clear() || !ok || still.Obs.Job.Status != statusQueued {
		msg := fmt.Sprintf("runner %s not started: the queue changed while it was being registered (a waiting job is not admitted, or the job is no longer waiting)", name)
		s.logf("withheld [%s]: %s", diag.CodeLaunchWithheld, msg)
		s.refundAttempt(job.ID) // called off before any container ran: not a failure of this job
		if err := s.cfg.Audit.Record(entry(KindWithheld, diag.CodeLaunchWithheld, msg)); err != nil {
			return launchResult{Withheld: true}, err
		}
		return launchResult{Withheld: true}, nil
	}
	known := map[int64]bool{}
	for id := range b.Admitted {
		known[id] = true
	}

	spec := isolation.Spec{
		Name: name, Image: s.cfg.Image, Network: s.cfg.Network, Stdin: jit.Encoded,
		MemoryMiB: s.cfg.MemoryMiB, Timeout: s.cfg.Timeout,
		Labels: map[string]string{LabelSupervisor: s.cfg.RunnerName, "bladerunner.run_id": strconv.FormatInt(run.ID, 10), "bladerunner.job_id": strconv.FormatInt(job.ID, 10)},
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	w := &watchState{done: make(chan struct{})}
	go s.watch(runCtx, name, known, cancel, w)
	res, runErr := s.cfg.Runtime.Run(runCtx, spec)
	cancel()
	<-w.done

	// Belt and braces: the runtime removes its container itself, and so does this.
	bg, bgCancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer bgCancel()
	_, _ = s.cfg.Runtime.RemoveStale(bg)
	cleanupMsg := cleanup()

	// Which job did this runner really take? Ask the provider; never assume it was the one chosen.
	var took []int64
	var unexpected []int64
	obs, scanErr := s.scan(bg, true)
	if scanErr == nil {
		for _, o := range obs {
			if o.Job.RunnerName == name {
				took = append(took, o.Job.ID)
				if !known[o.Job.ID] {
					unexpected = append(unexpected, o.Job.ID)
				}
			}
		}
		sort.Slice(took, func(i, k int) bool { return took[i] < took[k] })
	}

	fin := entry(KindFinished, "", "")
	fin.ExitCode, fin.TimedOut, fin.OOMKilled, fin.JobsRun = &res.ExitCode, res.TimedOut, res.OOMKilled, took
	var firstErr error
	switch {
	case w.alarm || len(unexpected) > 0:
		ids := unexpected
		why := w.reason
		if why == "" {
			why = fmt.Sprintf("job(s) %v were not admitted", ids)
		}
		msg := fmt.Sprintf("ALARM: runner %s was handed a job that is not admitted (%s); its container was stopped", name, why)
		s.logf("%s [%s]", msg, diag.CodeUnexpectedJob)
		firstErr = s.recordFailure(entry(KindAlarm, diag.CodeUnexpectedJob, msg),
			diag.New(diag.CodeUnexpectedJob, msg, "the one-runner-one-admitted-job assumption failed (decision 0007)", "read the audit log and treat it as an incident"))
		fin.Message = "runner ended after an alarm"
	case w.stopped:
		msg := fmt.Sprintf("runner %s was stopped before it was given a job: %s", name, w.reason)
		s.logf("withheld [%s]: %s", w.code, msg)
		s.refundAttempt(job.ID) // stopped because of someone else's job, not because this one failed
		fin.Message, fin.Code = msg, w.code
	case runErr != nil:
		fin.Message = "the runner's container could not be run: " + what(runErr)
		fin.Code = diag.CodeLaunchFailed
		firstErr = diag.Wrap(runErr, diag.CodeLaunchFailed, "the runner for run "+strconv.FormatInt(run.ID, 10)+" could not be run", "Docker refused or failed", "see the audit log; the job is retried a few times")
	case scanErr != nil:
		fin.Message = fmt.Sprintf("runner container ended (exit %d) but which job it took could not be confirmed: %s", res.ExitCode, what(scanErr))
		fin.Code = diag.CodeQueueUnreadable
	case len(took) == 0:
		fin.Message = fmt.Sprintf("runner container ended (exit %d) without taking any job", res.ExitCode)
	default:
		fin.Message = fmt.Sprintf("runner container ended (exit %d); the provider says it took job(s) %v", res.ExitCode, took)
		s.attempts[job.ID] = 0
	}
	if cleanupMsg != "" {
		fin.Message += "; " + cleanupMsg
	}
	if scanErr != nil && !w.alarm && len(unexpected) == 0 {
		// The runner is gone, and so is the ability to say which job it took. It may have taken
		// one that was never admitted, so this is an alarm, not a note: record it and fail.
		msg := fmt.Sprintf("ALARM: runner %s ended (exit %d) but which job it took could not be confirmed (%s): it may have run a job that is not admitted", name, res.ExitCode, what(scanErr))
		s.logf("%s [%s]", msg, diag.CodeQueueUnreadable)
		alarmErr := s.recordFailure(entry(KindAlarm, diag.CodeQueueUnreadable, msg),
			diag.Wrap(scanErr, diag.CodeQueueUnreadable, msg, "GitHub could not be read after the runner ended",
				"check the provider's job list for this runner by hand, and treat it as an incident until you have"))
		firstErr = errors.Join(firstErr, alarmErr)
	}
	s.logf("%s", fin.Message)
	if err := s.cfg.Audit.Record(fin); err != nil {
		firstErr = errors.Join(firstErr, err)
	}
	return launchResult{Launched: runErr == nil, Withheld: w.stopped && !w.alarm}, firstErr
}

// failed records a launch failure and returns it as BR-E078.
func (s *Supervisor) failed(entry func(kind, code, msg string) Entry, what_ string, err error) error {
	msg := what_ + ": " + what(err)
	s.logf("%s [%s]", msg, diag.CodeLaunchFailed)
	return s.recordFailure(entry(KindError, diag.CodeLaunchFailed, msg),
		diag.Wrap(err, diag.CodeLaunchFailed, what_, "GitHub refused or could not be reached", "nothing was started; the job is retried a few times"))
}

// deregister removes the runner's registration if the provider still lists it (a just-in-time
// runner normally removes itself after its job, so "already gone" is the usual case and is not an
// error). It returns a note when it could not make sure. Whatever it cannot remove is removed by
// Reconcile at the next start.
func (s *Supervisor) deregister(name string, id int64) string {
	bg, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var last error
	for attempt := 0; attempt < 2; attempt++ {
		last = nil
		runners, err := s.cfg.Provider.ListRunners(bg, s.cfg.Scope)
		if err != nil {
			last = err
			continue
		}
		for _, r := range runners {
			if r.Name == name || (id != 0 && r.ID == id) {
				if err := s.cfg.Provider.RemoveRunner(bg, s.cfg.Scope, r.ID); err != nil {
					last = err
				}
			}
		}
		if last == nil {
			return ""
		}
	}
	msg := fmt.Sprintf("the runner registration %s may remain (%s): it is removed at the next start", name, what(last))
	s.logf("%s [%s]", msg, diag.CodeLaunchFailed)
	return msg
}

// watch polls the queue while the runner has not yet been handed a job. A single-use runner
// takes exactly one job, so the exposure ends once it has one; until then, a new job it could
// take that was not admitted when the runner was started means the runner is stopped, because it
// might be handed that one. A job handed to the runner that is not an admitted one is an alarm.
func (s *Supervisor) watch(ctx context.Context, name string, known map[int64]bool, stop context.CancelFunc, w *watchState) {
	defer close(w.done)
	t := time.NewTicker(s.cfg.WatchInterval)
	defer t.Stop()
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		obs, err := s.scan(ctx, false)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			failures++
			if failures >= watchErrorLimit {
				w.stopped, w.code = true, diag.CodeQueueUnreadable
				w.reason = fmt.Sprintf("the queue could not be read %d times in a row (%s), so what the runner might be given is unknown", failures, what(err))
				stop()
				return
			}
			continue
		}
		assigned := false
		for _, o := range obs {
			if o.Job.RunnerName != name {
				continue
			}
			assigned = true
			if !known[o.Job.ID] {
				w.stopped, w.alarm, w.code = true, true, diag.CodeUnexpectedJob
				w.reason = fmt.Sprintf("job %d of run %d was not admitted", o.Job.ID, o.Run.ID)
				stop()
				return
			}
		}
		if assigned {
			return // single-use: it has its job, and it is an admitted one
		}
		// Admission is not a one-time fact: a key can be revoked, or a commit become unreadable,
		// while the runner waits. So every waiting job it could take is judged again each poll
		// (a commit is fetched once per poll however many jobs share it), not only the new ones.
		j := &judger{s: s, cache: map[string]admission{}}
		cannotJudge := ""
		for _, o := range obs {
			if !stillWaiting(o.Job) || !couldTake(s.labels, o.Job) {
				continue
			}
			if !known[o.Job.ID] {
				w.stopped, w.code = true, diag.CodeLaunchWithheld
				w.reason = fmt.Sprintf("job %d of run %d appeared that this runner could take and is not known to be admitted", o.Job.ID, o.Run.ID)
				stop()
				return
			}
			switch jd := j.judge(ctx, o); jd.Kind {
			case refused:
				w.stopped, w.code = true, diag.CodeLaunchWithheld
				w.reason = fmt.Sprintf("job %d of run %d is no longer admitted: %s", o.Job.ID, o.Run.ID, jd.Reason)
				stop()
				return
			case transient:
				if ctx.Err() != nil {
					return
				}
				cannotJudge = fmt.Sprintf("job %d of run %d could not be judged again: %s", o.Job.ID, o.Run.ID, jd.Reason)
			}
		}
		if cannotJudge != "" {
			failures++
			if failures >= watchErrorLimit {
				w.stopped, w.code = true, diag.CodeQueueUnreadable
				w.reason = fmt.Sprintf("a waiting job could not be judged %d times in a row (%s), so whether the runner may take it is unknown", failures, cannotJudge)
				stop()
				return
			}
			continue
		}
		failures = 0
	}
}
