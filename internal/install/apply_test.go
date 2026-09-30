package install_test

import (
	"github.com/modullar/blade-runner/internal/testrig"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/modullar/blade-runner/internal/core"
	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/install"
	"github.com/modullar/blade-runner/internal/provider/github/githubtest"
)

var allApplySteps = []string{"runner-binary", "work-dir", "runner-registration", "service-definition", "service-running"}

func TestApplyFromACleanMachineYieldsAnOnlineRunner(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	changed, _, err := r.Apply()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(changed, allApplySteps) {
		t.Errorf("changed = %v, want %v", changed, allApplySteps)
	}

	// The runner is on disk, executable, and pinned.
	runDir := r.Env.Layout.RunnerDir()
	info, err := os.Stat(filepath.Join(runDir, "run.sh"))
	if err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Errorf("run.sh missing or not executable: %v %v", info, err)
	}
	st, _ := r.Env.State.Load()
	if st.RunnerVersion != githubtest.Version || st.RunnerSHA256 == "" || st.Target != testrig.Target {
		t.Errorf("state = %+v, want the release pinned", st)
	}
	for id, want := range map[string]string{"runner-binary": "done", "service-running": "done"} {
		if st.Steps[id].Status != want {
			t.Errorf("state step %s = %+v", id, st.Steps[id])
		}
	}

	// GitHub sees it, online, with the implicit labels plus ours.
	got := r.Srv.Runners(testrig.Target)
	if len(got) != 1 || got[0].Name != testrig.RunnerName || !got[0].Online {
		t.Fatalf("GitHub runners = %+v", got)
	}
	if want := []string{"self-hosted", "Linux", "X64", "gpu"}; !reflect.DeepEqual(got[0].Labels, want) {
		t.Errorf("labels on GitHub = %v, want %v", got[0].Labels, want)
	}
	argv, _ := os.ReadFile(filepath.Join(r.Env.Layout.RunnerDir(), ".config-args"))
	if !strings.Contains(string(argv), "--labels\ngpu\n") || strings.Contains(string(argv), "self-hosted") {
		t.Errorf("config.sh must be given only the extra labels (the runner adds the defaults itself):\n%s", argv)
	}

	// The service is installed and running.
	if !r.Plat.Installed || !r.Plat.Running {
		t.Errorf("service installed=%v running=%v", r.Plat.Installed, r.Plat.Running)
	}
	// The work dir is marked as ours.
	if !r.Exists(".bladerunner/work/test-runner/.bladerunner-work") {
		t.Error("work dir marker missing")
	}
}

func TestTokenNeverReachesArgvOnlyTheEnvironment(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	if _, _, err := r.Apply(); err != nil {
		t.Fatal(err)
	}
	// The fake config.sh records its argv and exits 9 if it sees --token; registration
	// succeeding at all proves the token arrived through ACTIONS_RUNNER_INPUT_TOKEN.
	argv, err := os.ReadFile(filepath.Join(r.Env.Layout.RunnerDir(), ".config-args"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{testrig.Token, "REG-"} {
		if strings.Contains(string(argv), secret) {
			t.Errorf("config.sh argv contains %q:\n%s", secret, argv)
		}
	}
	for _, want := range []string{"--unattended", "--replace", "--name\ntest-runner", "--url\n" + r.Srv.URL + "/acme/widgets"} {
		if !strings.Contains(string(argv), want) {
			t.Errorf("argv missing %q:\n%s", want, argv)
		}
	}
	// And nothing on disk under the runner's home holds a registration token.
	_ = filepath.WalkDir(r.Env.Layout.RunnerHome(), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Type().IsRegular() {
			if data, _ := os.ReadFile(p); strings.Contains(string(data), "REG-") {
				t.Errorf("registration token persisted in %s", p)
			}
		}
		return nil
	})
}

func TestSecondApplyChangesNothing(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	if _, _, err := r.Apply(); err != nil {
		t.Fatal(err)
	}
	tokensBefore := countRequests(r, "POST /repos/acme/widgets/actions/runners/registration-token")
	downloadsBefore := countRequests(r, "GET /download/")
	installs, starts := r.Plat.Installs, r.Plat.Starts

	changed, rep, err := r.Apply()
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 0 {
		t.Errorf("second apply changed %v, want nothing", changed)
	}
	for id, o := range rep.Results {
		if o != core.Converged {
			t.Errorf("step %s = %v on the second run", id, o)
		}
	}
	if n := countRequests(r, "POST /repos/acme/widgets/actions/runners/registration-token"); n != tokensBefore {
		t.Errorf("a second apply minted another registration token (%d -> %d)", tokensBefore, n)
	}
	if n := countRequests(r, "GET /download/"); n != downloadsBefore {
		t.Errorf("a second apply downloaded the runner again")
	}
	if r.Plat.Installs != installs || r.Plat.Starts != starts {
		t.Error("a second apply touched the service")
	}
}

func countRequests(r *testrig.Rig, prefix string) int {
	n := 0
	for _, req := range r.Srv.Requests() {
		if strings.HasPrefix(req, prefix) {
			n++
		}
	}
	return n
}

func TestDryRunChangesNothingAndPredictsTheRealRun(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	rep := &testrig.Recorder{}
	plan, err := install.ApplyEngine(r.Env, rep).DryRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.Pending(), allApplySteps) {
		t.Errorf("dry-run pending = %v, want every step on a clean machine", plan.Pending())
	}
	if r.Exists(".bladerunner/runners") || r.Exists(".bladerunner/work") {
		t.Error("dry-run created files")
	}
	if r.Plat.Installs != 0 || r.Plat.Starts != 0 {
		t.Error("dry-run touched the service")
	}
	for _, req := range r.Srv.Requests() {
		if strings.HasPrefix(req, "POST") || strings.HasPrefix(req, "GET /download") {
			t.Errorf("dry-run made a changing or downloading request: %s", req)
		}
	}

	changed, _, err := r.Apply()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.Pending(), changed) {
		t.Errorf("dry-run predicted %v but apply changed %v", plan.Pending(), changed)
	}
}

