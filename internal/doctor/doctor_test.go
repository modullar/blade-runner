package doctor_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/doctor"
	"github.com/modullar/blade-runner/internal/platform"
	"github.com/modullar/blade-runner/internal/testrig"
)

var ctx = context.Background()

func plenty(string) (uint64, error) { return 100 << 30, nil }

func run(r *testrig.Rig) map[string]doctor.Result {
	out := map[string]doctor.Result{}
	for _, res := range doctor.Run(ctx, r.Env, doctor.Options{CLIVersion: "0.1.0-dev", FreeBytes: plenty}) {
		out[res.ID] = res
	}
	return out
}

func wantLevel(t *testing.T, got map[string]doctor.Result, id string, lvl doctor.Level, code string) {
	t.Helper()
	r, ok := got[id]
	if !ok {
		t.Fatalf("no result for check %q", id)
	}
	if r.Level != lvl || r.Code != code {
		t.Errorf("%s = %v %q (%s), want %v %q", id, r.Level, r.Code, r.Message, lvl, code)
	}
}

func applied(t *testing.T, cfg string) *testrig.Rig {
	t.Helper()
	r := testrig.New(t, cfg)
	if _, _, err := r.Apply(); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestHealthyInstallPassesEveryCheck(t *testing.T) {
	r := applied(t, testrig.BaseConfig)
	res := doctor.Run(ctx, r.Env, doctor.Options{CLIVersion: "0.1.0-dev", FreeBytes: plenty})
	if len(res) != 9 {
		t.Errorf("got %d checks, want 9", len(res))
	}
	for _, x := range res {
		if x.Level != doctor.Pass {
			t.Errorf("%s = %v %q: %s", x.ID, x.Level, x.Code, x.Message)
		}
	}
	if doctor.Failed(res) {
		t.Error("Failed reported a healthy install")
	}
}

func TestBeforeApplyTheRunnerAndServiceAreReportedMissing(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	got := run(r)
	wantLevel(t, got, "token", doctor.Pass, "")
	wantLevel(t, got, "token-valid", doctor.Pass, "")
	wantLevel(t, got, "runner-registered", doctor.Fail, diag.CodeRunnerNotRegistered)
	wantLevel(t, got, "service", doctor.Fail, diag.CodeServiceInstall)
	if !strings.Contains(got["runner-registered"].Fix, "bladerunner apply") {
		t.Errorf("fix should be `bladerunner apply`: %q", got["runner-registered"].Fix)
	}
}

func TestRunnerOfflineOnGitHub(t *testing.T) {
	r := applied(t, testrig.BaseConfig)
	r.Srv.SetOnline(testrig.Target, testrig.RunnerName, false)
	wantLevel(t, run(r), "runner-registered", doctor.Fail, diag.CodeRunnerOffline)
}

func TestServiceStoppedAndStale(t *testing.T) {
	r := applied(t, testrig.BaseConfig)
	r.Plat.Running = false
	wantLevel(t, run(r), "service", doctor.Fail, diag.CodeServiceNotRunning)
}

func TestLabelDriftIsReported(t *testing.T) {
	r := applied(t, testrig.BaseConfig)
	r.Env.Cfg.Runner.Labels = []string{"gpu", "fast"}
	got := run(r)
	wantLevel(t, got, "runner-registered", doctor.Fail, diag.CodeRunnerNotRegistered)
	if !strings.Contains(got["runner-registered"].Message, "fast") {
		t.Errorf("message should name the missing label: %q", got["runner-registered"].Message)
	}
}

func TestMissingTokenSkipsEverythingThatNeedsGitHub(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	if err := r.Env.Secrets.Delete(ctx, testrig.RunnerName); err != nil {
		t.Fatal(err)
	}
	got := run(r)
	wantLevel(t, got, "token", doctor.Fail, diag.CodeTokenMissing)
	for _, id := range []string{"token-valid", "public-repo", "runner-registered"} {
		wantLevel(t, got, id, doctor.Skip, "")
	}
	wantLevel(t, got, "service", doctor.Fail, diag.CodeServiceInstall) // local checks still run
}

func TestRejectedTokenSkipsDependentChecks(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	if err := r.Env.Secrets.Set(ctx, testrig.RunnerName, "ghp_wrong"); err != nil {
		t.Fatal(err)
	}
	got := run(r)
	wantLevel(t, got, "token", doctor.Pass, "")
	wantLevel(t, got, "token-valid", doctor.Fail, diag.CodeTokenRejected)
	wantLevel(t, got, "public-repo", doctor.Skip, "")
	wantLevel(t, got, "runner-registered", doctor.Skip, "")
}

func TestGitHubUnreachable(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	r.Srv.Down = true
	wantLevel(t, run(r), "token-valid", doctor.Fail, diag.CodeGitHubUnavailable)
}

func TestPublicRepositoryIsAFailure(t *testing.T) {
	r := applied(t, testrig.BaseConfig)
	r.Srv.Repos[testrig.Target] = false
	got := run(r)
	wantLevel(t, got, "public-repo", doctor.Fail, diag.CodePublicRepoRefused)
	if !doctor.Failed(doctor.Run(ctx, r.Env, doctor.Options{FreeBytes: plenty})) {
		t.Error("Failed must be true when a check fails")
	}
}

func TestMinVersion(t *testing.T) {
	tests := []struct {
		cli, min string
		lvl      doctor.Level
	}{
		{"0.1.0-dev", "0.1.0", doctor.Pass}, // a prerelease of the minimum satisfies it
		{"0.2.0", "0.1.0", doctor.Pass},
		{"1.0.0", "0.9.9", doctor.Pass},
		{"0.1.0", "0.2.0", doctor.Warn},
		{"0.9.9", "1.0.0", doctor.Warn},
		{"garbage", "0.1.0", doctor.Warn},
	}
	for _, tc := range tests {
		r := testrig.New(t, testrig.BaseConfig)
		r.Env.Cfg.BladeRunner.MinVersion = tc.min
		var got doctor.Result
		for _, x := range doctor.Run(ctx, r.Env, doctor.Options{CLIVersion: tc.cli, FreeBytes: plenty}) {
			if x.ID == "cli-version" {
				got = x
			}
		}
		if got.Level != tc.lvl {
			t.Errorf("cli %s, min %s: %v (%s), want %v", tc.cli, tc.min, got.Level, got.Message, tc.lvl)
		}
		if tc.lvl == doctor.Warn && got.Code != diag.CodeCLIOutdated {
			t.Errorf("code = %q", got.Code)
		}
	}
}

func TestDiskThresholds(t *testing.T) {
	const gib = 1 << 30
	tests := []struct {
		free uint64
		err  error
		lvl  doctor.Level
	}{
		{100 * gib, nil, doctor.Pass},
		{10 * gib, nil, doctor.Pass},
		{9 * gib, nil, doctor.Warn},
		{3 * gib, nil, doctor.Warn},
		{1 * gib, nil, doctor.Fail},
		{0, nil, doctor.Fail},
		{0, errors.New("statfs failed"), doctor.Warn},
	}
	for _, tc := range tests {
		r := testrig.New(t, testrig.BaseConfig)
		free, err := tc.free, tc.err
		res := doctor.Run(ctx, r.Env, doctor.Options{CLIVersion: "0.1.0", FreeBytes: func(string) (uint64, error) { return free, err }})
		i := slices.IndexFunc(res, func(x doctor.Result) bool { return x.ID == "disk-space" })
		if res[i].Level != tc.lvl {
			t.Errorf("free %d GiB err %v: %v (%s), want %v", tc.free/gib, tc.err, res[i].Level, res[i].Message, tc.lvl)
		}
		if tc.lvl != doctor.Pass && res[i].Code != diag.CodeDiskLow {
			t.Errorf("code = %q", res[i].Code)
		}
	}
}

func TestDiskCheckWorksBeforeTheWorkDirExists(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	var asked string
	doctor.Run(ctx, r.Env, doctor.Options{CLIVersion: "0.1.0", FreeBytes: func(p string) (uint64, error) { asked = p; return 100 << 30, nil }})
	if asked == "" || !strings.HasPrefix(r.Env.WorkDir(), asked) {
		t.Errorf("free space asked at %q, want an existing ancestor of %q", asked, r.Env.WorkDir())
	}
}

func TestLocalPlacementWarns(t *testing.T) {
	for cfg, want := range map[string]doctor.Level{
		testrig.BaseConfig: doctor.Pass,
		strings.Replace(testrig.BaseConfig, "default: auto", "default: local", 1):                       doctor.Warn,
		strings.Replace(testrig.BaseConfig, "default: auto", "default: auto\n  jobs:\n    t: local", 1): doctor.Warn,
	} {
		r := testrig.New(t, cfg)
		res := run(r)["local-placement"]
		if res.Level != want {
			t.Errorf("config %q: %v, want %v", cfg, res.Level, want)
		}
		if want == doctor.Warn && res.Code != diag.CodeLocalPlacement {
			t.Errorf("code = %q", res.Code)
		}
	}
}

func TestMissingPrerequisites(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	r.Plat.Prereqs = []platform.Prereq{{Name: "systemctl", OK: false, Fix: "install systemd"}}
	got := run(r)
	wantLevel(t, got, "prerequisites", doctor.Fail, diag.CodePrereqMissing)
	if !strings.Contains(got["prerequisites"].Fix, "install systemd") {
		t.Errorf("fix = %q", got["prerequisites"].Fix)
	}
}

func TestEveryFindingHasADocumentedCodeAndDoctorChangesNothing(t *testing.T) {
	r := testrig.New(t, strings.Replace(testrig.BaseConfig, "default: auto", "default: local", 1))
	r.Plat.Prereqs = []platform.Prereq{{Name: "git", OK: false, Fix: "install git"}}
	r.Env.Cfg.BladeRunner.MinVersion = "9.9.9"
	requestsBefore := len(r.Srv.Requests())

	res := doctor.Run(ctx, r.Env, doctor.Options{CLIVersion: "0.1.0", FreeBytes: func(string) (uint64, error) { return 1 << 30, nil }})
	known := diag.All()
	for _, x := range res {
		if x.Level == doctor.Warn || x.Level == doctor.Fail {
			if !slices.Contains(known, x.Code) {
				t.Errorf("%s reports undocumented code %q", x.ID, x.Code)
			}
			if x.Docs() == "" || x.Fix == "" {
				t.Errorf("%s: a finding needs a docs link and a fix: %+v", x.ID, x)
			}
		}
	}
	for _, req := range r.Srv.Requests()[requestsBefore:] {
		if !strings.HasPrefix(req, "GET") {
			t.Errorf("doctor made a changing request: %s", req)
		}
	}
	// Doctor is read-only: apart from the token the rig stored, it created no runner or work files.
	for _, l := range r.Leftovers() {
		if strings.Contains(l, "runners") || strings.Contains(l, "work") {
			t.Errorf("doctor created %s", l)
		}
	}
}
