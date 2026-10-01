package install_test

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/install"
	"github.com/modullar/blade-runner/internal/testrig"
)

// ---- layer 1: workflows -------------------------------------------------------------

func TestApplyRefusesAWorkflowThatLetsAnyoneRunOnTheRunner(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	r.WriteWorkflow("ci.yml", testrig.OpenWorkflow)
	_, _, err := r.Apply()
	if diag.CodeOf(err) != diag.CodeWorkflowPolicy {
		t.Fatalf("err = %v, want BR-E062", err)
	}
	for _, want := range []string{"ci.yml", "job test", "github.actor == 'acme'", "head.repo.full_name == github.repository"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should name %q so the user can fix it:\n%v", want, err)
		}
	}
	assertNothingHappened(t, r)
}

func TestApplyAcceptsTheGuardedWorkflow(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	r.WriteWorkflow("ci.yml", testrig.GuardedWorkflow)
	_, rep, err := r.Apply()
	if err != nil {
		t.Fatalf("a correctly locked workflow must pass: %v", err)
	}
	if !strings.Contains(strings.Join(rep.Notes, "\n"), "1 job(s) in 1 workflow file(s) can run on this runner; all are locked to acme") {
		t.Errorf("notes = %v", rep.Notes)
	}
}

func TestEveryWorkflowFileIsCheckedNotJustTheFirst(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	r.WriteWorkflow("a-good.yml", testrig.GuardedWorkflow)
	r.WriteWorkflow("z-bad.yml", strings.Replace(testrig.OpenWorkflow, "name: ci", "name: sneaky", 1))
	_, _, err := r.Apply()
	if diag.CodeOf(err) != diag.CodeWorkflowPolicy || !strings.Contains(err.Error(), "z-bad.yml") {
		t.Fatalf("a bad file after a good one must still be caught: %v", err)
	}
	assertNothingHappened(t, r)
}

func TestForkTriggerWorkflowsAreRefusedEvenWhenGuarded(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	r.WriteWorkflow("pr.yml", strings.Replace(testrig.GuardedWorkflow, "on: [push, pull_request]", "on: pull_request_target", 1))
	_, _, err := r.Apply()
	if diag.CodeOf(err) != diag.CodeWorkflowPolicy || !strings.Contains(err.Error(), "pull_request_target") {
		t.Fatalf("err = %v, want a refusal naming pull_request_target", err)
	}
}

func TestDryRunEnforcesThePolicyToo(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	r.WriteWorkflow("ci.yml", testrig.OpenWorkflow)
	if _, err := install.ApplyEngine(r.Env, &testrig.Recorder{}).DryRun(ctx); diag.CodeOf(err) != diag.CodeWorkflowPolicy {
		t.Errorf("dry-run err = %v: a plan for an unsafe setup must not be offered", err)
	}
}

func TestHostedOnlyWorkflowsNeedNoGuard(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	r.WriteWorkflow("ci.yml", "on: [push, pull_request, issue_comment]\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - run: x\n")
	if _, _, err := r.Apply(); err != nil {
		t.Fatalf("jobs that never reach this machine need no guard: %v", err)
	}
}

func TestPolicyUsesTheConfiguredTrustedActors(t *testing.T) {
	cfg := strings.Replace(testrig.BaseConfig, "  labels: [gpu]", "  labels: [gpu]\n  trusted_actors: [alice]", 1)
	r := testrig.New(t, cfg)
	r.WriteWorkflow("ci.yml", testrig.GuardedWorkflow) // locked to 'acme', not alice
	_, _, err := r.Apply()
	if diag.CodeOf(err) != diag.CodeWorkflowPolicy || !strings.Contains(err.Error(), "github.actor == 'alice'") {
		t.Fatalf("a guard for the wrong person must not count: %v", err)
	}
	r.WriteWorkflow("ci.yml", strings.ReplaceAll(testrig.GuardedWorkflow, "'acme'", "'alice'"))
	if _, _, err := r.Apply(); err != nil {
		t.Fatalf("locked to alice: %v", err)
	}
}

