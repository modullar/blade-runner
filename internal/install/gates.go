package install

import (
	"context"
	"fmt"
	"strings"

	"github.com/modullar/blade-runner/internal/config"
	"github.com/modullar/blade-runner/internal/core"
	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/provider"
)

// ApplyGates are the read-only preconditions of `apply`, in the order they run. A failing
// gate stops the run before anything changes, in dry-run too.
func ApplyGates(e *Env) []core.Gate {
	return []core.Gate{
		notRoot{e}, prereqs{e}, tokenPresent{e}, tokenValid{e}, publicRepo{e},
	}
}

type notRoot struct{ e *Env }

func (notRoot) ID() string { return "not-root" }
func (g notRoot) Run(context.Context, core.Reporter) error {
	if g.e.Euid != nil && g.e.Euid() == 0 {
		return diag.New(diag.CodeRunningAsRoot, "bladerunner is running as root",
			"the runner must run as your own user: a job that runs as root owns the whole machine (least privilege, spec section 10)",
			"run it again without sudo")
	}
	return nil
}

type prereqs struct{ e *Env }

func (prereqs) ID() string { return "prerequisites" }
func (g prereqs) Run(ctx context.Context, _ core.Reporter) error {
	var failed []string
	var fixes []string
	for _, p := range g.e.Platform.CheckPrereqs(ctx) {
		if !p.OK {
			failed = append(failed, p.Name)
			fixes = append(fixes, fmt.Sprintf("%s: %s", p.Name, p.Fix))
		}
	}
	if len(failed) == 0 {
		return nil
	}
	return diag.New(diag.CodePrereqMissing, "missing prerequisites: "+strings.Join(failed, ", "),
		"this machine is not set up for a self-hosted runner yet",
		"\n    "+strings.Join(fixes, "\n    "))
}

type tokenPresent struct{ e *Env }

func (tokenPresent) ID() string { return "token-present" }
func (g tokenPresent) Run(ctx context.Context, _ core.Reporter) error {
	_, err := g.e.Token(ctx)
	return err
}

type tokenValid struct{ e *Env }

func (tokenValid) ID() string { return "token-valid" }
func (g tokenValid) Run(ctx context.Context, _ core.Reporter) error {
	return g.e.Provider.CheckAuth(ctx, g.e.Scope())
}

// publicRepo refuses to register a runner to a public repository without the explicit
// opt-in: anyone can open a fork pull request, and a self-hosted runner would execute
// their code on this machine (spec section 10).
type publicRepo struct{ e *Env }

func (publicRepo) ID() string { return "public-repo-guard" }
func (g publicRepo) Run(ctx context.Context, rep core.Reporter) error {
	cfg := g.e.Cfg
	if cfg.Runner.Scope == config.ScopeOrg {
		rep.Note("organization scope: Blade Runner cannot tell which repositories may use this runner. " +
			"Make sure its runner group excludes public repositories.")
		return nil
	}
	vis, err := g.e.Provider.Visibility(ctx, cfg.Runner.Repository)
	if err != nil {
		return diag.Wrap(err, diag.CodeVisibilityUnknown,
			fmt.Sprintf("cannot tell whether %s is public", cfg.Runner.Repository),
			"GitHub could not be reached, or the token cannot read the repository",
			"fix the problem above and re-run: Blade Runner refuses to guess when a public repository is at stake")
	}
	return PublicRepoDecision(cfg.Runner.Repository, vis, g.e.AllowPublic)
}

// PublicRepoDecision is the guard's rule, shared by init and apply: a public repository is
// refused unless the user opted in.
func PublicRepoDecision(repo string, vis provider.Visibility, allow bool) error {
	if vis != provider.Public || allow {
		return nil
	}
	return diag.New(diag.CodePublicRepoRefused,
		fmt.Sprintf("%s is a public repository: refusing to set up a self-hosted runner for it", repo),
		"anyone can open a pull request from a fork, and a self-hosted runner would run their code on this machine",
		"use GitHub-hosted runners for public repositories (placement: github); if you understand the risk and trust every contributor, re-run with --allow-public-runner")
}
