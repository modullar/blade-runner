// Package hook is the enforcement that lives on the machine: the decision the runner's
// "job started" hook makes before any step of a job runs.
//
// Why here and not in the workflow: a workflow file is written by whoever opens the pull
// request or pushes the branch, so a guard inside it can be deleted by exactly the person it
// is meant to stop. This policy sits on the owner's disk, outside any repository, and is
// consulted before untrusted steps start. Anything it cannot determine is refused.
//
// ASSUMPTIONS, unverified until BR-0 (see docs/decisions/0005): the runner supports
// ACTIONS_RUNNER_HOOK_JOB_STARTED; fails the job when the hook exits non-zero; and provides
// the standard GITHUB_* variables and GITHUB_EVENT_PATH to it.
package hook

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Policy is written by `apply` next to the hook script.
type Policy struct {
	Version       int      `json:"version"`
	Scope         string   `json:"scope"`      // "repo" or "org"
	Repository    string   `json:"repository"` // OWNER/REPO, scope repo
	Organization  string   `json:"organization"`
	TrustedActors []string `json:"trusted_actors"`
}

// allowedEvents can only be caused by someone with write access to the repository, or carry
// no outside input. pull_request is allowed only for branches of the repository itself.
var allowedEvents = map[string]bool{
	"push": true, "workflow_dispatch": true, "schedule": true, "merge_group": true, "pull_request": true,
}

// Decision is the verdict and the reason, which is shown in the failed job's log.
type Decision struct {
	Allow  bool
	Reason string
}

func deny(format string, args ...any) Decision { return Decision{Reason: fmt.Sprintf(format, args...)} }

// Evaluate decides whether the job described by env may run. env is the hook's environment
// (name -> value); readFile reads the event payload.
func Evaluate(p Policy, env map[string]string, readFile func(string) ([]byte, error)) Decision {
	if p.Version != 1 || len(p.TrustedActors) == 0 {
		return deny("the job policy is missing or unusable, so no job is allowed")
	}
	repo, actor, event := env["GITHUB_REPOSITORY"], env["GITHUB_ACTOR"], env["GITHUB_EVENT_NAME"]
	if repo == "" || actor == "" || event == "" {
		return deny("the job did not say which repository, actor and event it is for (GITHUB_REPOSITORY=%q GITHUB_ACTOR=%q GITHUB_EVENT_NAME=%q)", repo, actor, event)
	}

	switch p.Scope {
	case "repo":
		if !strings.EqualFold(repo, p.Repository) {
			return deny("this runner is for %s, not %s", p.Repository, repo)
		}
	case "org":
		if owner, _, ok := strings.Cut(repo, "/"); !ok || !strings.EqualFold(owner, p.Organization) {
			return deny("this runner is for the organization %s, not %s", p.Organization, repo)
		}
	default:
		return deny("the job policy has an unknown scope %q", p.Scope)
	}

	if !trusted(p, actor) {
		return deny("%s is not a trusted actor for this runner (trusted: %s)", actor, strings.Join(p.TrustedActors, ", "))
	}
	// A re-run is attributed to the person who clicked it as well: both must be trusted.
	if trig := env["GITHUB_TRIGGERING_ACTOR"]; trig != "" && !trusted(p, trig) {
		return deny("%s triggered this run and is not a trusted actor for this runner", trig)
	}
	if !allowedEvents[event] {
		return deny("events of type %q can be caused by people outside your control and never run on this machine", event)
	}
	if event == "pull_request" {
		return pullRequest(env, readFile, repo)
	}
	return Decision{Allow: true, Reason: fmt.Sprintf("%s's %s on %s", actor, event, repo)}
}

func trusted(p Policy, actor string) bool {
	for _, t := range p.TrustedActors {
		if strings.EqualFold(actor, t) { // GitHub logins are case-insensitive
			return true
		}
	}
	return false
}

// pullRequest allows a pull request only when its head is a branch of this same repository.
// A trusted actor who updates someone else's fork branch would otherwise run that code here.
func pullRequest(env map[string]string, readFile func(string) ([]byte, error), repo string) Decision {
	path := env["GITHUB_EVENT_PATH"]
	if path == "" {
		return deny("a pull_request job without an event payload cannot be checked, so it is refused")
	}
	data, err := readFile(path)
	if err != nil {
		return deny("the pull request's event payload could not be read (%v), so it is refused", err)
	}
	var ev struct {
		PullRequest struct {
			Head struct {
				Repo *struct {
					FullName string `json:"full_name"`
				} `json:"repo"`
			} `json:"head"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return deny("the pull request's event payload is not valid JSON (%v), so it is refused", err)
	}
	head := ev.PullRequest.Head.Repo
	switch {
	case head == nil || head.FullName == "":
		return deny("the pull request's head repository is unknown (a deleted fork?), so it is refused")
	case !strings.EqualFold(head.FullName, repo):
		return deny("this pull request comes from %s, not from %s itself: code from a fork never runs on this machine", head.FullName, repo)
	}
	return Decision{Allow: true, Reason: "a pull request from a branch of " + repo}
}

// LoadPolicy reads a policy file.
func LoadPolicy(path string) (Policy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Policy{}, err
	}
	var p Policy
	if err := json.Unmarshal(data, &p); err != nil {
		return Policy{}, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// EnvMap turns os.Environ() into a map.
func EnvMap(environ []string) map[string]string {
	m := make(map[string]string, len(environ))
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return m
}