// ---- layer 2: the checkout is this repository ---------------------------------------

func TestForkedCheckoutCannotInheritSomeoneElsesRunnerSetup(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)       // registers a runner for acme/widgets
	r.GitInit(r.Srv.URL + "/mallory/widgets.git") // ...but this checkout is mallory's fork
	_, _, err := r.Apply()
	if diag.CodeOf(err) != diag.CodeRepoMismatch {
		t.Fatalf("err = %v, want BR-E063", err)
	}
	for _, want := range []string{"mallory/widgets", "acme/widgets", "bladerunner init --force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q:\n%v", want, err)
		}
	}
	assertNothingHappened(t, r)
}

func TestCheckoutOriginFormats(t *testing.T) {
	for _, tc := range []struct {
		name, remote string
		wantErr      bool
	}{
		{"https", "/acme/widgets", false},
		{"https with .git", "/acme/widgets.git", false},
		{"different case", "/ACME/Widgets.git", false},
		{"other owner", "/mallory/widgets", true},
		{"other repo", "/acme/other", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := testrig.New(t, testrig.BaseConfig)
			r.GitInit(r.Srv.URL + tc.remote)
			_, err := r.Env.CheckoutMismatch(ctx)
			if (diag.CodeOf(err) == diag.CodeRepoMismatch) != tc.wantErr {
				t.Errorf("remote %s: err = %v, wantErr %v", tc.remote, err, tc.wantErr)
			}
		})
	}
}

func TestCheckoutCheckIsSkippedWhenItCannotBeMade(t *testing.T) {
	t.Run("not a git checkout", func(t *testing.T) {
		r := testrig.New(t, testrig.BaseConfig)
		note, err := r.Env.CheckoutMismatch(ctx)
		if err != nil || !strings.Contains(note, "not a git checkout") {
			t.Errorf("note=%q err=%v", note, err)
		}
	})
	t.Run("no origin", func(t *testing.T) {
		r := testrig.New(t, testrig.BaseConfig)
		if out, err := exec.Command("git", "-C", r.ProjectDir, "init", "-q").CombinedOutput(); err != nil {
			t.Skipf("git unusable: %s", out)
		}
		note, err := r.Env.CheckoutMismatch(ctx)
		if err != nil || !strings.Contains(note, "no `origin`") {
			t.Errorf("note=%q err=%v", note, err)
		}
	})
	t.Run("another host", func(t *testing.T) {
		r := testrig.New(t, testrig.BaseConfig)
		r.GitInit("git@gitlab.example.com:mallory/widgets.git")
		note, err := r.Env.CheckoutMismatch(ctx)
		if err != nil || !strings.Contains(note, "gitlab.example.com") {
			t.Errorf("note=%q err=%v", note, err)
		}
	})
}

func TestAllowRepoMismatchIsAnExplicitOverride(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	r.GitInit(r.Srv.URL + "/mallory/widgets.git")
	r.Env.AllowRepoMismatch = true
	_, rep, err := r.Apply()
	if err != nil {
		t.Fatalf("override: %v", err)
	}
	if !strings.Contains(strings.Join(rep.Notes, "\n"), "--allow-repo-mismatch") {
		t.Errorf("using the override must be said out loud: %v", rep.Notes)
	}
}

func TestOrganizationScopeComparesTheOwnerOnly(t *testing.T) {
	cfg := strings.Replace(testrig.BaseConfig, "scope: repo\n  repository: acme/widgets", "scope: org\n  organization: acme\n  trusted_actors: [acme]", 1)
	r := testrig.New(t, cfg)
	r.GitInit(r.Srv.URL + "/acme/anything.git")
	if _, err := r.Env.CheckoutMismatch(ctx); err != nil {
		t.Errorf("any repo of the organization is fine: %v", err)
	}
	r2 := testrig.New(t, cfg)
	r2.GitInit(r2.Srv.URL + "/mallory/anything.git")
	if _, err := r2.Env.CheckoutMismatch(ctx); diag.CodeOf(err) != diag.CodeRepoMismatch {
		t.Errorf("another owner must be refused: %v", err)
	}
}

