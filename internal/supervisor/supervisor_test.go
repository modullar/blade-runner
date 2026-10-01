package supervisor_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/isolation"
	"github.com/modullar/blade-runner/internal/provider"
	"github.com/modullar/blade-runner/internal/supervisor"
	"github.com/modullar/blade-runner/internal/trust"
)

// ---- the happy path -----------------------------------------------------------------------

func TestAdmittedJobGetsOneEphemeralIsolatedRunner(t *testing.T) {
	r := newRig(t)
	sha := r.queue(q{run: 1, job: 10})
	registeredDuringRun := false
	r.rt.OnRun = func(c context.Context, spec isolation.Spec) (isolation.Result, error) {
		for _, rn := range r.srv.Runners(repo) {
			if rn.Name == spec.Name {
				registeredDuringRun = true
			}
		}
		return r.takes(repo, 10, 1)(c, spec)
	}

	out, err := r.sup.Tick(ctx)
	if err != nil || !out.Launched || out.RunID != 1 || out.JobID != 10 {
		t.Fatalf("outcome = %+v, err = %v", out, err)
	}

	specs := r.rt.Specs()
	if len(specs) != 1 {
		t.Fatalf("%d containers started, want exactly one", len(specs))
	}
	s := specs[0]
	if s.Image != testImage {
		t.Errorf("image = %q, want the one pinned by digest in the config", s.Image)
	}
	if !strings.HasPrefix(s.Stdin, "JIT-") {
		t.Errorf("the just-in-time config did not reach the container's standard input: %q", s.Stdin)
	}
	if len(s.Env) != 0 {
		t.Errorf("environment = %v: the container must get no environment (a secret there is visible to docker inspect)", s.Env)
	}
	for _, a := range s.Args {
		if strings.Contains(a, "JIT-") {
			t.Errorf("the config is on the command line: %q", a)
		}
	}
	if s.Network != isolation.NetworkBridge {
		t.Errorf("network = %q: a runner must reach GitHub", s.Network)
	}
	if !strings.HasPrefix(s.Name, "br-jit-mini-") {
		t.Errorf("runner/container name = %q", s.Name)
	}
	if !registeredDuringRun {
		t.Error("the runner was not registered while its container ran")
	}
	if rs := r.srv.Runners(repo); len(rs) != 0 {
		t.Errorf("the registration was not removed afterwards: %+v", rs)
	}

	// The audit trail says who vouched, before and after.
	launching := r.entriesOfKind(supervisor.KindLaunching)
	finished := r.entriesOfKind(supervisor.KindFinished)
	if len(launching) != 1 || len(finished) != 1 {
		t.Fatalf("audit entries: %+v", r.entries())
	}
	l := launching[0]
	if l.Signer != "owner" || l.Fingerprint == "" || l.SHA != sha || l.Repository != repo || l.RunID != 1 || l.JobID != 10 || l.Runner != s.Name {
		t.Errorf("launching entry = %+v", l)
	}
	f := finished[0]
	if f.ExitCode == nil || *f.ExitCode != 0 || len(f.JobsRun) != 1 || f.JobsRun[0] != 10 {
		t.Errorf("finished entry = %+v", f)
	}
	raw, _ := os.ReadFile(r.auditPath)
	if strings.Contains(string(raw), "JIT-") || strings.Contains(r.log.String(), "JIT-") {
		t.Error("the just-in-time config leaked into the audit log or the log")
	}
	if n, err := supervisor.VerifyAuditLog(r.auditPath); err != nil || n != 2 {
		t.Errorf("audit chain: %d entries, %v", n, err)
	}

	// The job is done: the next cycle has nothing to do and starts nothing.
	out, err = r.sup.Tick(ctx)
	if err != nil || !out.Idle || len(r.rt.Specs()) != 1 {
		t.Errorf("second tick = %+v, %v, containers %d", out, err, len(r.rt.Specs()))
	}
}