func TestPartialStateDryRunListsOnlyWhatRemains(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	r.Plat.FailStartOnce = true
	if _, _, err := r.Apply(); err == nil {
		t.Fatal("expected the start failure")
	}
	plan, err := install.ApplyEngine(r.Env, &testrig.Recorder{}).DryRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"service-running"}; !reflect.DeepEqual(plan.Pending(), want) {
		t.Errorf("pending after a crash at the last step = %v, want %v", plan.Pending(), want)
	}
}

func TestResumeAfterAFailedStartDoesNotRedoEarlierSteps(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	r.Plat.FailStartOnce = true

	changed, _, err := r.Apply()
	if diag.CodeOf(err) != diag.CodeServiceInstall {
		t.Fatalf("first apply err = %v, want the service failure", err)
	}
	if want := allApplySteps[:4]; !reflect.DeepEqual(changed, want) {
		t.Errorf("first apply changed %v, want %v", changed, want)
	}
	st, _ := r.Env.State.Load()
	if st.Steps["service-running"].Status != "failed" {
		t.Errorf("failure not recorded: %+v", st.Steps)
	}

	changed, _, err = r.Apply()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"service-running"}; !reflect.DeepEqual(changed, want) {
		t.Errorf("resumed apply changed %v, want only %v", changed, want)
	}
	if n := countRequests(r, "POST /repos/acme/widgets/actions/runners/registration-token"); n != 1 {
		t.Errorf("registration tokens minted = %d, want exactly 1 across both runs", n)
	}
	if len(r.Srv.Runners(testrig.Target)) != 1 {
		t.Errorf("GitHub runners = %+v", r.Srv.Runners(testrig.Target))
	}
}

func TestChecksumMismatchInstallsNothing(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	r.Srv.ChecksumOverride = strings.Repeat("ab", 32)
	changed, _, err := r.Apply()
	if diag.CodeOf(err) != diag.CodeChecksumMismatch {
		t.Fatalf("err = %v, want BR-E031", err)
	}
	if len(changed) != 0 {
		t.Errorf("changed = %v", changed)
	}
	if r.Exists(".bladerunner/runners/test-runner/runner") || r.Exists(".bladerunner/runners/test-runner/runner.new") {
		t.Error("a runner directory exists although the checksum did not match")
	}
	entries, _ := os.ReadDir(r.Env.Layout.DownloadDir())
	if len(entries) != 0 {
		t.Errorf("the unverified download was kept: %v", entries)
	}
	if len(r.Srv.Runners(testrig.Target)) != 0 {
		t.Error("a runner was registered")
	}
	if countRequests(r, "POST /repos/acme/widgets/actions/runners/registration-token") != 0 {
		t.Error("a registration token was minted for a runner that was never installed")
	}
}