// ---- layer 3: outside contributors on a public repository --------------------------

func publicRig(t *testing.T) *testrig.Rig {
	r := testrig.New(t, testrig.BaseConfig)
	r.Srv.Repos[testrig.Target] = false // public
	r.Env.AllowPublic = true
	return r
}

func TestPublicRepoNeedsStrictForkApproval(t *testing.T) {
	r := publicRig(t)
	if _, _, err := r.Apply(); err != nil {
		t.Fatalf("strict policy plus the opt-in flag should proceed: %v", err)
	}
}

func TestWeakForkApprovalIsRefused(t *testing.T) {
	for _, weak := range []string{"first_time_contributors", "first_time_contributors_new_to_github"} {
		r := publicRig(t)
		r.Srv.ForkApproval = map[string]string{testrig.Target: weak}
		_, _, err := r.Apply()
		if diag.CodeOf(err) != diag.CodeForkApproval || !strings.Contains(err.Error(), weak) {
			t.Errorf("policy %s: err = %v, want BR-E064 naming it", weak, err)
		}
		assertNothingHappened(t, r)
	}
}

func TestUnverifiableForkApprovalFailsClosedUnlessConfirmed(t *testing.T) {
	r := publicRig(t)
	r.Srv.ForkApprovalUnsupported = true
	_, _, err := r.Apply()
	if diag.CodeOf(err) != diag.CodeForkApproval || !strings.Contains(err.Error(), "--fork-approval-confirmed") {
		t.Fatalf("err = %v, want BR-E064 pointing at --fork-approval-confirmed", err)
	}
	assertNothingHappened(t, r)

	r.Env.ForkApprovalConfirmed = true
	if _, _, err := r.Apply(); err != nil {
		t.Errorf("once the user confirms they set it: %v", err)
	}
}

func TestConfirmationFlagCannotOverrideAKnownWeakPolicy(t *testing.T) {
	r := publicRig(t)
	r.Srv.ForkApproval = map[string]string{testrig.Target: "first_time_contributors"}
	r.Env.ForkApprovalConfirmed = true
	if _, _, err := r.Apply(); diag.CodeOf(err) != diag.CodeForkApproval {
		t.Errorf("the API says the policy is weak; a confirmation flag must not outvote it: %v", err)
	}
}

func TestPrivateRepoDoesNotNeedForkApproval(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	r.Srv.ForkApprovalUnsupported = true
	if _, _, err := r.Apply(); err != nil {
		t.Fatalf("private repositories have no outside contributors to approve: %v", err)
	}
	for _, req := range r.Srv.Requests() {
		if strings.Contains(req, "fork-pr-contributor-approval") {
			t.Error("the approval setting was queried for a private repository")
		}
	}
}

// ---- all three layers together -----------------------------------------------------

func TestAnEveryLayerPassingSetupConverges(t *testing.T) {
	r := publicRig(t)
	r.WriteWorkflow("ci.yml", testrig.GuardedWorkflow)
	r.GitInit(r.Srv.URL + "/acme/widgets.git")
	if _, _, err := r.Apply(); err != nil {
		t.Fatalf("guarded workflow + matching checkout + strict approval: %v", err)
	}
	if !r.Plat.Running || len(r.Srv.Runners(testrig.Target)) != 1 {
		t.Errorf("not converged: running=%v runners=%v", r.Plat.Running, r.Srv.Runners(testrig.Target))
	}
	if !filepath.IsAbs(r.ProjectDir) {
		t.Error("ProjectDir should be absolute")
	}
}