func TestPullRequestFromForkIsAdmittedOnItsHeadCommitInTheForksRepository(t *testing.T) {
	r := newRig(t)
	sha := r.queue(q{run: 2, job: 20, event: "pull_request", headRepo: fork,
		prs: []provider.PullRequest{{Number: 7, HeadSHA: r.commits["owner"].SHA, HeadRepository: fork}}})
	r.rt.OnRun = r.takes(repo, 20, 2)

	out, err := r.sup.Tick(ctx)
	if err != nil || !out.Launched {
		t.Fatalf("outcome = %+v, err = %v", out, err)
	}
	var asked []string
	for _, req := range r.srv.Requests() {
		if strings.Contains(req, "/git/commits/") {
			asked = append(asked, req)
		}
	}
	// (Twice: when judging, and again after the runner is registered.)
	if len(asked) == 0 {
		t.Fatal("no commit was fetched")
	}
	for _, a := range asked {
		if a != "GET /repos/"+fork+"/git/commits/"+sha {
			t.Errorf("commit requests = %v: the head commit must be fetched from the head repository", asked)
		}
	}
}

func TestOldestAdmittedJobFirstAndOnlyOneRunnerPerCycle(t *testing.T) {
	r := newRig(t)
	r.queue(q{run: 5, job: 50})
	r.queue(q{run: 3, job: 30})
	r.queue(q{run: 4, job: 40})
	var order []int64
	r.rt.OnRun = func(c context.Context, spec isolation.Spec) (isolation.Result, error) {
		// The runner is handed the oldest waiting job.
		var id, run int64
		switch len(order) {
		case 0:
			id, run = 30, 3
		case 1:
			id, run = 40, 4
		default:
			id, run = 50, 5
		}
		order = append(order, id)
		return r.takes(repo, id, run)(c, spec)
	}
	for i := 1; i <= 3; i++ {
		if _, err := r.sup.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if got := len(r.rt.Specs()); got != i {
			t.Fatalf("after cycle %d, %d containers started: exactly one runner per cycle, never two at once", i, got)
		}
	}
	var launched []int64
	for _, e := range r.entriesOfKind(supervisor.KindLaunching) {
		launched = append(launched, e.JobID)
	}
	if len(launched) != 3 || launched[0] != 30 || launched[1] != 40 || launched[2] != 50 {
		t.Errorf("launch order = %v, want oldest first", launched)
	}
}

func TestJobsOfOtherRunnersAreNotThisRunnersBusiness(t *testing.T) {
	r := newRig(t)
	// Hosted jobs and jobs needing a label this runner lacks are neither run nor a threat, even
	// when their commits are unsigned: no runner of ours could be handed them.
	r.queue(q{run: 1, job: 10, who: "unsigned", labels: []string{"ubuntu-latest"}})
	r.queue(q{run: 2, job: 20, who: "mallory", labels: []string{"self-hosted", "windows"}})
	r.queue(q{run: 3, job: 30, who: "mallory", labels: []string{"self-hosted", "gpu", "extra-label-we-lack"}})
	out, err := r.sup.Tick(ctx)
	if err != nil || !out.Idle {
		t.Fatalf("outcome = %+v, err = %v", out, err)
	}
	r.mustStartNothing()
	if len(r.entries()) != 0 {
		t.Errorf("audit entries for jobs that are not ours: %+v", r.entries())
	}
}

func TestLabelsMatchLikeGitHubDoes(t *testing.T) {
	r := newRig(t)
	// Case-insensitive, and a job needing a subset of the runner's labels matches.
	r.queue(q{run: 1, job: 10, labels: []string{"SELF-HOSTED", "linux", "X64"}})
	r.rt.OnRun = r.takes(repo, 10, 1)
	if out, err := r.sup.Tick(ctx); err != nil || !out.Launched {
		t.Fatalf("outcome = %+v, err = %v", out, err)
	}
}

// ---- every refusal path -------------------------------------------------------------------

