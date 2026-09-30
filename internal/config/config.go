// Package config defines bladerunner.yaml: the schema, strict loading (unknown keys are
// errors that name their key path), validation, defaults and rendering for `init`.
package config

import (
	"strings"
)

// Placement is where a job runs (spec section 7).
type Placement string

const (
	PlaceLocal  Placement = "local"
	PlaceGitHub Placement = "github"
	PlaceAuto   Placement = "auto"
)

// Scope is the level a runner registers at.
const (
	ScopeRepo = "repo"
	ScopeOrg  = "org"
)

// Token sources: how the tool reads the GitHub token.
const (
	TokenKeychain = "keychain"
	TokenEnv      = "env"
	TokenFile     = "file"
)

// Config is the parsed, defaulted and validated bladerunner.yaml.
type Config struct {
	Version     int
	BladeRunner BladeRunner
	Runner      Runner
	Placement   PlacementConfig
	Workflows   []string
	Agent       Agent
}

// BladeRunner carries the tool's own pin.
type BladeRunner struct {
	MinVersion string
}

// Runner describes the self-hosted runner.
type Runner struct {
	Scope        string
	Repository   string // OWNER/REPO, scope=repo
	Organization string // scope=org
	Name         string
	Labels       []string
	WorkDir      string
	Token        Token
}

// Token says how the tool reads its GitHub token. The token itself is never in the file.
type Token struct {
	Source string
}

// PlacementConfig is the per-job placement model.
type PlacementConfig struct {
	Default      Placement
	GitHubLabels []string
	Jobs         map[string]Placement
	JobOrder     []string // Jobs' keys in file order, so rendering is stable
}

// Agent configures the local agent (BR-4); parsed now so the schema is complete.
type Agent struct {
	Listen            string
	PollActiveSeconds int
	PollIdleSeconds   int
}

// Defaults are the machine facts the loader needs to fill omitted keys.
type Defaults struct {
	Hostname string
	GOOS     string
}

// Target is the human name of what the runner registers to: OWNER/REPO or the org.
func (r Runner) Target() string {
	if r.Scope == ScopeOrg {
		return r.Organization
	}
	return r.Repository
}

// Labels returns the runner's full label set: self-hosted, the OS and the architecture
// first (always present, in that order), then the configured extras in file order.
// Comparison is case-insensitive, as GitHub's is.
func (r Runner) AllLabels(goos, goarch string) []string {
	out := implicitLabels(goos, goarch)
	seen := map[string]bool{}
	for _, l := range out {
		seen[strings.ToLower(l)] = true
	}
	for _, l := range r.Labels {
		if !seen[strings.ToLower(l)] {
			seen[strings.ToLower(l)] = true
			out = append(out, l)
		}
	}
	return out
}

// ExtraLabels is AllLabels without the ones the runner adds to itself, which is what
// `config.sh --labels` should be given.
func (r Runner) ExtraLabels(goos, goarch string) []string {
	extra := r.AllLabels(goos, goarch)[len(implicitLabels(goos, goarch)):]
	if len(extra) == 0 {
		return nil
	}
	return extra
}

func implicitLabels(goos, goarch string) []string {
	out := []string{"self-hosted"}
	switch goos {
	case "darwin":
		out = append(out, "macOS")
	case "linux":
		out = append(out, "Linux")
	}
	switch goarch {
	case "arm64":
		out = append(out, "ARM64")
	case "amd64":
		out = append(out, "X64")
	}
	return out
}

// PlacementFor resolves a job's placement: the explicit per-job value, then the default.
func (p PlacementConfig) PlacementFor(job string) Placement {
	if v, ok := p.Jobs[job]; ok {
		return v
	}
	return p.Default
}

// HasLocalOnlyJobs reports whether any job is pinned to the local runner: those queue
// instead of falling back when the runner is down (spec section 7).
func (p PlacementConfig) HasLocalOnlyJobs() bool {
	if p.Default == PlaceLocal {
		return true
	}
	for _, v := range p.Jobs {
		if v == PlaceLocal {
			return true
		}
	}
	return false
}
