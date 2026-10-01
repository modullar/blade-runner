// Package testrig wires a whole Blade Runner environment for tests: a real filesystem in a
// temp directory, the real GitHub client against the in-process fake GitHub, the real
// config.sh script run as a process, and the real token store. Only the service manager is
// replaced (FakePlatform), because launchd and systemd cannot run inside a test.
package testrig

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/modullar/blade-runner/internal/config"
	"github.com/modullar/blade-runner/internal/core"
	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/download"
	"github.com/modullar/blade-runner/internal/execx"
	"github.com/modullar/blade-runner/internal/install"
	"github.com/modullar/blade-runner/internal/platform"
	"github.com/modullar/blade-runner/internal/provider/github"
	"github.com/modullar/blade-runner/internal/provider/github/githubtest"
	"github.com/modullar/blade-runner/internal/secrets"
	"github.com/modullar/blade-runner/internal/trust"
)

// What githubtest accepts and what BaseConfig names.
const (
	Token      = "ghp_testtoken"
	RunnerName = "test-runner"
	Target     = "acme/widgets"
)

// BaseConfig is a minimal valid config for the fake GitHub's acme/widgets repository.
const BaseConfig = `version: 1
runner:
  scope: repo
  repository: acme/widgets
  name: test-runner
  labels: [gpu]
  token:
    source: file
placement:
  default: auto
`

var ctx = context.Background()

// FakePlatform stands in for launchd/systemd. It keeps the state a service manager would.
type FakePlatform struct {
	Installed, Running bool
	Installs, Starts   int
	Stops              int
	FailStartOnce      bool
	Prereqs            []platform.Prereq
}

func (*FakePlatform) OS() string                           { return "fake" }
func (*FakePlatform) DefinitionPath(platform.Spec) string  { return "/fake/unit" }
func (*FakePlatform) Render(platform.Spec) ([]byte, error) { return []byte("unit"), nil }
func (f *FakePlatform) Stop(context.Context, platform.Spec) error {
	f.Stops++
	f.Running = false
	return nil
}
func (*FakePlatform) Logs(context.Context, platform.Spec, bool, io.Writer) error {
	return nil
}
func (f *FakePlatform) CheckPrereqs(context.Context) []platform.Prereq { return f.Prereqs }
func (f *FakePlatform) Status(context.Context, platform.Spec) (platform.Status, error) {
	return platform.Status{Installed: f.Installed, Current: f.Installed, Running: f.Running, Detail: fmt.Sprintf("running=%v", f.Running)}, nil
}
func (f *FakePlatform) Install(context.Context, platform.Spec) error {
	f.Installs++
	f.Installed = true
	return nil
}
func (f *FakePlatform) Start(context.Context, platform.Spec) error {
	f.Starts++
	if f.FailStartOnce {
		f.FailStartOnce = false
		return diag.New(diag.CodeServiceInstall, "launchd would not start the runner", "test", "test")
	}
	f.Running = true
	return nil
}
func (f *FakePlatform) Uninstall(context.Context, platform.Spec) error {
	f.Installed, f.Running = false, false
	return nil
}

// Recorder is a core.Reporter that keeps what it is told.
type Recorder struct {
	Notes   []string
	Gates   []string
	Results map[string]core.Outcome
}

func (r *Recorder) Note(m string)        { r.Notes = append(r.Notes, m) }
func (r *Recorder) GatePassed(id string) { r.Gates = append(r.Gates, id) }
func (r *Recorder) StepResult(id string, o core.Outcome, _ string) {
	if r.Results == nil {
		r.Results = map[string]core.Outcome{}
	}
	r.Results[id] = o
}

// Rig is the wired environment.
type Rig struct {
	T        *testing.T
	Srv      *githubtest.Server
	Env      *install.Env
	Plat     *FakePlatform
	UserHome string
	// ProjectDir stands for the user's project: where bladerunner.yaml, .github/workflows and
	// the git checkout live. It starts empty: no workflows, no .git.
	ProjectDir string
}

// New builds a Rig for configYAML with the token already stored (except for the env source,
// which the caller sets with t.Setenv).
func New(t *testing.T, configYAML string) *Rig {
	t.Helper()
	srv := githubtest.New()
	t.Cleanup(srv.Close)

	userHome := t.TempDir()
	cfg, err := config.Parse([]byte(configYAML), config.Defaults{Hostname: "host", GOOS: "linux"})
	if err != nil {
		t.Fatal(err)
	}
	layout := install.Layout{Home: filepath.Join(userHome, ".bladerunner"), RunnerName: cfg.Runner.Name}
	store, err := secrets.New(cfg.Runner.Token.Source, secrets.Options{Dir: layout.SecretsDir()})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runner.Token.Source != config.TokenEnv {
		if err := store.Set(ctx, cfg.Runner.Name, Token); err != nil {
			t.Fatal(err)
		}
	}
	plat := &FakePlatform{Prereqs: []platform.Prereq{{Name: "git", OK: true}}}
	projectDir := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	env := &install.Env{
		Cfg: cfg, Layout: layout, UserHome: userHome, GOOS: "linux", GOARCH: "amd64", ProjectDir: projectDir,
		Provider: &github.Client{
			APIURL: srv.URL, WebURL: srv.URL,
			Token: func(ctx context.Context) (string, error) { return store.Get(ctx, cfg.Runner.Name) },
		},
		Platform: plat,
		Secrets:  store,
		Exec:     execx.OS{},
		Fetcher:  &download.Fetcher{AllowHTTP: true},
		State:    &core.StateStore{Path: layout.StateFile()},
		Trust:    &trust.Store{Path: layout.TrustFile()},
		Euid:     func() int { return 1000 },
	}
	env.BinaryPath = Binary(t)
	return &Rig{T: t, Srv: srv, Env: env, Plat: plat, UserHome: userHome, ProjectDir: projectDir}
}

