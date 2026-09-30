// Package install holds the concrete gates and steps behind `apply` and `remove`: fetch and
// verify the runner, register it, install and start its service, and undo all of it.
// Each unit does one thing and is injected with the collaborators it needs.
package install

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/modullar/blade-runner/internal/config"
	"github.com/modullar/blade-runner/internal/core"
	"github.com/modullar/blade-runner/internal/download"
	"github.com/modullar/blade-runner/internal/execx"
	"github.com/modullar/blade-runner/internal/platform"
	"github.com/modullar/blade-runner/internal/provider"
	"github.com/modullar/blade-runner/internal/secrets"
	"github.com/modullar/blade-runner/internal/trust"
)

// Layout is where one runner's files live: everything for runner NAME sits under
// Home/runners/NAME, so two runners on one machine never share a path, and removing one
// is deleting one directory.
type Layout struct {
	Home       string // ~/.bladerunner, or $BLADERUNNER_HOME
	RunnerName string
}

func (l Layout) RunnerHome() string  { return filepath.Join(l.Home, "runners", l.RunnerName) }
func (l Layout) RunnerDir() string   { return filepath.Join(l.RunnerHome(), "runner") }
func (l Layout) LogDir() string      { return filepath.Join(l.RunnerHome(), "logs") }
func (l Layout) DownloadDir() string { return filepath.Join(l.RunnerHome(), "downloads") }
func (l Layout) StateFile() string   { return filepath.Join(l.RunnerHome(), "state.json") }
func (l Layout) HooksDir() string    { return filepath.Join(l.RunnerHome(), "hooks") }
func (l Layout) TrustFile() string   { return filepath.Join(l.RunnerHome(), "trust.json") }
func (l Layout) SecretsDir() string  { return filepath.Join(l.Home, "secrets") }

// ExpandHome turns a leading ~ into the user's home directory.
func ExpandHome(path, home string) string {
	switch {
	case path == "~":
		return home
	case strings.HasPrefix(path, "~/"):
		return filepath.Join(home, path[2:])
	}
	return path
}

// Env is everything apply, remove and doctor need, injected so each piece is testable.
type Env struct {
	Cfg      *config.Config
	Layout   Layout
	UserHome string // the user's home, for expanding ~ in config
	GOOS     string
	GOARCH   string

	Provider provider.Provider
	Platform platform.Platform
	Secrets  secrets.Store
	Exec     execx.Runner
	Fetcher  *download.Fetcher
	State    *core.StateStore
	// Trust holds the public keys whose signed commits may run here.
	Trust *trust.Store

	// BinaryPath is this program's own path: the job hook script calls it back (`bladerunner
	// hook job-started`), so it is recorded in the script and must be stable.
	BinaryPath string

	// ProjectDir is the directory holding bladerunner.yaml and, usually, the git checkout and
	// .github/workflows that the policy in policy.go inspects. Empty disables those checks.
	ProjectDir string

	Euid func() int // effective uid; 0 means root
	// AllowPublic is the explicit opt-in for registering a runner to a public repository.
	AllowPublic bool
	// AllowRepoMismatch lets apply proceed when the checkout's origin is not the configured
	// repository. ForkApprovalConfirmed is the user's word that the fork-approval setting is
	// strict, for when the API cannot confirm it.
	AllowRepoMismatch     bool
	ForkApprovalConfirmed bool
	// SkipDeregister makes remove leave the GitHub registration alone, for a machine that
	// cannot reach GitHub; the runner must then be deleted in GitHub's settings.
	SkipDeregister bool
}

// Scope converts the config's runner scope for the provider.
func (e *Env) Scope() provider.Scope {
	r := e.Cfg.Runner
	return provider.Scope{Kind: r.Scope, Repository: r.Repository, Organization: r.Organization}
}

// WorkDir is the configured work directory with ~ expanded.
func (e *Env) WorkDir() string { return ExpandHome(e.Cfg.Runner.WorkDir, e.UserHome) }

// Spec is the service definition's input.
func (e *Env) Spec() platform.Spec {
	return platform.Spec{
		RunnerName: e.Cfg.Runner.Name,
		RunnerDir:  e.Layout.RunnerDir(),
		LogDir:     e.Layout.LogDir(),
		Home:       e.UserHome,
	}
}

// Token reads the GitHub token from the configured store.
func (e *Env) Token(ctx context.Context) (string, error) {
	return e.Secrets.Get(ctx, e.Cfg.Runner.Name)
}