func TestRefusedJobsStartNothingAndLeaveAnAuditRecord(t *testing.T) {
	tests := []struct {
		name     string
		queue    q
		revoke   bool
		code     string
		contains string
	}{
		{"unsigned commit", q{run: 1, job: 10, who: "unsigned"}, false, diag.CodeJobRefused, "not signed"},
		{"signed by a key nobody trusted", q{run: 1, job: 10, who: "mallory"}, false, diag.CodeJobRefused, "not"},
		{"signer revoked", q{run: 1, job: 10}, true, diag.CodeJobRefused, "revoked"},
		{"pull request whose head is unsigned", q{run: 1, job: 10, event: "pull_request", who: "unsigned", headRepo: fork}, false, diag.CodeJobRefused, "not signed"},
		{"pull request signed by a stranger's key", q{run: 1, job: 10, event: "pull_request", who: "mallory", headRepo: fork}, false, diag.CodeJobRefused, "not"},
		{"pull_request_target", q{run: 1, job: 10, event: "pull_request_target"}, false, diag.CodeJobEventRefused, "pull_request_target"},
		{"issue_comment", q{run: 1, job: 10, event: "issue_comment"}, false, diag.CodeJobEventRefused, "issue_comment"},
		{"workflow_run", q{run: 1, job: 10, event: "workflow_run"}, false, diag.CodeJobEventRefused, "workflow_run"},
		{"no event", q{run: 1, job: 10, event: "-"}, false, diag.CodeJobAmbiguous, "no event"},
		{"no head repository", q{run: 1, job: 10, noRepo: true}, false, diag.CodeJobAmbiguous, "repository"},
		{"head commit is not a full id", q{run: 1, job: 10, sha: "abc123"}, false, diag.CodeJobAmbiguous, "40-digit"},
		{"head commit is empty", q{run: 1, job: 10, sha: "-"}, false, diag.CodeJobAmbiguous, "40-digit"},
		{"job names no labels", q{run: 1, job: 10, labels: []string{}}, false, diag.CodeJobAmbiguous, "no labels"},
		{"job's commit differs from its run's", q{run: 1, job: 10, jobSHA: strings.Repeat("c", 40)}, false, diag.CodeJobAmbiguous, "differs"},
		{"push whose commit lives in another repository", q{run: 1, job: 10, headRepo: fork}, false, diag.CodeJobAmbiguous, "not in"},
		{"job has no status", q{run: 1, job: 10, status: "-"}, false, diag.CodeJobAmbiguous, "no status"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			if tc.revoke {
				if _, err := r.trust.Revoke("owner"); err != nil {
					t.Fatal(err)
				}
			}
			x := tc.queue
			// "-" asks for an empty field (zero values mean "use the default").
			if x.event == "-" {
				r.srv.AddRun(repo, provider.Run{ID: x.run, HeadSHA: r.commits["owner"].SHA, Event: "", Status: "queued", HeadRepository: repo})
				r.srv.AddJob(repo, provider.Job{ID: x.job, RunID: x.run, Status: "queued", Labels: []string{"self-hosted"}})
			} else if x.status == "-" {
				r.srv.AddRun(repo, provider.Run{ID: x.run, HeadSHA: r.commits["owner"].SHA, Event: "push", Status: "queued", HeadRepository: repo})
				r.srv.AddJob(repo, provider.Job{ID: x.job, RunID: x.run, Status: "", Labels: []string{"self-hosted"}})
			} else {
				if x.sha == "-" {
					r.srv.AddRun(repo, provider.Run{ID: x.run, HeadSHA: "", Event: "push", Status: "queued", HeadRepository: repo})
					r.srv.AddJob(repo, provider.Job{ID: x.job, RunID: x.run, Status: "queued", Labels: []string{"self-hosted"}})
				} else {
					r.queue(x)
				}
			}
			out, err := r.sup.Tick(ctx)
			if err != nil {
				t.Fatalf("a refusal is correct behaviour, not a failure: %v", err)
			}
			if len(out.Refused) != 1 || out.Refused[0].Code != tc.code || !out.Withheld || out.Launched {
				t.Fatalf("outcome = %+v, want one refusal with %s", out, tc.code)
			}
			if !strings.Contains(out.Refused[0].Reason, tc.contains) {
				t.Errorf("reason %q should mention %q", out.Refused[0].Reason, tc.contains)
			}
			r.mustStartNothing()

			refusals := r.entriesOfKind(supervisor.KindRefused)
			if len(refusals) != 1 {
				t.Fatalf("audit: %+v", r.entries())
			}
			e := refusals[0]
			if e.Code != tc.code || e.RunID != 1 || e.JobID != 10 || e.Message == "" {
				t.Errorf("audit entry = %+v", e)
			}
			if !strings.Contains(r.log.String(), tc.code) {
				t.Errorf("the log does not show the diag code %s:\n%s", tc.code, r.log.String())
			}

			// Asked again at the next poll, the answer is the same and is not recorded twice.
			if _, err := r.sup.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			if n := len(r.entriesOfKind(supervisor.KindRefused)); n != 1 {
				t.Errorf("%d refusal records after two polls, want 1", n)
			}
			r.mustStartNothing()
		})
	}
}

