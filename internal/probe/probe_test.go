package probe_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/modullar/blade-runner/internal/probe"
	"github.com/modullar/blade-runner/internal/provider"
	"github.com/modullar/blade-runner/internal/testrig"
)

var ctx = context.Background()

func find(fs []probe.Finding, id string) probe.Finding {
	for _, f := range fs {
		if f.ID == id {
			return f
		}
	}
	return probe.Finding{ID: id, Status: "MISSING"}
}

func want(t *testing.T, fs []probe.Finding, id string, st probe.Status, detail string) {
	t.Helper()
	f := find(fs, id)
	if f.Status != st || !strings.Contains(f.Detail, detail) {
		t.Errorf("%s = %s %q, want %s containing %q", id, f.Status, f.Detail, st, detail)
	}
}

func run(t *testing.T, r *testrig.Rig, skipJIT bool) []probe.Finding {
	t.Helper()
	return probe.Run(ctx, r.Env.Provider, probe.Options{Repository: testrig.Target, SkipJIT: skipJIT, NewName: func() string { return "br0-probe-test" }})
}

// goodWorld is a repository that behaves the way the design assumes.
func goodWorld(t *testing.T) *testrig.Rig {
	r := testrig.New(t, testrig.BaseConfig)
	r.ServeCommits(testrig.Target)
	r.Srv.AddRun(testrig.Target, provider.Run{ID: 7, HeadSHA: strings.Repeat("a", 40), Event: "push", Status: "completed", HeadRepository: testrig.Target, Actor: "acme"})
	r.Srv.AddRun(testrig.Target, provider.Run{ID: 8, HeadSHA: strings.Repeat("b", 40), Event: "pull_request", Status: "queued", HeadRepository: "mallory/widgets", Actor: "mallory"})
	r.Srv.AddJob(testrig.Target, provider.Job{ID: 70, RunID: 8, Status: "queued", Labels: []string{"self-hosted"}, HeadSHA: strings.Repeat("b", 40)})
	return r
}

func TestEveryAssumptionHoldsInAWellBehavedRepository(t *testing.T) {
	r := goodWorld(t)
	fs := run(t, r, false)
	for _, id := range []string{"A2", "C1", "C3", "C3b", "C2"} {
		if f := find(fs, id); f.Status != probe.Pass {
			t.Errorf("%s = %s: %s", id, f.Status, f.Detail)
		}
	}
	if probe.Failed(fs) {
		t.Error("Failed() on a healthy repository")
	}
	want(t, fs, "C1", probe.Pass, "SHA256:") // shows the signing keys, so the owner can `trust add` them
	want(t, fs, "C3", probe.Pass, "forks): 1")
	want(t, fs, "H6", probe.Pass, provider.StrictForkApproval)
}

func TestTheTemporaryRunnerIsAlwaysRemoved(t *testing.T) {
	r := goodWorld(t)
	run(t, r, false)
	if left := r.Srv.Runners(testrig.Target); len(left) != 0 {
		t.Errorf("the probe left runners behind on GitHub: %+v", left)
	}
	reqs := strings.Join(r.Srv.Requests(), "\n")
	if !strings.Contains(reqs, "POST /repos/acme/widgets/actions/runners/generate-jitconfig") || !strings.Contains(reqs, "DELETE /repos/acme/widgets/actions/runners/") {
		t.Errorf("expected a registration and a deletion:\n%s", reqs)
	}
}

func TestSkipJITRegistersNothing(t *testing.T) {
	r := goodWorld(t)
	fs := run(t, r, true)
	want(t, fs, "C2", probe.Skip, "--skip-jit")
	for _, req := range r.Srv.Requests() {
		if strings.HasPrefix(req, "POST") || strings.HasPrefix(req, "DELETE") {
			t.Errorf("a changing request was made with --skip-jit: %s", req)
		}
	}
}

func TestTheProbeOnlyReadsExceptForItsOwnTemporaryRunner(t *testing.T) {
	r := goodWorld(t)
	run(t, r, false)
	for _, req := range r.Srv.Requests() {
		if strings.HasPrefix(req, "GET") {
			continue
		}
		if !strings.Contains(req, "generate-jitconfig") && !strings.HasPrefix(req, "DELETE /repos/acme/widgets/actions/runners/") {
			t.Errorf("the probe made a changing request it should not: %s", req)
		}
	}
}