// Apply runs the real apply pipeline.
func (r *Rig) Apply() (changed []string, rep *Recorder, err error) {
	rep = &Recorder{}
	changed, err = install.ApplyEngine(r.Env, rep).Apply(ctx)
	return changed, rep, err
}

// Remove runs the real remove pipeline.
func (r *Rig) Remove() (changed []string, err error) {
	return install.RemoveEngine(r.Env, &Recorder{}).Apply(ctx)
}

// Exists reports whether a path under the user's home exists.
func (r *Rig) Exists(rel string) bool {
	_, err := os.Stat(filepath.Join(r.UserHome, rel))
	return err == nil
}

// Leftovers lists everything under the user's home.
func (r *Rig) Leftovers() []string {
	var out []string
	_ = filepath.WalkDir(r.UserHome, func(p string, d os.DirEntry, err error) error {
		if err == nil && p != r.UserHome {
			rel, _ := filepath.Rel(r.UserHome, p)
			out = append(out, rel)
		}
		return nil
	})
	return out
}

// WriteWorkflow writes .github/workflows/<name> in the project.
func (r *Rig) WriteWorkflow(name, body string) {
	r.T.Helper()
	dir := filepath.Join(r.ProjectDir, ".github", "workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		r.T.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		r.T.Fatal(err)
	}
}

// GitInit makes the project a real git checkout whose origin is remote, using the real git.
func (r *Rig) GitInit(remote string) {
	r.T.Helper()
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", remote}} {
		cmd := exec.Command("git", append([]string{"-C", r.ProjectDir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			r.T.Skipf("git is not usable here: %v: %s", err, out)
		}
	}
}

// GuardedWorkflow is a workflow with one job that can run on the local runner and is locked
// to the trusted actor the way the policy requires.
const GuardedWorkflow = `name: ci
on: [push, pull_request]
jobs:
  test:
    runs-on: self-hosted
    if: github.actor == 'acme' && (github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository)
    steps:
      - run: echo ok
`

// OpenWorkflow is the same job with no guard: anyone's pull request would run on the runner.
const OpenWorkflow = `name: ci
on: [push, pull_request]
jobs:
  test:
    runs-on: self-hosted
    steps:
      - run: echo ok
`

var (
	binOnce sync.Once
	binPath string
	binErr  error
)

// Binary builds the real bladerunner program once per test process and returns its path. The
// job hook script calls the program back, so tests that run the hook need the real thing.
func Binary(t testing.TB) string {
	t.Helper()
	binOnce.Do(func() {
		dir, err := os.MkdirTemp("", "bladerunner-bin-")
		if err != nil {
			binErr = err
			return
		}
		binPath = filepath.Join(dir, "bladerunner")
		if out, err := exec.Command("go", "build", "-o", binPath, "github.com/modullar/blade-runner/cmd/bladerunner").CombinedOutput(); err != nil {
			binErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if binErr != nil {
		t.Fatalf("cannot build the bladerunner binary: %v", binErr)
	}
	return binPath
}

// RealCommit is a commit made by the real `git commit -S` with a real ssh-keygen key (see
// internal/trust/testdata/real), split the way GitHub's API returns it.
type RealCommit struct {
	SHA       string
	Payload   string
	Signature string
	Parents   []string // the parent ids, first parent first
}

// RealFixtures loads the real signed-commit fixtures: commits by "owner" and "mallory" (two
// different keys) and an "unsigned" one, plus each signer's public key line.
func RealFixtures(t testing.TB) (commits map[string]RealCommit, pubkeys map[string]string) {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(file), "..", "trust", "testdata", "real")
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	commits, pubkeys = map[string]RealCommit{}, map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(string(read("shas.txt"))), "\n") {
		f := strings.Fields(l)
		payload, sig, err := trust.SplitSignedCommit(read(f[0] + "-commit.raw"))
		if err != nil {
			t.Fatal(err)
		}
		commits[f[0]] = RealCommit{SHA: f[1], Payload: string(payload), Signature: sig}
	}
	for _, who := range []string{"owner", "mallory"} {
		pubkeys[who] = strings.TrimSpace(string(read(who + ".pub")))
	}
	return commits, pubkeys
}

// ServeCommits publishes the real fixtures on the fake GitHub for repo.
func (r *Rig) ServeCommits(repo string) map[string]RealCommit {
	r.T.Helper()
	commits, _ := RealFixtures(r.T)
	for _, c := range commits {
		r.Srv.AddCommit(repo, c.SHA, c.Payload, c.Signature)
	}
	return commits
}
