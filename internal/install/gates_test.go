package install_test

import (
	"context"
	"github.com/modullar/blade-runner/internal/testrig"
	"strings"
	"testing"

	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/install"
	"github.com/modullar/blade-runner/internal/platform"
	"github.com/modullar/blade-runner/internal/provider"
)

// assertNothingHappened checks a refused run left the machine untouched.
func assertNothingHappened(t *testing.T, r *testrig.Rig) {
	t.Helper()
	if r.Exists(".bladerunner/runners") || r.Exists(".bladerunner/work") {
		t.Error("files were created although a gate refused the run")
	}
	if r.Plat.Installs != 0 || r.Plat.Starts != 0 {
		t.Error("the service was touched although a gate refused the run")
	}
	if len(r.Srv.Runners(testrig.Target)) != 0 {
		t.Error("a runner was registered although a gate refused the run")
	}
	for _, req := range r.Srv.Requests() {
		if strings.HasPrefix(req, "POST") || strings.HasPrefix(req, "GET /download") {
			t.Errorf("a mutating or downloading request was made: %s", req)
		}
	}
}

func TestRefusesToRunAsRoot(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	r.Env.Euid = func() int { return 0 }
	_, _, err := r.Apply()
	if diag.CodeOf(err) != diag.CodeRunningAsRoot {
		t.Fatalf("err = %v, want BR-E012", err)
	}
	assertNothingHappened(t, r)
}

func TestMissingPrerequisitesAreListedWithTheirFixes(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	r.Plat.Prereqs = []platform.Prereq{
		{Name: "git", OK: true},
		{Name: "Xcode Command Line Tools", OK: false, Fix: "xcode-select --install"},
		{Name: "GUI login session", OK: false, Fix: "log in to the desktop"},
	}
	_, _, err := r.Apply()
	if diag.CodeOf(err) != diag.CodePrereqMissing {
		t.Fatalf("err = %v, want BR-E010", err)
	}
	for _, want := range []string{"Xcode Command Line Tools", "GUI login session", "xcode-select --install", "log in to the desktop"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "git:") {
		t.Errorf("a passing prerequisite must not be listed:\n%v", err)
	}
	assertNothingHappened(t, r)
}

func TestMissingTokenStopsBeforeAnyChange(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	if err := r.Env.Secrets.Delete(ctx, testrig.RunnerName); err != nil {
		t.Fatal(err)
	}
	_, _, err := r.Apply()
	if diag.CodeOf(err) != diag.CodeTokenMissing {
		t.Fatalf("err = %v, want BR-E020", err)
	}
	assertNothingHappened(t, r)
}

func TestRejectedTokenStopsBeforeAnyChange(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	if err := r.Env.Secrets.Set(ctx, testrig.RunnerName, "ghp_wrong"); err != nil {
		t.Fatal(err)
	}
	_, _, err := r.Apply()
	if diag.CodeOf(err) != diag.CodeTokenRejected {
		t.Fatalf("err = %v, want BR-E021", err)
	}
	if strings.Contains(err.Error(), "ghp_wrong") {
		t.Errorf("the token leaked into the error:\n%v", err)
	}
	assertNothingHappened(t, r)
}

func TestGitHubDownStopsBeforeAnyChange(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	r.Srv.Down = true
	_, _, err := r.Apply()
	if diag.CodeOf(err) != diag.CodeGitHubUnavailable {
		t.Fatalf("err = %v, want BR-E022", err)
	}
	assertNothingHappened(t, r)
}

func TestPublicRepositoryIsRefusedWithoutOptIn(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	r.Srv.Repos[testrig.Target] = false // make acme/widgets public
	_, _, err := r.Apply()
	if diag.CodeOf(err) != diag.CodePublicRepoRefused {
		t.Fatalf("err = %v, want BR-E060", err)
	}
	for _, want := range []string{"fork", "--allow-public-runner", "placement: github"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should explain itself, missing %q:\n%v", want, err)
		}
	}
	assertNothingHappened(t, r)

	// The guard holds for dry-run as well: a plan for a forbidden setup is not offered.
	if _, err := install.ApplyEngine(r.Env, &testrig.Recorder{}).DryRun(ctx); diag.CodeOf(err) != diag.CodePublicRepoRefused {
		t.Errorf("dry-run err = %v, want BR-E060", err)
	}
}

func TestPublicRepositoryWithExplicitOptInProceeds(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	r.Srv.Repos[testrig.Target] = false
	r.Env.AllowPublic = true
	if _, _, err := r.Apply(); err != nil {
		t.Fatalf("explicit opt-in should proceed: %v", err)
	}
}

// failingVisibility is a provider whose visibility lookup fails, everything else real.
type failingVisibility struct{ provider.Provider }

func (failingVisibility) Visibility(context.Context, string) (provider.Visibility, error) {
	return provider.VisibilityUnknown, diag.New(diag.CodeGitHubUnavailable, "boom", "test", "test")
}

func TestUnknownVisibilityFailsClosed(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	r.Env.Provider = failingVisibility{r.Env.Provider}
	_, _, err := r.Apply()
	if diag.CodeOf(err) != diag.CodeVisibilityUnknown {
		t.Fatalf("err = %v, want BR-E061: when a public repository is possible, guessing is not allowed", err)
	}
	assertNothingHappened(t, r)
}

func TestPublicRepoDecisionTable(t *testing.T) {
	tests := []struct {
		vis   provider.Visibility
		allow bool
		want  string
	}{
		{provider.Private, false, ""},
		{provider.Private, true, ""},
		{provider.Public, true, ""},
		{provider.Public, false, diag.CodePublicRepoRefused},
	}
	for _, tc := range tests {
		err := install.PublicRepoDecision("o/r", tc.vis, tc.allow)
		if diag.CodeOf(err) != tc.want {
			t.Errorf("vis=%v allow=%v: code %q, want %q", tc.vis, tc.allow, diag.CodeOf(err), tc.want)
		}
	}
}
