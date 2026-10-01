package cli_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modullar/blade-runner/internal/cli"
	"github.com/modullar/blade-runner/internal/core"
	"github.com/modullar/blade-runner/internal/provider"
	"github.com/modullar/blade-runner/internal/supervisor"
	"github.com/modullar/blade-runner/internal/testrig"
)

// The whole `bladerunner supervise` command, end to end: the real command line, the real config
// file, the real trust store (via `trust add`), real signed commits served by the fake GitHub, a
// REAL Docker container from a FROM scratch image, and the real audit log on disk.

type superviseRig struct {
	*cliRig
	image   string
	commits map[string]testrig.RealCommit
	pub     map[string]string
}

func newSuperviseRig(t *testing.T) *superviseRig {
	t.Helper()
	img := testrig.FakeRunnerImage(t) // skips with a reason when Docker is unavailable
	c := newCLIRig(t)
	commits, pub := c.serveCommits()
	c.mustRun(testrig.Token+"\n", c.initArgs()...)
	cfg, err := os.ReadFile(c.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.cfgPath, append(cfg, []byte("\nsupervisor:\n  image: "+img+"\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	return &superviseRig{cliRig: c, image: img, commits: commits, pub: pub}
}

func (s *superviseRig) trustOwner() {
	s.t.Helper()
	s.mustRun("", "trust", "add", "-c", s.cfgPath, "--name", "owner", "--key", s.writePub("owner", s.pub["owner"]))
}

// queue puts a queued push run with one job for this runner's labels at the fake GitHub.
func (s *superviseRig) queue(run, job int64, who string) {
	s.t.Helper()
	sha := s.commits[who].SHA
	s.srv.AddRun("acme/widgets", provider.Run{ID: run, HeadSHA: sha, Event: "push", Status: "queued", HeadRepository: "acme/widgets", Actor: who})
	s.srv.AddJob("acme/widgets", provider.Job{ID: job, RunID: run, Status: "queued", Labels: []string{"self-hosted", "gpu"}, HeadSHA: sha})
}

func (s *superviseRig) auditPath() string {
	// One log per runner name: two supervisors must never share (and fork) one chain.
	return filepath.Join(s.home(), "audit", "supervisor-e2e-runner.jsonl")
}

func (s *superviseRig) audit() []supervisor.Entry {
	s.t.Helper()
	es, err := supervisor.ReadAuditLog(s.auditPath())
	if err != nil {
		s.t.Fatal(err)
	}
	return es
}

func (s *superviseRig) noContainers(runID string) {
	s.t.Helper()
	out, err := exec.Command("docker", "ps", "-aq", "--filter", "label=bladerunner.run_id="+runID).Output()
	if err != nil {
		s.t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "" {
		s.t.Errorf("container %s was left behind", strings.TrimSpace(string(out)))
	}
}

func (s *superviseRig) requestedJIT() bool {
	for _, r := range s.srv.Requests() {
		if strings.HasSuffix(r, "/generate-jitconfig") {
			return true
		}
	}
	return false
}

func TestSuperviseRunsAnAdmittedJobInARealContainer(t *testing.T) {
	s := newSuperviseRig(t)
	s.trustOwner()
	s.queue(1, 10, "owner")

	s.mustRun("", "supervise", "-c", s.cfgPath, "--once")
	out := s.out.String()
	for _, want := range []string{"admitted: run 1 job 10", "signed by owner", "ran run 1 job 10 in an isolated container"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
	entries := s.audit()
	var kinds []string
	for _, e := range entries {
		kinds = append(kinds, e.Kind)
	}
	if strings.Join(kinds, ",") != "startup,launching,finished" {
		t.Fatalf("audit kinds = %v", kinds)
	}
	if e := entries[1]; e.Signer != "owner" || e.SHA != s.commits["owner"].SHA || e.RunID != 1 || e.JobID != 10 || !strings.HasPrefix(e.Runner, "br-jit-e2e-runner-") {
		t.Errorf("launching entry = %+v", e)
	}
	// The fixture inside the container exits 0 only if the runner config arrived on STDIN.
	if e := entries[2]; e.ExitCode == nil || *e.ExitCode != 0 {
		t.Errorf("finished entry = %+v: the container did not receive the config on stdin", e)
	}
	raw, _ := os.ReadFile(s.auditPath())
	if strings.Contains(string(raw), "JIT-") || strings.Contains(out, "JIT-") || strings.Contains(s.err.String(), "JIT-") {
		t.Error("the just-in-time config leaked into the audit log or the output")
	}
	if info, err := os.Stat(s.auditPath()); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("audit log: %v %v, want a 0600 file", info, err)
	}
	if n, err := supervisor.VerifyAuditLog(s.auditPath()); err != nil || n != 3 {
		t.Errorf("audit chain: %d, %v", n, err)
	}
	if rs := s.srv.Runners("acme/widgets"); len(rs) != 0 {
		t.Errorf("runner registration left behind: %+v", rs)
	}
	s.noContainers("1")
}

func TestSuperviseRefusesAJobNobodyVouchedForAndStartsNothing(t *testing.T) {
	s := newSuperviseRig(t)
	s.trustOwner()
	s.queue(1, 10, "unsigned")
	s.queue(2, 20, "mallory")

	if code := s.run("", "supervise", "-c", s.cfgPath, "--once"); code != cli.ExitOK {
		t.Fatalf("a refusal is correct behaviour: exit %d\n%s", code, s.err.String())
	}
	out := s.out.String()
	for _, want := range []string{"refused: run 1 job 10", "refused: run 2 job 20", "BR-E072", "BR-E075", "nothing started"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
	if s.requestedJIT() {
		t.Error("a runner registration was requested")
	}
	var refused, launching int
	for _, e := range s.audit() {
		switch e.Kind {
		case supervisor.KindRefused:
			refused++
			if e.Code != "BR-E072" || e.SHA == "" || e.Message == "" {
				t.Errorf("refusal entry = %+v", e)
			}
		case supervisor.KindLaunching:
			launching++
		}
	}
	if refused != 2 || launching != 0 {
		t.Errorf("refused %d, launching %d", refused, launching)
	}
	s.noContainers("1")
	s.noContainers("2")
}

func TestSuperviseWithholdsAnAdmittedJobWhileAStrangersJobWaits(t *testing.T) {
	s := newSuperviseRig(t)
	s.trustOwner()
	s.queue(1, 10, "owner")
	s.queue(2, 20, "mallory")

	s.mustRun("", "supervise", "-c", s.cfgPath, "--once")
	if !strings.Contains(s.out.String(), "BR-E075") || s.requestedJIT() {
		t.Errorf("the owner's job must wait while a stranger's could be taken by the same runner:\n%s", s.out.String())
	}
	// Trusting the second key admits it: now both are admitted and the oldest runs.
	s.mustRun("", "trust", "add", "-c", s.cfgPath, "--name", "mallory", "--key", s.writePub("mallory", s.pub["mallory"]))
	s.mustRun("", "supervise", "-c", s.cfgPath, "--once")
	if !strings.Contains(s.out.String(), "ran run 1 job 10") {
		t.Errorf("after trusting the stranger's key:\n%s", s.out.String())
	}
}

func TestSuperviseHonoursRevocation(t *testing.T) {
	s := newSuperviseRig(t)
	s.trustOwner()
	s.mustRun("", "trust", "revoke", "owner", "-c", s.cfgPath)
	s.queue(1, 10, "owner")
	s.mustRun("", "supervise", "-c", s.cfgPath, "--once")
	if !strings.Contains(s.out.String(), "revoked") || !strings.Contains(s.out.String(), "BR-E072") || s.requestedJIT() {
		t.Errorf("a revoked signer must be refused:\n%s", s.out.String())
	}
}

func TestSuperviseFailsClosedWhenGitHubIsDown(t *testing.T) {
	s := newSuperviseRig(t)
	s.trustOwner()
	s.queue(1, 10, "owner")
	s.srv.Down = true
	if code := s.run("", "supervise", "-c", s.cfgPath, "--once"); code != cli.ExitFailure || !strings.Contains(s.err.String(), "BR-E076") {
		t.Fatalf("exit %d\nstderr:\n%s", code, s.err.String())
	}
	s.srv.Down = false
	if s.requestedJIT() {
		t.Error("a runner was registered while GitHub was unreachable")
	}
	s.noContainers("1")
}

func TestSuperviseRefusesToStartWithoutAPinnedImageOrWhileAnotherSupervisorRuns(t *testing.T) {
	s := newSuperviseRig(t)
	s.trustOwner()
	for _, img := range []string{"ghcr.io/example/runner:latest", "alpine"} {
		if code := s.run("", "supervise", "-c", s.cfgPath, "--once", "--image", img); code != cli.ExitFailure || !strings.Contains(s.err.String(), "BR-E001") {
			t.Errorf("image %q: exit %d\n%s", img, code, s.err.String())
		}
	}

	// No image at all.
	cfg, _ := os.ReadFile(s.cfgPath)
	noImage := filepath.Join(filepath.Dir(s.cfgPath), "noimage.yaml")
	if err := os.WriteFile(noImage, []byte(strings.Split(string(cfg), "\nsupervisor:")[0]), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := s.run("", "supervise", "-c", noImage, "--once"); code != cli.ExitFailure || !strings.Contains(s.err.String(), "no runner image") {
		t.Errorf("no image: exit %d\n%s", code, s.err.String())
	}

	// An unpinned image in the config file is a config error naming its key.
	bad := filepath.Join(filepath.Dir(s.cfgPath), "bad.yaml")
	if err := os.WriteFile(bad, []byte(strings.Split(string(cfg), "\nsupervisor:")[0]+"\nsupervisor:\n  image: runner:latest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := s.run("", "supervise", "-c", bad, "--once"); code != cli.ExitFailure || !strings.Contains(s.err.String(), "supervisor.image") {
		t.Errorf("unpinned image in config: exit %d\n%s", code, s.err.String())
	}

	// A second supervisor is refused while one holds the lock.
	unlock, err := core.LockDir(filepath.Join(s.home(), "runners", "e2e-runner", "supervisor"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if code := s.run("", "supervise", "-c", s.cfgPath, "--once"); code != cli.ExitFailure || !strings.Contains(s.err.String(), "BR-E004") {
		t.Errorf("second supervisor: exit %d\n%s", code, s.err.String())
	}
}

func TestSuperviseRejectsBadFlags(t *testing.T) {
	s := newSuperviseRig(t)
	if code := s.run("", "supervise", "-c", s.cfgPath, "--bogus"); code != cli.ExitUsage {
		t.Errorf("unknown flag: exit %d", code)
	}
	if code := s.run("", "supervise", "-c", s.cfgPath, "extra"); code != cli.ExitUsage {
		t.Errorf("stray argument: exit %d", code)
	}
}
