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
	"github.com/modullar/blade-runner/internal/secrets"
)

func TestRemoveLeavesNothingBehind(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	if _, _, err := r.Apply(); err != nil {
		t.Fatal(err)
	}

	changed, err := r.Remove()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"service-removed", "github-registration", "runner-files", "work-dir-removed", "token-removed", "local-state"}; !reflect.DeepEqual(changed, want) {
		t.Errorf("removed via %v, want %v", changed, want)
	}
	if left := r.Leftovers(); len(left) != 0 {
		t.Errorf("remove left files behind: %v", left)
	}
	if got := r.Srv.Runners(testrig.Target); len(got) != 0 {
		t.Errorf("runner still registered on GitHub: %+v", got)
	}
	if r.Plat.Installed || r.Plat.Running {
		t.Error("service still installed or running")
	}
	if _, err := r.Env.Secrets.Get(ctx, testrig.RunnerName); diag.CodeOf(err) != diag.CodeTokenMissing {
		t.Errorf("token still stored: %v", err)
	}
}

func TestRemoveTwiceAndOnAnEmptyMachineIsFine(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	if _, _, err := r.Apply(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Remove(); err != nil {
		t.Fatal(err)
	}
	// Everything, the token included, is gone: a second remove has nothing to do and must
	// not demand a token just to find that out.
	changed, err := r.Remove()
	if err != nil || len(changed) != 0 {
		t.Errorf("second remove: changed=%v err=%v, want a clean no-op", changed, err)
	}
}

func TestRemoveKeepsOtherRunnersAndSharedDirectories(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	if _, _, err := r.Apply(); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(r.Env.Layout.Home, "runners", "another-runner")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("removing one runner deleted another: %v", err)
	}
}

func TestRemoveNeverDeletesAWorkDirItDidNotCreate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "precious")
	cfg := strings.Replace(testrig.BaseConfig, "  token:", "  work_dir: "+dir+"\n  token:", 1)
	r := testrig.New(t, cfg)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Remove without ever applying: the directory has no Blade Runner marker.
	rep := &testrig.Recorder{}
	if _, err := install.RemoveEngine(r.Env, rep).Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "keep.txt")); err != nil {
		t.Errorf("remove deleted a directory it did not create: %v", err)
	}
}

func TestRemoveNeverDeletesAWorkDirOwnedByAnotherRunner(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	if _, _, err := r.Apply(); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(r.UserHome, ".bladerunner/work/test-runner/.bladerunner-work")
	if err := os.WriteFile(marker, []byte("someone-else\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("a work dir marked for another runner was deleted: %v", err)
	}
}

func TestRemoveRefusesToTouchHomeEvenIfConfiguredAsWorkDir(t *testing.T) {
	cfg := strings.Replace(testrig.BaseConfig, "  token:", "  work_dir: \"~\"\n  token:", 1)
	r := testrig.New(t, cfg)
	// A marker in the home directory, as a hostile or mistaken setup might leave.
	if err := os.WriteFile(filepath.Join(r.UserHome, ".bladerunner-work"), []byte(testrig.RunnerName+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.UserHome, ".bladerunner-work")); err != nil {
		t.Errorf("remove deleted the home directory's contents: %v", err)
	}
}

func TestRemoveResumesAfterGitHubWasUnreachable(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	if _, _, err := r.Apply(); err != nil {
		t.Fatal(err)
	}
	r.Srv.Down = true
	changed, err := r.Remove()
	if diag.CodeOf(err) != diag.CodeGitHubUnavailable {
		t.Fatalf("err = %v, want BR-E022", err)
	}
	if want := []string{"service-removed"}; !reflect.DeepEqual(changed, want) {
		t.Errorf("before the failure: changed %v, want %v", changed, want)
	}
	if !r.Exists(".bladerunner/runners/test-runner/runner") {
		t.Error("local files were deleted before the GitHub registration could be removed")
	}
	if _, err := r.Env.Secrets.Get(ctx, testrig.RunnerName); err != nil {
		t.Error("the token was deleted while it was still needed to deregister")
	}

	r.Srv.Down = false
	changed, err = r.Remove()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"github-registration", "runner-files", "work-dir-removed", "token-removed", "local-state"}; !reflect.DeepEqual(changed, want) {
		t.Errorf("resumed remove changed %v, want %v", changed, want)
	}
	if left := r.Leftovers(); len(left) != 0 {
		t.Errorf("leftovers: %v", left)
	}
}

func TestSkipDeregisterLeavesTheGitHubRunner(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	if _, _, err := r.Apply(); err != nil {
		t.Fatal(err)
	}
	r.Srv.Down = true // the machine cannot reach GitHub
	r.Env.SkipDeregister = true
	if _, err := r.Remove(); err != nil {
		t.Fatalf("remove --skip-deregister must work offline: %v", err)
	}
	if left := r.Leftovers(); len(left) != 0 {
		t.Errorf("leftovers: %v", left)
	}
	if got := r.Srv.Runners(testrig.Target); len(got) != 1 {
		t.Errorf("the GitHub runner should remain for the user to delete, got %+v", got)
	}
}

func TestRemoveWithoutATokenSuggestsSkipDeregister(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	if _, _, err := r.Apply(); err != nil {
		t.Fatal(err)
	}
	if err := r.Env.Secrets.Delete(ctx, testrig.RunnerName); err != nil {
		t.Fatal(err)
	}
	_, err := r.Remove()
	if diag.CodeOf(err) != diag.CodeTokenMissing || !strings.Contains(err.Error(), "--skip-deregister") {
		t.Errorf("err = %v, want BR-E020 pointing at --skip-deregister", err)
	}
}

func TestRemoveStopsAServiceThatIsStillRunningBeforeDeregistering(t *testing.T) {
	r := testrig.New(t, testrig.BaseConfig)
	if _, _, err := r.Apply(); err != nil {
		t.Fatal(err)
	}
	var order []string
	rep := orderRecorder{order: &order}
	if _, err := install.RemoveEngine(r.Env, rep).Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if len(order) < 2 || order[0] != "service-removed" || order[1] != "github-registration" {
		t.Errorf("order = %v, want the service stopped before deregistering (GitHub refuses a busy runner)", order)
	}
}

type orderRecorder struct{ order *[]string }

func (orderRecorder) Note(string)       {}
func (orderRecorder) GatePassed(string) {}
func (o orderRecorder) StepResult(id string, out core.Outcome, _ string) {
	if out == core.Applied {
		*o.order = append(*o.order, id)
	}
}

func TestEnvTokenSourceNeedsNothingDeletedOnRemove(t *testing.T) {
	t.Setenv(secrets.EnvVar, testrig.Token)
	r := testrig.New(t, strings.Replace(testrig.BaseConfig, "source: file", "source: env", 1))
	if _, _, err := r.Apply(); err != nil {
		t.Fatal(err)
	}
	changed, err := r.Remove()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range changed {
		if id == "token-removed" {
			t.Error("the env token source has nothing stored, so nothing to delete")
		}
	}
	if left := r.Leftovers(); len(left) != 0 {
		t.Errorf("leftovers: %v", left)
	}
}