func TestWhatIsNotYetKnownIsSkippedWithInstructions(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig) // no commits, no runs
	fs := run(t, r, false)
	want(t, fs, "C1", probe.Skip, "git commit -S")
	want(t, fs, "C3", probe.Skip, "push a commit")
	if probe.Failed(fs) {
		t.Errorf("a repository that has simply not been used yet is not a failure: %s", probe.Render(fs))
	}
}

func TestC1FailsWhenGitHubHandsBackBytesThatDoNotMatch(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	commits := r.ServeCommits(testrig.Target)
	o := commits["owner"]
	r.Srv.AddCommit(testrig.Target, o.SHA, o.Payload+"tampered", o.Signature) // same id, different bytes
	fs := run(t, r, true)
	want(t, fs, "C1", probe.Fail, "does not recompute")
	if !probe.Failed(fs) {
		t.Error("Failed() must be true")
	}
}

func TestC1FailsWhenThePayloadIsMissing(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	commits := r.ServeCommits(testrig.Target)
	o := commits["owner"]
	r.Srv.AddCommit(testrig.Target, o.SHA, "", o.Signature)
	want(t, run(t, r, true), "C1", probe.Fail, "no payload")
}

func TestC1FailsForGPGOnlyRepositories(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	r.Srv.AddCommit(testrig.Target, strings.Repeat("c", 40), "tree x\n\nm\n", "-----BEGIN PGP SIGNATURE-----\nabc\n-----END PGP SIGNATURE-----")
	want(t, run(t, r, true), "C1", probe.Fail, "GPG")
}

func TestC3FailsWhenARunLacksAField(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	r.Srv.AddRun(testrig.Target, provider.Run{ID: 1, HeadSHA: "", Event: "push", Status: "queued", HeadRepository: testrig.Target, Actor: "acme"})
	want(t, run(t, r, true), "C3", probe.Fail, "missing head_sha: 1")
}

func TestC2FailsWhenJITRunnersAreNotAvailable(t *testing.T) {
	r := goodWorld(t)
	r.Srv.JITUnsupported = true
	fs := run(t, r, false)
	want(t, fs, "C2", probe.Fail, "generate-jitconfig failed")
	if left := r.Srv.Runners(testrig.Target); len(left) != 0 {
		t.Errorf("runners left: %+v", left)
	}
}

// failingRemove is a real provider whose RemoveRunner fails, to prove the probe says plainly that
// a runner was left behind and which one.
type failingRemove struct{ provider.Provider }

func (failingRemove) RemoveRunner(context.Context, provider.Scope, int64) error {
	return errors.New("boom")
}

func TestALeftoverRunnerIsReportedLoudlyWithItsName(t *testing.T) {
	r := goodWorld(t)
	fs := probe.Run(ctx, failingRemove{r.Env.Provider}, probe.Options{Repository: testrig.Target, NewName: func() string { return "br0-probe-left" }})
	want(t, fs, "C2", probe.Fail, `"br0-probe-left" could NOT be removed`)
	want(t, fs, "C2", probe.Fail, "Settings > Actions > Runners")
}

func TestA2FailsWithAnUnusableToken(t *testing.T) {
	r := goodWorld(t)
	if err := r.Env.Secrets.Set(ctx, testrig.RunnerName, "ghp_wrong"); err != nil {
		t.Fatal(err)
	}
	fs := run(t, r, true)
	want(t, fs, "A2", probe.Fail, "BR-E021")
}

func TestForkApprovalUnreadableIsANoteNotAFailure(t *testing.T) {
	r := goodWorld(t)
	r.Srv.ForkApprovalUnsupported = true
	fs := run(t, r, true)
	want(t, fs, "H6", probe.Note, "--fork-approval-confirmed")
	if probe.Failed(fs) {
		t.Error("an unreadable approval setting has a manual fallback and must not fail the probe")
	}
	r.Srv.ForkApprovalUnsupported = false
	r.Srv.ForkApproval = map[string]string{testrig.Target: "first_time_contributors"}
	want(t, run(t, r, true), "H6", probe.Note, "not the strictest")
}

func TestTheReportNeverContainsASecret(t *testing.T) {
	r := goodWorld(t)
	out := probe.Render(run(t, r, false))
	for _, secret := range []string{testrig.Token, "JIT-", "REG-", "Bearer"} {
		if strings.Contains(out, secret) {
			t.Errorf("the report contains %q:\n%s", secret, out)
		}
	}
}