func TestAJobThatClaimsAnotherRunIsRefused(t *testing.T) {
	r := newRig(t)
	r.queue(q{run: 1, job: 10})
	r.srv.JobRunIDOffset = 5
	out, err := r.sup.Tick(ctx)
	if err != nil || len(out.Refused) != 1 || out.Refused[0].Code != diag.CodeJobAmbiguous || !strings.Contains(out.Refused[0].Reason, "claims run") {
		t.Fatalf("outcome = %+v, err = %v", out, err)
	}
	r.mustStartNothing()
}

func TestExpiredGrantIsRefused(t *testing.T) {
	r := newRig(t)
	k, err := trust.ParsePublicKey(r.pub["mallory"])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.trust.Add("mallory", k, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	r.queue(q{run: 1, job: 10, who: "mallory"})
	r.rt.OnRun = r.takes(repo, 10, 1)
	if out, err := r.sup.Tick(ctx); err != nil || !out.Launched {
		t.Fatalf("while the grant is valid: %+v, %v", out, err)
	}
	// Time passes beyond the expiry: the same signer is refused.
	r.trust.Now = func() time.Time { return time.Now().Add(48 * time.Hour) }
	r.queue(q{run: 2, job: 20, who: "mallory"})
	out, err := r.sup.Tick(ctx)
	if err != nil || len(out.Refused) != 1 || out.Refused[0].Code != diag.CodeJobRefused || !strings.Contains(out.Refused[0].Reason, "expired") {
		t.Fatalf("outcome = %+v, err = %v", out, err)
	}
}

func TestPullRequestMergeCommitIsNeverVerified(t *testing.T) {
	r := newRig(t)
	// GitHub (assumption C3 wrong) reports the merge commit as the run's head: nobody signed it.
	// The pull request itself names the owner's signed head. The two disagree, so the
	// supervisor does not guess which one is the code: refused, nothing started.
	merge := r.commits["unsigned"].SHA
	r.queue(q{run: 1, job: 10, event: "pull_request", headRepo: fork, sha: merge,
		prs: []provider.PullRequest{{Number: 4, HeadSHA: r.commits["owner"].SHA, HeadRepository: fork}}})
	out, err := r.sup.Tick(ctx)
	if err != nil || len(out.Refused) != 1 || out.Refused[0].Code != diag.CodeJobAmbiguous || !strings.Contains(out.Refused[0].Reason, "pull request #4") {
		t.Fatalf("outcome = %+v, err = %v", out, err)
	}
	r.mustStartNothing()
	for _, req := range r.srv.Requests() {
		if strings.Contains(req, merge) {
			t.Errorf("the supervisor fetched the merge commit: %s", req)
		}
	}
}

func TestPullRequestRepositoryMismatchIsRefused(t *testing.T) {
	r := newRig(t)
	r.queue(q{run: 1, job: 10, event: "pull_request", headRepo: fork,
		prs: []provider.PullRequest{{Number: 4, HeadSHA: r.commits["owner"].SHA, HeadRepository: "other/repo"}}})
	out, err := r.sup.Tick(ctx)
	if err != nil || len(out.Refused) != 1 || out.Refused[0].Code != diag.CodeJobAmbiguous {
		t.Fatalf("outcome = %+v, err = %v", out, err)
	}
	r.mustStartNothing()
}

func TestADishonestServerCannotChooseWhatIsVerified(t *testing.T) {
	r := newRig(t)
	// For the OWNER's commit id the server returns MALLORY's signed bytes: the id recomputed
	// from them is not the one asked for, so nothing is admitted, whatever signed them.
	m, o := r.commits["mallory"], r.commits["owner"]
	r.srv.AddCommit(repo, o.SHA, m.Payload, m.Signature)
	r.queue(q{run: 1, job: 10})
	out, err := r.sup.Tick(ctx)
	if err != nil || len(out.Refused) != 1 || out.Refused[0].Code != diag.CodeJobRefused || !strings.Contains(out.Refused[0].Reason, "hash") {
		t.Fatalf("outcome = %+v, err = %v", out, err)
	}
	r.mustStartNothing()
}

// ---- fail closed on any API error ---------------------------------------------------------

func TestProviderFailuresStartNothing(t *testing.T) {
	cases := map[string]func(r *rig){
		"GitHub is down":              func(r *rig) { r.srv.Down = true },
		"the run list fails":          func(r *rig) { r.srv.FailPathContains = "/actions/runs" },
		"the job list of a run fails": func(r *rig) { r.srv.FailPathContains = "/jobs" },
		"the commit cannot be read":   func(r *rig) { r.srv.FailPathContains = "/git/commits/" },
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.queue(q{run: 1, job: 10})
			breakIt(r)
			out, err := r.sup.Tick(ctx)
			if err == nil || out.Launched {
				t.Fatalf("outcome = %+v, err = %v: an error must stop everything", out, err)
			}
			if code := diag.CodeOf(err); code != diag.CodeQueueUnreadable {
				t.Errorf("code = %s, want %s: %v", code, diag.CodeQueueUnreadable, err)
			}
			if !out.Withheld {
				t.Errorf("outcome = %+v, want withheld", out)
			}
			r.srv.Down = false
			r.srv.FailPathContains = ""
			r.mustStartNothing()
			if n := len(r.entriesOfKind(supervisor.KindRefused)); n != 0 {
				t.Errorf("an API error was recorded as a refusal of the commit (%d)", n)
			}
			// And once GitHub answers again, the job is admitted: the error was not remembered.
			r.rt.OnRun = r.takes(repo, 10, 1)
			if out, err := r.sup.Tick(ctx); err != nil || !out.Launched {
				t.Errorf("after recovery: %+v, %v", out, err)
			}
		})
	}
}

