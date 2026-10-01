package install

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/modullar/blade-runner/internal/config"
	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/execx"
	"github.com/modullar/blade-runner/internal/guard"
	"github.com/modullar/blade-runner/internal/provider"
)

// This file is the "only my code runs on my machine" policy, shared by the apply gates and by
// doctor so both judge the same way. The layers:
//
//	enforcement (jobhook.go, internal/hook): the runner runs a hook on this machine before
//	every job and refuses anyone but the trusted actors. An attacker cannot edit it.
//
//	defence in depth, checked here:
//	1. workflows: every job that can land on this runner is locked to a trusted actor. An
//	   attacker can delete this in their own copy of the file, so it is NOT the control;
//	2. checkout: the project this config sits in is the repository it registers a runner
//	   for, so a fork or copy cannot inherit someone else's runner setup;
//	3. fork approval: on a public repository, outside contributors' runs wait for approval.

// GuardPolicy builds the workflow policy from the config.
func (e *Env) GuardPolicy() guard.Policy {
	return guard.Policy{
		TrustedActors: e.Cfg.Runner.TrustedActors,
		LocalLabels:   e.Cfg.Runner.AllLabels(e.GOOS, e.GOARCH),
	}
}

// WorkflowReport scans the project's workflow files. A project with none passes.
func (e *Env) WorkflowReport() (guard.Report, error) {
	if e.ProjectDir == "" {
		return guard.Report{}, nil
	}
	return guard.ScanDir(e.ProjectDir, e.GuardPolicy())
}

// WorkflowPolicyError turns findings into the refusal shown to the user, or nil.
func WorkflowPolicyError(rep guard.Report, actors []string) error {
	if len(rep.Findings) == 0 {
		return nil
	}
	lines := make([]string, len(rep.Findings))
	for i, f := range rep.Findings {
		lines[i] = f.String()
	}
	return diag.New(diag.CodeWorkflowPolicy,
		fmt.Sprintf("%d workflow problem(s) would let someone else's code run on this machine", len(rep.Findings)),
		"a job that can land on your runner is not locked to "+strings.Join(actors, ", ")+"; a pull request from a fork, or a push by another collaborator, would run there\n    "+strings.Join(lines, "\n    "),
		"add the `if:` shown for each job (and move jobs off unsafe triggers), then re-run; `bladerunner generate` will emit this for you in BR-3")
}

var remoteRe = regexp.MustCompile(`^(?:[a-z+]+://)?(?:[^@/]+@)?([^/:]+)(?::\d+)?[:/]+([^/]+)/([^/]+?)(?:\.git)?/?$`)

// parseRemote reads host, owner and repo from a git remote URL: https, ssh://, scp-style.
func parseRemote(raw string) (host, owner, repo string, ok bool) {
	raw = strings.TrimSpace(raw)
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) != 2 {
			return "", "", "", false
		}
		return u.Host, parts[0], strings.TrimSuffix(parts[1], ".git"), true
	}
	m := remoteRe.FindStringSubmatch(raw)
	if m == nil {
		return "", "", "", false
	}
	return m[1], m[2], m[3], true
}

// CheckoutMismatch compares the project's git origin with the repository the config
// registers a runner for. It returns a note when the comparison is not possible (no git
// checkout, no origin, another host) and an error when the checkout is a different repository.
func (e *Env) CheckoutMismatch(ctx context.Context) (note string, err error) {
	if e.ProjectDir == "" {
		return "", nil
	}
	if _, statErr := os.Stat(filepath.Join(e.ProjectDir, ".git")); statErr != nil {
		return "not a git checkout: cannot confirm this is the repository the runner is for", nil
	}
	res, runErr := e.Exec.Run(ctx, execx.Cmd{Name: "git", Args: []string{"-C", e.ProjectDir, "remote", "get-url", "origin"}})
	if runErr != nil {
		return "this checkout has no `origin` remote: cannot confirm it is the repository the runner is for", nil
	}
	host, owner, repo, ok := parseRemote(res.Stdout)
	if !ok {
		return "the `origin` remote is not a recognizable repository URL: cannot confirm it matches", nil
	}
	want, err2 := url.Parse(e.Provider.RegistrationURL(e.Scope()))
	if err2 != nil || !strings.EqualFold(host, want.Host) {
		return "the `origin` remote is on " + host + ", not the provider's host: cannot confirm it matches", nil
	}
	cfg := e.Cfg.Runner
	var match bool
	var wantName, gotName string
	if cfg.Scope == config.ScopeOrg {
		wantName, gotName, match = cfg.Organization, owner, strings.EqualFold(owner, cfg.Organization)
	} else {
		wantName, gotName = cfg.Repository, owner+"/"+repo
		match = strings.EqualFold(gotName, cfg.Repository)
	}
	if match {
		return "", nil
	}
	return "", diag.New(diag.CodeRepoMismatch,
		fmt.Sprintf("this checkout is %s, but bladerunner.yaml registers a runner for %s", gotName, wantName),
		"this looks like a fork or a copy of someone else's project: a runner is one person's machine for one repository, and each person sets up their own",
		fmt.Sprintf("run `bladerunner init --force --repository %s` to set up your own repository (with your own token), or pass --allow-repo-mismatch if this is intentional", gotName))
}

// ForkApprovalError checks the fork-approval setting of a PUBLIC repository. It returns nil
// when the strictest policy is confirmed by the API, or when the user has confirmed it by hand
// (confirmed); otherwise the refusal.
func (e *Env) ForkApprovalError(ctx context.Context, confirmed bool) error {
	repo := e.Cfg.Runner.Repository
	policy, err := e.Provider.ForkApprovalPolicy(ctx, repo)
	switch {
	case err == nil && policy == provider.StrictForkApproval:
		return nil
	case err == nil:
		return diag.New(diag.CodeForkApproval,
			fmt.Sprintf("%s lets some outside contributors' workflows run without approval (policy: %s)", repo, policy),
			"on a public repository, a first-time contributor's pull request would start running immediately",
			"set Settings > Actions > General > \"Fork pull request workflows from outside collaborators\" to \"Require approval for all outside collaborators\", then re-run")
	case confirmed:
		return nil
	}
	return diag.Wrap(err, diag.CodeForkApproval,
		fmt.Sprintf("cannot verify the fork pull request approval setting of %s", repo),
		"the API did not answer (the endpoint used is unverified, see docs/decisions/0005), so Blade Runner cannot see it",
		"set Settings > Actions > General > \"Fork pull request workflows from outside collaborators\" to \"Require approval for all outside collaborators\", then re-run with --fork-approval-confirmed")
}
