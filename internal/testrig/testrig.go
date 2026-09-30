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
	"path/filepath"
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
	FailStartOnce      bool
	Prereqs            []platform.Prereq
}

func (*FakePlatform) OS() string                                { return "fake" }
func (*FakePlatform) DefinitionPath(platform.Spec) string       { return "/fake/unit" }
func (*FakePlatform) Render(platform.Spec) ([]byte, error)      { return []byte("unit"), nil }
func (*FakePlatform) Stop(context.Context, platform.Spec) error { return nil }
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
	env := &install.Env{
		Cfg: cfg, Layout: layout, UserHome: userHome, GOOS: "linux", GOARCH: "amd64",
		Provider: &github.Client{
			APIURL: srv.URL, WebURL: srv.URL,
			Token: func(ctx context.Context) (string, error) { return store.Get(ctx, cfg.Runner.Name) },
		},
		Platform: plat,
		Secrets:  store,
		Exec:     execx.OS{},
		Fetcher:  &download.Fetcher{AllowHTTP: true},
		State:    &core.StateStore{Path: layout.StateFile()},
		Euid:     func() int { return 1000 },
	}
	return &Rig{T: t, Srv: srv, Env: env, Plat: plat, UserHome: userHome}
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