func TestMissingJITConfigStartsNothing(t *testing.T) {
	r := newRig(t)
	r.queue(q{run: 1, job: 10})
	r.srv.JITUnsupported = true
	out, err := r.sup.Tick(ctx)
	if err == nil || out.Launched || diag.CodeOf(err) != diag.CodeLaunchFailed {
		t.Fatalf("outcome = %+v, err = %v, want BR-E078", out, err)
	}
	if len(r.rt.Specs()) != 0 {
		t.Error("a container was started without a runner registration")
	}
}

func TestRunnerWhoseContainerFailsIsCleanedUpAndGivenUpOn(t *testing.T) {
	r := newRig(t)
	r.queue(q{run: 1, job: 10})
	r.rt.OnRun = func(context.Context, isolation.Spec) (isolation.Result, error) {
		return isolation.Result{}, diag.New(diag.CodeIsolation, "docker said no", "x", "y")
	}
	for i := 0; i < 3; i++ {
		_, err := r.sup.Tick(ctx)
		if diag.CodeOf(err) != diag.CodeLaunchFailed {
			t.Fatalf("attempt %d: err = %v", i+1, err)
		}
		if rs := r.srv.Runners(repo); len(rs) != 0 {
			t.Fatalf("attempt %d left a registration behind: %+v", i+1, rs)
		}
	}
	// Three failures: it is left alone, not retried in a loop that burns API calls and CPU.
	out, err := r.sup.Tick(ctx)
	if err != nil || !out.Idle || len(r.rt.Specs()) != 3 {
		t.Fatalf("after giving up: %+v, %v, containers %d", out, err, len(r.rt.Specs()))
	}
	if n := len(r.entriesOfKind(supervisor.KindGaveUp)); n != 1 {
		t.Errorf("gave_up records = %d", n)
	}
	// A restart forgets, and tries again.
	r.rt.OnRun = r.takes(repo, 10, 1)
	s2 := r.newSupervisor()
	if out, err := s2.Tick(ctx); err != nil || !out.Launched {
		t.Fatalf("after restart: %+v, %v", out, err)
	}
}

