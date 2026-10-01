package supervisor_test

import (
	"context"
	"strings"
	"testing"

	"github.com/modullar/blade-runner/internal/admit"
	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/supervisor"
	"github.com/modullar/blade-runner/internal/trust"
)

func TestACommitThatCanNeverBeFetchedIsARefusalThatCancelCanClear(t *testing.T) {
	// A force-pushed or deleted commit, or a fork that was deleted, makes GitHub answer 404 or
	// 422 for ever. Treated as "GitHub could not be reached" the job would wedge the queue for
	// good, out of reach of --cancel-unadmitted.
	for name, status := range map[string]int{"not found": 0, "unprocessable": 422} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, withConfig(func(c *supervisor.Config) { c.CancelUnadmitted = true }))
			gone := strings.Repeat("b", 40)
			r.queue(q{run: 2, job: 20, sha: gone})
			if status != 0 {
				r.srv.SetCommitStatus(gone, status)
			}
			r.queue(q{run: 1, job: 10})
			r.rt.OnRun = r.takes(repo, 10, 1)

			out, err := r.sup.Tick(ctx)
			if err != nil || out.Launched || !out.Withheld || len(out.Refused) != 1 || out.Refused[0].JobID != 20 || out.Refused[0].Code != diag.CodeJobRefused {
				t.Fatalf("outcome = %+v, err = %v: a commit that cannot exist is a refusal, not a transient failure", out, err)
			}
			r.mustStartNothing()
			if c := r.cancelRequests(); len(c) != 1 || !strings.HasSuffix(c[0], "/runs/2/cancel") {
				t.Errorf("cancel requests = %v", c)
			}
			if got := r.entriesOfKind(supervisor.KindRefused); len(got) != 1 || got[0].Code != diag.CodeJobRefused {
				t.Errorf("refused entries = %+v", got)
			}
			// Cancelled, it no longer blocks the queue.
			if out, err := r.sup.Tick(ctx); err != nil || !out.Launched || out.JobID != 10 {
				t.Fatalf("after the cancel: %+v, %v", out, err)
			}
		})
	}
}

func TestACommitTheServerCannotAnswerForIsStillOnlyWithheld(t *testing.T) {
	// The other side of the same rule: 5xx and rate limits are "not now", never a refusal (and so
	// never grounds for cancelling someone's run).
	r := newRig(t, withConfig(func(c *supervisor.Config) { c.CancelUnadmitted = true }))
	r.queue(q{run: 1, job: 10})
	r.srv.SetCommitStatus(r.commits["owner"].SHA, 503)
	out, err := r.sup.Tick(ctx)
	if err == nil || diag.CodeOf(err) != diag.CodeQueueUnreadable || len(out.Refused) != 0 || !out.Withheld {
		t.Fatalf("outcome = %+v, err = %v", out, err)
	}
	if len(r.cancelRequests()) != 0 || len(r.entriesOfKind(supervisor.KindRefused)) != 0 {
		t.Error("a GitHub outage was recorded as a refusal or cancelled a run")
	}
}

// answers is an Admitter that gives a fixed answer: what matters here is how the supervisor
// classifies it, not how a commit is verified.
type answers struct{ err error }

func (a answers) Admit(context.Context, admit.Subject) (trust.Verdict, error) {
	return trust.Verdict{}, a.err
}

func TestARefusalIsToldFromAFailureByItsCodeNotByItsWording(t *testing.T) {
	t.Run("a refusal whose text starts like a fetch failure", func(t *testing.T) {
		r := newRig(t, withConfig(func(c *supervisor.Config) {
			c.Admitter = answers{diag.New(diag.CodeNotAdmitted, "cannot fetch commit approval: the signer is not trusted", "x", "y")}
		}))
		r.queue(q{run: 1, job: 10})
		out, err := r.sup.Tick(ctx)
		if err != nil || len(out.Refused) != 1 || out.Refused[0].Code != diag.CodeJobRefused {
			t.Fatalf("outcome = %+v, err = %v: it is a refusal whatever its wording", out, err)
		}
	})
	t.Run("a failure whose text sounds like a refusal", func(t *testing.T) {
		r := newRig(t, withConfig(func(c *supervisor.Config) {
			c.Admitter = answers{diag.New(diag.CodeGitHubUnavailable, "the commit is not admitted", "x", "y")}
		}))
		r.queue(q{run: 1, job: 10})
		out, err := r.sup.Tick(ctx)
		if err == nil || len(out.Refused) != 0 || !out.Withheld {
			t.Fatalf("outcome = %+v, err = %v: a failure is not a refusal whatever its wording", out, err)
		}
	})
}
