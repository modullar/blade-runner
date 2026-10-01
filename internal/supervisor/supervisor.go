// Package supervisor joins admission and isolation (decision 0006, increment 3): it watches the
// provider's queue and, for a job whose head commit this machine admits, starts ONE ephemeral
// isolated runner for it, then removes the container and the runner's registration. A job that is
// not admitted starts nothing and leaves a durable audit record.
//
// The central hazard is in decision 0007: a just-in-time runner takes ANY waiting job whose
// labels match, not the job it was started for. The supervisor therefore starts a runner only
// while every waiting job that runner could take is admitted, re-checks after registering the
// runner and while it waits, and watches which job the runner is actually given. What remains is
// a race that only the provider can close; 0007 states it and says what BR-0 must verify.
//
// All provider-specific parsing stays in internal/provider/github; this package sees only
// provider.Run and provider.Job.
package supervisor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/modullar/blade-runner/internal/admit"
	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/isolation"
	"github.com/modullar/blade-runner/internal/provider"
	"github.com/modullar/blade-runner/internal/trust"
)

// Runtime is the isolated container runtime (*isolation.Docker satisfies it).
type Runtime interface {
	Run(ctx context.Context, spec isolation.Spec) (isolation.Result, error)
	RemoveStale(ctx context.Context) (int, error)
}

// Admitter decides whether a commit may run here (*admit.Admitter satisfies it).
type Admitter interface {
	Admit(ctx context.Context, s admit.Subject) (trust.Verdict, error)
}

// Defaults for the timings and limits.
const (
	DefaultPollInterval  = 15 * time.Second
	DefaultWatchInterval = 15 * time.Second
	DefaultMaxAttempts   = 3
	// watchErrorLimit is how many provider failures in a row the watcher tolerates before it
	// stops a runner that has not been given a job yet: it can no longer tell what it is exposed to.
	watchErrorLimit = 3
	runnerPrefix    = "br-jit-"
	// runnerIDLen is the length of the random hex id that ends a runner name.
	runnerIDLen = 12
)

// Config is everything the supervisor needs. Collaborators are injected so each is testable with
// the real thing.
type Config struct {
	Provider provider.Provider
	Admitter Admitter
	Runtime  Runtime
	Audit    Recorder
	// Log receives one human-readable line per decision; nil discards.
	Log io.Writer

	Scope provider.Scope // repository scope only in this increment
	// RunnerName names this supervisor's runners: just-in-time runners are called
	// br-jit-<RunnerName>-<random>, and only runners with that prefix are ever removed.
	RunnerName string
	// Labels is the full label set a runner started here carries (self-hosted, the OS and
	// architecture of the CONTAINER, and the configured extras). A job is this runner's business
	// when all of the job's labels are in it.
	Labels []string

	Image            string // pinned by content
	Network          string // "bridge" (default) or "none"
	MemoryMiB        int
	Timeout          time.Duration
	CancelUnadmitted bool
	MaxAttempts      int
	PollInterval     time.Duration
	WatchInterval    time.Duration

	// Sleep waits d or until ctx ends; nil means a real timer. Now is the clock; nil means
	// time.Now. NewID returns a random hex id for runner names; nil means crypto/rand.
	Sleep func(ctx context.Context, d time.Duration)
	NewID func() string
}

// Supervisor is the loop. It keeps only in-memory bookkeeping (what it already recorded, how
// often a job failed to start); everything about the queue and the runners is read from the
// provider every time.
type Supervisor struct {
	cfg    Config
	labels map[string]bool

	noted    map[int64]string // job id -> the last refusal recorded for it
	runNoted map[string]bool  // run-level notes already recorded
	attempts map[int64]int
}

// New validates cfg and returns a Supervisor.
func New(cfg Config) (*Supervisor, error) {
	bad := func(msg string) error {
		return diag.New(diag.CodeConfigInvalid, "the supervisor is misconfigured: "+msg, "a bug in the caller", "fix the configuration")
	}
	switch {
	case cfg.Provider == nil || cfg.Admitter == nil || cfg.Runtime == nil || cfg.Audit == nil:
		return nil, bad("provider, admitter, runtime and audit log are all required")
	case cfg.Scope.Kind != "repo" || !strings.Contains(cfg.Scope.Repository, "/"):
		return nil, diag.New(diag.CodeConfigInvalid, "`supervise` works on a single repository (runner.scope: repo)",
			"organization scope is not supported by the supervisor in this version", "use runner.scope: repo with runner.repository: OWNER/REPO")
	case strings.TrimSpace(cfg.RunnerName) == "":
		return nil, bad("no runner name")
	case len(cfg.Labels) == 0:
		return nil, bad("no labels")
	}
	if _, err := (isolation.Spec{Name: "check", Image: cfg.Image}).Validate(); err != nil {
		return nil, diag.Wrap(err, diag.CodeConfigInvalid, "no usable runner image: "+cfg.Image,
			"the runner image must be pinned by content (name@sha256:<64 hex digits>)",
			"set supervisor.image in bladerunner.yaml or pass --image")
	}
	if cfg.Network == "" {
		cfg.Network = isolation.NetworkBridge
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = DefaultMaxAttempts
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultPollInterval
	}
	if cfg.WatchInterval <= 0 {
		cfg.WatchInterval = DefaultWatchInterval
	}
	if cfg.Sleep == nil {
		cfg.Sleep = func(ctx context.Context, d time.Duration) {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
			case <-t.C:
			}
		}
	}
	if cfg.NewID == nil {
		cfg.NewID = func() string {
			b := make([]byte, 6)
			_, _ = rand.Read(b)
			return hex.EncodeToString(b)
		}
	}
	return &Supervisor{
		cfg: cfg, labels: labelSet(cfg.Labels),
		noted: map[int64]string{}, runNoted: map[string]bool{}, attempts: map[int64]int{},
	}, nil
}