func TestRunnerThatTakesNothingIsNotCountedAsSuccess(t *testing.T) {
	r := newRig(t)
	r.queue(q{run: 1, job: 10})
	if out, err := r.sup.Tick(ctx); err != nil || !out.Launched {
		t.Fatalf("outcome = %+v, err = %v", out, err)
	}
	f := r.entriesOfKind(supervisor.KindFinished)
	if len(f) != 1 || len(f[0].JobsRun) != 0 || !strings.Contains(f[0].Message, "without taking any job") {
		t.Errorf("finished = %+v", f)
	}
}

// ---- configuration ------------------------------------------------------------------------

func TestNewRefusesUnsafeConfiguration(t *testing.T) {
	r := newRig(t)
	good := r.cfg
	good.Provider = r.prov
	for name, edit := range map[string]func(c *supervisor.Config){
		"an image tag that can move":     func(c *supervisor.Config) { c.Image = "ghcr.io/example/runner:latest" },
		"no image":                       func(c *supervisor.Config) { c.Image = "" },
		"organization scope":             func(c *supervisor.Config) { c.Scope = provider.Scope{Kind: "org", Organization: "acme"} },
		"no labels":                      func(c *supervisor.Config) { c.Labels = nil },
		"no runner name":                 func(c *supervisor.Config) { c.RunnerName = "" },
		"no admitter":                    func(c *supervisor.Config) { c.Admitter = nil },
		"no audit log":                   func(c *supervisor.Config) { c.Audit = nil },
		"no runtime":                     func(c *supervisor.Config) { c.Runtime = nil },
		"no provider":                    func(c *supervisor.Config) { c.Provider = nil },
		"repository that is not OWNER/R": func(c *supervisor.Config) { c.Scope = provider.Scope{Kind: "repo", Repository: "widgets"} },
	} {
		t.Run(name, func(t *testing.T) {
			c := good
			edit(&c)
			if _, err := supervisor.New(c); diag.CodeOf(err) != diag.CodeConfigInvalid {
				t.Errorf("err = %v, want BR-E001", err)
			}
		})
	}
}

// ---- the audit log is a hard dependency ---------------------------------------------------

type failingRecorder struct {
	err  error
	kind string // fail only entries of this kind; "" fails every entry
}

func (f failingRecorder) Record(e supervisor.Entry) error {
	if f.kind == "" || f.kind == e.Kind {
		return f.err
	}
	return nil
}

func TestNothingStartsIfItCannotBeRecorded(t *testing.T) {
	auditErr := diag.New(diag.CodeAuditFailed, "disk full", "x", "y")
	t.Run("a launch", func(t *testing.T) {
		r := newRig(t, withConfig(func(c *supervisor.Config) { c.Audit = failingRecorder{err: auditErr} }))
		r.queue(q{run: 1, job: 10})
		_, err := r.sup.Tick(ctx)
		if diag.CodeOf(err) != diag.CodeAuditFailed {
			t.Fatalf("err = %v, want BR-E079", err)
		}
		r.mustStartNothing()
	})
	t.Run("a refusal", func(t *testing.T) {
		// Only the refusal record fails: the later "withheld" record would succeed.
		r := newRig(t, withConfig(func(c *supervisor.Config) { c.Audit = failingRecorder{err: auditErr, kind: supervisor.KindRefused} }))
		r.queue(q{run: 1, job: 10, who: "unsigned"})
		_, err := r.sup.Tick(ctx)
		if diag.CodeOf(err) != diag.CodeAuditFailed {
			t.Fatalf("err = %v, want BR-E079", err)
		}
		r.mustStartNothing()
	})
}

// ---- crash and restart: state comes from the provider, never from disk --------------------