func TestFailedRegistrationIsDiagnosedWithoutLeakingTheToken(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	r.Srv.Tarball = githubtest.RunnerTarballWith("#!/bin/sh\necho \"refused token $ACTIONS_RUNNER_INPUT_TOKEN\" >&2\nexit 4\n")
	_, _, err := r.Apply()
	if diag.CodeOf(err) != diag.CodeRegistrationFailed {
		t.Fatalf("err = %v, want BR-E040", err)
	}
	if strings.Contains(err.Error(), "REG-") {
		t.Errorf("the registration token leaked into the error:\n%v", err)
	}
	if !strings.Contains(err.Error(), "refused token ***") {
		t.Errorf("the runner's own message should survive, redacted:\n%v", err)
	}
	if r.Plat.Installed {
		t.Error("the service was installed for an unregistered runner")
	}
}

func TestPinnedVersionSurvivesAReinstall(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	if _, _, err := r.Apply(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(r.Env.Layout.RunnerDir()); err != nil { // the runner directory is lost
		t.Fatal(err)
	}
	if _, _, err := r.Apply(); err != nil {
		t.Fatal(err)
	}
	if countRequests(r, "GET /repos/actions/runner/releases/tags/v"+githubtest.Version) == 0 {
		t.Errorf("a reinstall must resolve the pinned release, not latest; requests: %v", r.Srv.Requests())
	}
	if n := countRequests(r, "GET /download/"); n != 1 {
		t.Errorf("downloads = %d, want 1 (the verified archive is cached)", n)
	}
}

func TestEditingLabelsReRegisters(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	if _, _, err := r.Apply(); err != nil {
		t.Fatal(err)
	}
	r.Env.Cfg.Runner.Labels = []string{"gpu", "fast"}
	changed, _, err := r.Apply()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"runner-registration"}; !reflect.DeepEqual(changed, want) {
		t.Errorf("changed = %v, want %v", changed, want)
	}
	got := r.Srv.Runners(testrig.Target)
	if len(got) != 1 || !reflect.DeepEqual(got[0].Labels, []string{"self-hosted", "Linux", "X64", "gpu", "fast"}) {
		t.Errorf("runners = %+v, want one runner with the new labels (--replace)", got)
	}
}

func TestRunnerDeletedOnGitHubIsRegisteredAgain(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	if _, _, err := r.Apply(); err != nil {
		t.Fatal(err)
	}
	for _, rn := range r.Srv.Runners(testrig.Target) {
		c := r.Env.Provider
		if err := c.RemoveRunner(ctx, r.Env.Scope(), rn.ID); err != nil {
			t.Fatal(err)
		}
	}
	changed, _, err := r.Apply()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"runner-registration"}; !reflect.DeepEqual(changed, want) {
		t.Errorf("changed = %v, want %v", changed, want)
	}
	if len(r.Srv.Runners(testrig.Target)) != 1 {
		t.Errorf("runners = %+v", r.Srv.Runners(testrig.Target))
	}
}

func TestUnsafeWorkDirIsRefused(t *testing.T) {
	for _, dir := range []string{"~", "/", "relative/dir"} {
		cfg := strings.Replace(testrig.BaseConfig, "  token:", "  work_dir: \""+dir+"\"\n  token:", 1)
		r := testrig.New(t, cfg)
		_, _, err := r.Apply()
		if diag.CodeOf(err) != diag.CodeConfigInvalid {
			t.Errorf("work_dir %q: err = %v, want BR-E001", dir, err)
		}
		if r.Plat.Installed {
			t.Errorf("work_dir %q: the service was installed anyway", dir)
		}
	}
}

func TestOrgScopeRegistersAtTheOrganization(t *testing.T) {
	cfg := strings.Replace(testrig.BaseConfig, "scope: repo\n  repository: acme/widgets", "scope: org\n  organization: acme", 1)
	r := testrig.New(t, cfg)
	_, rep, err := r.Apply()
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Srv.Runners("acme"); len(got) != 1 {
		t.Errorf("org runners = %+v", got)
	}
	if len(rep.Notes) == 0 || !strings.Contains(rep.Notes[0], "runner group") {
		t.Errorf("org scope should warn about runner groups and public repos, notes = %v", rep.Notes)
	}
}
