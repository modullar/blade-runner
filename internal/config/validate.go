package config

import (
	"fmt"
	"net"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/modullar/blade-runner/internal/version"
)

var (
	nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	repoRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})/[A-Za-z0-9._-]{1,100}$`)
	orgRe  = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
)

// Validate checks a defaulted Config and returns every problem, each with its key path.
func Validate(c *Config) []Problem {
	var ps []Problem
	add := func(p, format string, args ...any) {
		ps = append(ps, Problem{Path: p, Msg: fmt.Sprintf(format, args...)})
	}

	if c.Version != 1 {
		add("version", "must be 1 (got %d)", c.Version)
	}
	if v := c.BladeRunner.MinVersion; v != "" {
		if _, err := version.Parse(v); err != nil {
			add("bladerunner.min_version", "%v", err)
		}
	}

	r := c.Runner
	switch r.Scope {
	case ScopeRepo:
		switch {
		case r.Repository == "":
			add("runner.repository", "required when runner.scope is repo")
		case !repoRe.MatchString(r.Repository):
			add("runner.repository", "%q is not OWNER/REPO", r.Repository)
		}
		if r.Organization != "" {
			add("runner.organization", "not allowed when runner.scope is repo")
		}
	case ScopeOrg:
		switch {
		case r.Organization == "":
			add("runner.organization", "required when runner.scope is org")
		case !orgRe.MatchString(r.Organization):
			add("runner.organization", "%q is not a valid organization name", r.Organization)
		}
		if r.Repository != "" {
			add("runner.repository", "not allowed when runner.scope is org")
		}
	case "":
		add("runner.scope", "required: %s or %s", ScopeRepo, ScopeOrg)
	default:
		add("runner.scope", "must be %s or %s (got %q)", ScopeRepo, ScopeOrg, r.Scope)
	}
	if !nameRe.MatchString(r.Name) {
		add("runner.name", "%q must be 1-64 letters, digits, '.', '_' or '-', starting with a letter or digit", r.Name)
	}
	for i, l := range r.Labels {
		checkLabel(fmt.Sprintf("runner.labels[%d]", i), l, add)
	}
	if len(r.TrustedActors) == 0 {
		add("runner.trusted_actors", "required: list the GitHub logins whose code may run on this machine (for scope org there is no default)")
	}
	for i, a := range r.TrustedActors {
		if !orgRe.MatchString(a) {
			add(fmt.Sprintf("runner.trusted_actors[%d]", i), "%q is not a GitHub login", a)
		}
	}
	if strings.TrimSpace(r.WorkDir) == "" {
		add("runner.work_dir", "must not be empty")
	}
	switch r.Token.Source {
	case TokenKeychain, TokenEnv, TokenFile:
	default:
		add("runner.token.source", "must be %s, %s or %s (got %q)", TokenKeychain, TokenEnv, TokenFile, r.Token.Source)
	}

	checkPlacement("placement.default", c.Placement.Default, add)
	for i, l := range c.Placement.GitHubLabels {
		checkLabel(fmt.Sprintf("placement.github_labels[%d]", i), l, add)
	}
	for _, job := range c.Placement.JobOrder {
		if strings.TrimSpace(job) == "" {
			add("placement.jobs", "job names must not be empty")
		}
		checkPlacement("placement.jobs."+job, c.Placement.Jobs[job], add)
	}

	for i, w := range c.Workflows {
		key := fmt.Sprintf("workflows[%d]", i)
		clean := path.Clean(w)
		switch {
		case strings.TrimSpace(w) == "":
			add(key, "must not be empty")
		case path.IsAbs(w) || clean == ".." || strings.HasPrefix(clean, "../"):
			add(key, "%q must be a path inside the project", w)
		}
	}

	checkListen(c.Agent.Listen, add)
	if c.Agent.PollActiveSeconds < 1 {
		add("agent.poll_active_seconds", "must be at least 1")
	}
	if c.Agent.PollIdleSeconds < c.Agent.PollActiveSeconds {
		add("agent.poll_idle_seconds", "must be at least agent.poll_active_seconds (%d)", c.Agent.PollActiveSeconds)
	}
	return ps
}

func checkLabel(key, l string, add func(string, string, ...any)) {
	switch {
	case strings.TrimSpace(l) == "":
		add(key, "must not be empty")
	case strings.ContainsAny(l, ","):
		add(key, "%q must not contain a comma", l)
	case l != strings.TrimSpace(l):
		add(key, "%q has leading or trailing space", l)
	}
}

func checkPlacement(key string, p Placement, add func(string, string, ...any)) {
	switch p {
	case PlaceLocal, PlaceGitHub, PlaceAuto:
	default:
		add(key, "must be local, github or auto (got %q)", string(p))
	}
}

// checkListen enforces that the agent is loopback-only (spec section 10). An empty host
// would bind every interface, so it is refused too.
func checkListen(listen string, add func(string, string, ...any)) {
	const key = "agent.listen"
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		add(key, "%q is not host:port", listen)
		return
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		add(key, "port %q is not 1-65535", port)
	}
	if host == "localhost" {
		return
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		add(key, "%q is not a loopback address: the agent must listen on 127.0.0.1, ::1 or localhost in v1", host)
	}
}