func TestRestartRemovesLeftoverRegistrationsButNotOtherRunners(t *testing.T) {
	r := newRig(t)
	// A crash between registering a single-use runner and removing it leaves it registered.
	if _, err := r.client.GenerateJITConfig(ctx, r.cfg.Scope, "br-jit-mini-deadbeef0001", []string{"gpu"}); err != nil {
		t.Fatal(err)
	}
	// The owner's permanent runner, and another supervisor's runner, are not ours to remove.
	r.srv.AddRunner(repo, "mini", []string{"self-hosted"}, true)
	r.srv.AddRunner(repo, "br-jit-other-0001", []string{"self-hosted"}, false)

	if err := r.sup.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, rn := range r.srv.Runners(repo) {
		names = append(names, rn.Name)
	}
	if len(names) != 2 || names[0] != "mini" || names[1] != "br-jit-other-0001" {
		t.Errorf("runners after reconcile = %v", names)
	}
	if r.rt.removes != 1 {
		t.Errorf("stale containers were not removed (%d calls)", r.rt.removes)
	}
	if st := r.entriesOfKind(supervisor.KindStartup); len(st) != 1 || !strings.Contains(st[0].Message, "1 leftover runner") {
		t.Errorf("startup entry = %+v", st)
	}
}

func TestRestartNeverRunsAJobTwice(t *testing.T) {
	r := newRig(t)
	r.queue(q{run: 1, job: 10})
	r.rt.OnRun = r.takes(repo, 10, 1)
	if out, err := r.sup.Tick(ctx); err != nil || !out.Launched {
		t.Fatalf("first supervisor: %+v, %v", out, err)
	}
	// "Crash and restart": a brand new supervisor with no memory, and a reopened audit log.
	r.audit.Close()
	audit2, err := supervisor.OpenAuditLog(r.auditPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer audit2.Close()
	r.cfg.Audit = audit2
	s2 := r.newSupervisor()
	if err := s2.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	out, err := s2.Tick(ctx)
	if err != nil || !out.Idle || len(r.rt.Specs()) != 1 {
		t.Fatalf("restarted supervisor: %+v, %v, containers %d: the provider says the job is done, so nothing runs", out, err, len(r.rt.Specs()))
	}
	if n, err := supervisor.VerifyAuditLog(r.auditPath); err != nil || n != 3 {
		t.Errorf("chain across the restart: %d entries, %v", n, err)
	}
}

func TestRestartAfterCrashMidRunStillRunsTheStillQueuedJobOnce(t *testing.T) {
	r := newRig(t)
	r.queue(q{run: 1, job: 10})
	// The first supervisor dies while its container runs: the runner registration stays, the job
	// is still queued at GitHub.
	boom := errors.New("supervisor killed")
	r.rt.OnRun = func(context.Context, isolation.Spec) (isolation.Result, error) { return isolation.Result{}, boom }
	_, _ = r.sup.Tick(ctx)
	// Simulate that the kill also prevented the cleanup: re-register what it left behind.
	if _, err := r.client.GenerateJITConfig(ctx, r.cfg.Scope, "br-jit-mini-cafe00000001", []string{"gpu"}); err != nil {
		t.Fatal(err)
	}

	s2 := r.newSupervisor()
	r.rt.OnRun = r.takes(repo, 10, 1)
	if err := s2.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if rs := r.srv.Runners(repo); len(rs) != 0 {
		t.Fatalf("leftover registration survived the restart: %+v", rs)
	}
	if out, err := s2.Tick(ctx); err != nil || !out.Launched {
		t.Fatalf("after restart: %+v, %v", out, err)
	}
	if out, err := s2.Tick(ctx); err != nil || !out.Idle {
		t.Fatalf("once done it is not run again: %+v, %v", out, err)
	}
}

func TestRunLoopStopsWhenContextEnds(t *testing.T) {
	r := newRig(t)
	r.queue(q{run: 1, job: 10})
	r.rt.OnRun = r.takes(repo, 10, 1)
	c, cancel := context.WithCancel(ctx)
	sleeps := 0
	r.cfg.Sleep = func(context.Context, time.Duration) {
		sleeps++
		if sleeps == 2 {
			cancel()
		}
	}
	s := r.newSupervisor()
	if err := s.Run(c); err != nil {
		t.Fatal(err)
	}
	if len(r.rt.Specs()) != 1 || sleeps < 2 {
		t.Errorf("containers %d, sleeps %d", len(r.rt.Specs()), sleeps)
	}
}