func (s *Supervisor) logf(format string, args ...any) {
	if s.cfg.Log != nil {
		fmt.Fprintf(s.cfg.Log, "supervisor: "+format+"\n", args...)
	}
}

// runnerPrefix is the prefix of every runner (and container) name this supervisor creates.
func (s *Supervisor) runnerPrefix() string { return runnerPrefix + s.cfg.RunnerName + "-" }

// ownRunner reports whether name is exactly a runner this supervisor generates:
// br-jit-<runner name>-<12 lower-case hex digits>. A prefix test is not enough: the supervisor
// "mini-2" names its runners br-jit-mini-2-..., which start with the prefix of "mini".
func (s *Supervisor) ownRunner(name string) bool {
	rest, ok := strings.CutPrefix(name, s.runnerPrefix())
	if !ok || len(rest) != runnerIDLen {
		return false
	}
	for _, c := range rest {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Reconcile brings the world back to a clean state at start-up, from what the provider and the
// container runtime say, never from a file: it removes leftover runner containers and any
// just-in-time runner registration this supervisor created and did not remove (a crash between
// registering and removing leaves one). It is safe to run at any time no supervisor is running.
func (s *Supervisor) Reconcile(ctx context.Context) error {
	removedContainers, err := s.cfg.Runtime.RemoveStale(ctx)
	if err != nil {
		return err
	}
	runners, err := s.cfg.Provider.ListRunners(ctx, s.cfg.Scope)
	if err != nil {
		return queueErr(err, "cannot list the registered runners")
	}
	removedRunners := 0
	for _, r := range runners {
		if !s.ownRunner(r.Name) {
			continue
		}
		if err := s.cfg.Provider.RemoveRunner(ctx, s.cfg.Scope, r.ID); err != nil {
			return diag.Wrap(err, diag.CodeLaunchFailed, "cannot remove the leftover runner registration "+r.Name,
				"GitHub refused", "remove it in the repository's Settings > Actions > Runners")
		}
		removedRunners++
	}
	msg := fmt.Sprintf("started: removed %d leftover container(s) and %d leftover runner registration(s)", removedContainers, removedRunners)
	s.logf("%s", msg)
	return s.cfg.Audit.Record(Entry{Kind: KindStartup, Message: msg, Repository: s.cfg.Scope.Repository})
}

// Run reconciles, then polls until ctx ends. A failed cycle starts nothing and is retried after
// the poll interval; after a launch the next cycle starts at once, so a queue drains quickly.
func (s *Supervisor) Run(ctx context.Context) error {
	if err := s.Reconcile(ctx); err != nil {
		return err
	}
	for ctx.Err() == nil {
		out, err := s.Tick(ctx)
		if err != nil {
			s.logf("%v", err)
			if hasCode(err, diag.CodeAuditFailed) {
				// What cannot be recorded cannot be accounted for: stop, rather than carry on
				// polling and deciding things nobody can read back.
				return err
			}
		}
		if out.Launched && err == nil {
			continue
		}
		s.cfg.Sleep(ctx, s.cfg.PollInterval)
	}
	return nil
}

// Refusal is a job that was refused, as reported to the caller.
type Refusal struct {
	RunID, JobID int64
	Code, Reason string
}

// Outcome is what one cycle did.
type Outcome struct {
	Idle     bool // nothing waiting that this runner could take
	Launched bool // a runner was started (and finished) for an admitted job
	RunID    int64
	JobID    int64
	Withheld bool // something waiting is not admitted or not judgeable: nothing was started
	Refused  []Refusal
}

// assessment is the judged picture of everything waiting.
type assessment struct {
	Waiting   []judgement // every waiting job this runner could take
	Admitted  map[int64]judgement
	Refused   []judgement
	Transient []judgement
}

// clear reports whether a runner may be started: something is waiting and every waiting job is admitted.
func (a assessment) clear() bool {
	return len(a.Waiting) > 0 && len(a.Refused) == 0 && len(a.Transient) == 0
}

// assess reads the queue and judges every waiting job this runner could take.
func (s *Supervisor) assess(ctx context.Context) (assessment, error) {
	obs, err := s.scan(ctx, false)
	if err != nil {
		return assessment{}, err
	}
	a := assessment{Admitted: map[int64]judgement{}}
	j := &judger{s: s, cache: map[string]admission{}}
	for _, o := range obs {
		if !stillWaiting(o.Job) || !couldTake(s.labels, o.Job) {
			continue
		}
		jd := j.judge(ctx, o)
		a.Waiting = append(a.Waiting, jd)
		switch jd.Kind {
		case admitted:
			a.Admitted[o.Job.ID] = jd
		case refused:
			a.Refused = append(a.Refused, jd)
		default:
			a.Transient = append(a.Transient, jd)
		}
	}
	return a, nil
}

// Tick runs one cycle: read the queue, judge, and either refuse/withhold or run one job.
func (s *Supervisor) Tick(ctx context.Context) (Outcome, error) {
	a, err := s.assess(ctx)
	if err != nil {
		return Outcome{Withheld: true}, s.noteError(err)
	}
	if len(a.Waiting) == 0 {
		return Outcome{Idle: true}, nil
	}
	out := Outcome{}
	for _, jd := range a.Refused {
		out.Refused = append(out.Refused, Refusal{RunID: jd.Obs.Run.ID, JobID: jd.Obs.Job.ID, Code: jd.Code, Reason: jd.Reason})
		if err := s.recordRefusal(jd); err != nil {
			return Outcome{Withheld: true}, err
		}
	}
	for _, jd := range a.Admitted {
		delete(s.noted, jd.Obs.Job.ID) // admitted now: a later refusal is news again
	}
	if len(a.Transient) > 0 {
		out.Withheld = true
		for _, jd := range a.Transient {
			s.logf("withheld: cannot judge run %d job %d (%s): %s", jd.Obs.Run.ID, jd.Obs.Job.ID, jd.Code, jd.Reason)
		}
		first := a.Transient[0]
		err := diag.New(diag.CodeQueueUnreadable, fmt.Sprintf("a waiting job could not be judged: %s", first.Reason),
			"GitHub could not be reached for the commit", "nothing was started; the supervisor tries again at the next poll")
		if rerr := s.recordOnce(fmt.Sprintf("transient|%d|%s", first.Obs.Job.ID, first.Reason), Entry{
			Kind: KindWithheld, Code: first.Code, Message: "nothing started: a waiting job could not be judged: " + first.Reason,
			RunID: first.Obs.Run.ID, JobID: first.Obs.Job.ID, Repository: first.Subject.Repository, SHA: first.Subject.SHA,
		}); rerr != nil {
			return out, rerr
		}
		return out, err
	}
	if len(a.Refused) > 0 {
		out.Withheld = true
		return out, s.withhold(ctx, a)
	}

	// The oldest admitted job that is queued (not merely waiting on another job) and has not used
	// up its attempts.
	var cand judgement
	found := false
	for _, jd := range a.Waiting {
		id := jd.Obs.Job.ID
		if jd.Obs.Job.Status != statusQueued || jd.Kind != admitted {
			continue
		}
		if s.attempts[id] >= s.cfg.MaxAttempts {
			if err := s.recordOnce(fmt.Sprintf("gaveup|%d", id), Entry{
				Kind: KindGaveUp, Code: diag.CodeLaunchFailed,
				Message: fmt.Sprintf("a runner for this job failed to start or take it %d times; leaving it alone until the supervisor restarts", s.attempts[id]),
				RunID:   jd.Obs.Run.ID, JobID: id, Repository: jd.Subject.Repository, SHA: jd.Subject.SHA,
			}); err != nil {
				return out, err
			}
			s.logf("gave up on run %d job %d after %d attempts", jd.Obs.Run.ID, id, s.attempts[id])
			continue
		}
		cand, found = jd, true
		break
	}
	if !found {
		out.Idle = true
		return out, nil
	}
	out.RunID, out.JobID = cand.Obs.Run.ID, cand.Obs.Job.ID
	res, err := s.launch(ctx, cand, a)
	out.Launched, out.Withheld = res.Launched, res.Withheld
	return out, err
}

// hasCode reports whether err, or any error joined into it, carries the diag code.
func hasCode(err error, code string) bool {
	if err == nil {
		return false
	}
	var de *diag.Error
	if errors.As(err, &de) && de.Code == code {
		return true
	}
	switch u := err.(type) {
	case interface{ Unwrap() []error }:
		for _, e := range u.Unwrap() {
			if hasCode(e, code) {
				return true
			}
		}
	case interface{ Unwrap() error }:
		return hasCode(u.Unwrap(), code)
	}
	return false
}

// recordFailure writes e, the audit record of a failure whose error is primary, and returns
// primary. If the record cannot be written the audit failure is returned with it: a failure that
// could be neither recorded nor reported must not look like a clean outcome.
func (s *Supervisor) recordFailure(e Entry, primary error) error {
	if err := s.cfg.Audit.Record(e); err != nil {
		return errors.Join(primary, err)
	}
	return primary
}

// refundAttempt takes back one attempt for a launch that was only called off (a job that is not
// admitted was waiting, or the queue could not be read): the admitted job did not fail to start,
// so it must not move towards being given up on.
func (s *Supervisor) refundAttempt(jobID int64) {
	if s.attempts[jobID] > 0 {
		s.attempts[jobID]--
	}
}

func (s *Supervisor) noteError(err error) error {
	if rerr := s.recordOnce("error|"+err.Error(), Entry{Kind: KindError, Code: diag.CodeOf(err), Message: what(err), Repository: s.cfg.Scope.Repository}); rerr != nil {
		return rerr
	}
	return err
}

// recordRefusal writes a refusal once per distinct reason (a job that stays queued is refused on
// every poll, but the audit log is not told every fifteen seconds).
func (s *Supervisor) recordRefusal(jd judgement) error {
	key := jd.Code + "|" + jd.Reason + "|" + jd.Subject.SHA
	if s.noted[jd.Obs.Job.ID] == key {
		return nil
	}
	run, job := jd.Obs.Run, jd.Obs.Job
	msg := fmt.Sprintf("refused: run %d job %d (%s, commit %s in %s): %s", run.ID, job.ID, run.Event, short(run.HeadSHA), run.HeadRepository, jd.Reason)
	s.logf("%s [%s]", msg, jd.Code)
	if err := s.cfg.Audit.Record(Entry{
		Kind: KindRefused, Code: jd.Code, Message: msg,
		Repository: run.HeadRepository, SHA: run.HeadSHA, RunID: run.ID, JobID: job.ID, Event: run.Event, Actor: run.Actor,
	}); err != nil {
		return err
	}
	s.noted[jd.Obs.Job.ID] = key
	return nil
}

// recordOnce records e unless the same key was already recorded.
func (s *Supervisor) recordOnce(key string, e Entry) error {
	if s.runNoted[key] {
		return nil
	}
	if err := s.cfg.Audit.Record(e); err != nil {
		return err
	}
	s.runNoted[key] = true
	return nil
}

// withhold is the mitigation for the shared-queue hazard: while any waiting job this runner
// could take is not admitted, no runner is started. With CancelUnadmitted the refused runs are
// also asked to cancel so they do not block the queue for ever; cancelling is only a request,
// so the next cycle reads the queue again and starts nothing until it is really clear.
func (s *Supervisor) withhold(ctx context.Context, a assessment) error {
	first := a.Refused[0]
	msg := fmt.Sprintf("nothing started: run %d job %d is waiting for a runner like this one and is not admitted (%s); a runner started now could take it",
		first.Obs.Run.ID, first.Obs.Job.ID, first.Code)
	s.logf("withheld [%s]: %s", diag.CodeLaunchWithheld, msg)
	if err := s.recordOnce(fmt.Sprintf("withheld|%d|%s", first.Obs.Job.ID, first.Code), Entry{
		Kind: KindWithheld, Code: diag.CodeLaunchWithheld, Message: msg,
		RunID: first.Obs.Run.ID, JobID: first.Obs.Job.ID, Repository: first.Obs.Run.HeadRepository, SHA: first.Obs.Run.HeadSHA,
	}); err != nil {
		return err
	}
	if !s.cfg.CancelUnadmitted {
		return nil
	}
	done := map[int64]bool{}
	for _, jd := range a.Refused {
		id := jd.Obs.Run.ID
		if done[id] {
			continue
		}
		done[id] = true
		cerr := s.cfg.Provider.CancelRun(ctx, s.cfg.Scope.Repository, id)
		text := fmt.Sprintf("asked GitHub to cancel refused run %d", id)
		code := ""
		if cerr != nil {
			text = fmt.Sprintf("could not ask GitHub to cancel refused run %d: %s", id, what(cerr))
			code = diag.CodeLaunchFailed
		}
		s.logf("%s", text)
		if err := s.recordOnce(fmt.Sprintf("cancel|%d|%v", id, cerr != nil), Entry{
			Kind: KindCancel, Code: code, Message: text, RunID: id, Repository: jd.Obs.Run.HeadRepository, SHA: jd.Obs.Run.HeadSHA, Event: jd.Obs.Run.Event,
		}); err != nil {
			return err
		}
	}
	return nil
}
