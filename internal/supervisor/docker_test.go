package supervisor_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/modullar/blade-runner/internal/execx"
	"github.com/modullar/blade-runner/internal/isolation"
	"github.com/modullar/blade-runner/internal/provider"
	"github.com/modullar/blade-runner/internal/supervisor"
	"github.com/modullar/blade-runner/internal/testrig"
)

// These tests use the REAL Docker daemon and the real isolation runtime. The image is built here
// FROM scratch (registry pulls are not available in every environment), holds only the
// fakerunner fixture, and is referenced by image id, which is content-pinned.

func realDocker(t *testing.T) string {
	t.Helper()
	return testrig.FakeRunnerImage(t) // skips with a reason when there is no Docker
}

type argvRecorder struct {
	inner execx.Runner
	mu    sync.Mutex
	argvs []string
}

func (a *argvRecorder) Run(c context.Context, cmd execx.Cmd) (execx.Result, error) {
	a.mu.Lock()
	a.argvs = append(a.argvs, cmd.Name+" "+strings.Join(cmd.Args, " "))
	a.mu.Unlock()
	return a.inner.Run(c, cmd)
}
func (a *argvRecorder) LookPath(n string) (string, error) { return a.inner.LookPath(n) }

func (a *argvRecorder) commands() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.argvs...)
}

// assigning plays GitHub's side around a real container: when the container starts, GitHub has
// handed the runner its job; when it ends, the job is done. The container itself is real.
type assigning struct {
	inner *supervisor.DockerRuntime
	r     *rig
	job   int64
	run   int64
}

func (a *assigning) Run(c context.Context, spec isolation.Spec) (isolation.Result, error) {
	a.r.srv.UpdateJob(repo, a.job, func(j *provider.Job) { j.Status, j.RunnerName = "in_progress", spec.Name })
	res, err := a.inner.Run(c, spec)
	a.r.srv.UpdateJob(repo, a.job, func(j *provider.Job) { j.Status = "completed" })
	a.r.srv.SetRunStatus(repo, a.run, "completed")
	return res, err
}
func (a *assigning) RemoveStale(c context.Context) (int, error) { return a.inner.RemoveStale(c) }

func containersFor(t *testing.T, label string) string {
	t.Helper()
	out, err := exec.Command("docker", "ps", "-aq", "--filter", "label="+label).Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestRealDocker_AdmittedJobRunsInAnIsolatedContainerAndLeavesNothingBehind(t *testing.T) {
	img := realDocker(t)
	rec := &argvRecorder{inner: execx.OS{}}
	dock := &supervisor.DockerRuntime{Docker: &isolation.Docker{Exec: rec}, Owner: "mini"}
	r := newRig(t, withConfig(func(c *supervisor.Config) { c.Image = img }))
	r.queue(q{run: 1, job: 10})
	r.cfg.Runtime = &assigning{inner: dock, r: r, job: 10, run: 1}
	s := r.newSupervisor()

	out, err := s.Tick(ctx)
	if err != nil || !out.Launched {
		t.Fatalf("outcome = %+v, err = %v\nlog:\n%s", out, err, r.log.String())
	}
	f := r.entriesOfKind(supervisor.KindFinished)
	if len(f) != 1 || f[0].ExitCode == nil || *f[0].ExitCode != 0 || len(f[0].JobsRun) != 1 {
		t.Fatalf("finished = %+v: the fixture exits 0 only if the config arrived on stdin", f)
	}
	if left := containersFor(t, "bladerunner.run_id=1"); left != "" {
		t.Errorf("container %s was left behind", left)
	}
	if rs := r.srv.Runners(repo); len(rs) != 0 {
		t.Errorf("registration left behind: %+v", rs)
	}
	var created bool
	for _, c := range rec.commands() {
		if strings.HasPrefix(c, "docker create") {
			created = true
			if strings.Contains(c, "JIT-") {
				t.Errorf("the runner config is on the docker command line: %s", c)
			}
			if !strings.Contains(c, "--read-only") || !strings.Contains(c, "--cap-drop ALL") || !strings.Contains(c, "sha256:") {
				t.Errorf("the container is not created confined and pinned: %s", c)
			}
		}
	}
	if !created {
		t.Error("docker create was never called")
	}
}

func TestRealDocker_RefusedJobNeverReachesDocker(t *testing.T) {
	img := realDocker(t)
	rec := &argvRecorder{inner: execx.OS{}}
	dock := &supervisor.DockerRuntime{Docker: &isolation.Docker{Exec: rec}, Owner: "mini"}
	r := newRig(t, withConfig(func(c *supervisor.Config) { c.Image = img; c.Runtime = dock }))
	r.queue(q{run: 1, job: 10, who: "unsigned"})
	r.queue(q{run: 2, job: 20, who: "mallory", event: "pull_request", headRepo: fork})

	out, err := r.sup.Tick(ctx)
	if err != nil || out.Launched || len(out.Refused) != 2 {
		t.Fatalf("outcome = %+v, err = %v", out, err)
	}
	if cmds := rec.commands(); len(cmds) != 0 {
		t.Errorf("Docker was contacted for jobs that were refused: %v", cmds)
	}
	if r.requestedJIT() {
		t.Error("a runner registration was requested for a refused job")
	}
	if n := len(r.entriesOfKind(supervisor.KindRefused)); n != 2 {
		t.Errorf("%d refusal records, want 2", n)
	}
}

func TestRealDocker_StartUpRemovesOnlyTheContainersThisSupervisorLeftBehind(t *testing.T) {
	img := realDocker(t)
	dock := &supervisor.DockerRuntime{Docker: &isolation.Docker{Exec: execx.OS{}}, Owner: "mini"}
	mine := fmt.Sprintf("br-jit-mini-stale%d", os.Getpid())
	other := fmt.Sprintf("br-other-%d", os.Getpid())
	for name, label := range map[string]string{mine: supervisor.LabelSupervisor + "=mini", other: supervisor.LabelSupervisor + "=someone-else"} {
		if out, err := exec.Command("docker", "create", "--name", name, "--label", label, "--label", isolation.LabelJob+"="+name, img).CombinedOutput(); err != nil {
			t.Fatalf("docker create: %v\n%s", err, out)
		}
		name := name
		t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	}
	r := newRig(t, withConfig(func(c *supervisor.Config) { c.Image = img; c.Runtime = dock }))
	if err := r.sup.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if left := containersFor(t, supervisor.LabelSupervisor+"=mini"); left != "" {
		t.Errorf("the stale container %s survived the restart", left)
	}
	if left := containersFor(t, supervisor.LabelSupervisor+"=someone-else"); left == "" {
		t.Error("a container that is not this supervisor's was removed")
	}
	if st := r.entriesOfKind(supervisor.KindStartup); len(st) != 1 || !strings.Contains(st[0].Message, "1 leftover container") {
		t.Errorf("startup entry = %+v", st)
	}
}
