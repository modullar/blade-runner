package hook

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var repoPolicy = Policy{Version: 1, Scope: "repo", Repository: "heron/app", TrustedActors: []string{"heron"}}

// Real event payloads, abridged to the fields GitHub sends that matter here.
const (
	prFromSameRepo = `{"action":"synchronize","pull_request":{"number":7,"head":{"ref":"feature","repo":{"full_name":"heron/app","fork":false}},"base":{"repo":{"full_name":"heron/app"}}}}`
	prFromFork     = `{"action":"synchronize","pull_request":{"number":8,"head":{"ref":"main","repo":{"full_name":"mallory/app","fork":true}},"base":{"repo":{"full_name":"heron/app"}}}}`
	prDeletedFork  = `{"action":"opened","pull_request":{"number":9,"head":{"ref":"x","repo":null}}}`
)

func files(content map[string]string) func(string) ([]byte, error) {
	return func(p string) ([]byte, error) {
		if c, ok := content[p]; ok {
			return []byte(c), nil
		}
		return nil, os.ErrNotExist
	}
}

func env(kv ...string) map[string]string {
	m := map[string]string{"GITHUB_REPOSITORY": "heron/app", "GITHUB_ACTOR": "heron", "GITHUB_EVENT_NAME": "push"}
	for i := 0; i < len(kv); i += 2 {
		if kv[i+1] == "" {
			delete(m, kv[i])
		} else {
			m[kv[i]] = kv[i+1]
		}
	}
	return m
}

func TestOwnersOwnWorkIsAllowed(t *testing.T) {
	for name, e := range map[string]map[string]string{
		"push":           env(),
		"manual run":     env("GITHUB_EVENT_NAME", "workflow_dispatch"),
		"schedule":       env("GITHUB_EVENT_NAME", "schedule"),
		"merge queue":    env("GITHUB_EVENT_NAME", "merge_group"),
		"different case": env("GITHUB_ACTOR", "Heron", "GITHUB_REPOSITORY", "Heron/App"),
		"trusted re-run": env("GITHUB_TRIGGERING_ACTOR", "heron"),
		"own branch PR":  env("GITHUB_EVENT_NAME", "pull_request", "GITHUB_EVENT_PATH", "/ev.json"),
	} {
		if d := Evaluate(repoPolicy, e, files(map[string]string{"/ev.json": prFromSameRepo})); !d.Allow {
			t.Errorf("%s was refused: %s", name, d.Reason)
		}
	}
}

func TestSomeoneElsesWorkIsRefused(t *testing.T) {
	read := files(map[string]string{"/fork.json": prFromFork, "/gone.json": prDeletedFork, "/same.json": prFromSameRepo, "/bad.json": "not json"})
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"a stranger's push to a fork's branch of the repo", env("GITHUB_ACTOR", "mallory"), "not a trusted actor"},
		{"another collaborator's push", env("GITHUB_ACTOR", "alice"), "not a trusted actor"},
		{"a stranger's fork pull request", env("GITHUB_ACTOR", "mallory", "GITHUB_EVENT_NAME", "pull_request", "GITHUB_EVENT_PATH", "/fork.json"), "not a trusted actor"},
		{"I update a stranger's fork PR: trusted actor, fork head", env("GITHUB_EVENT_NAME", "pull_request", "GITHUB_EVENT_PATH", "/fork.json"), "comes from mallory/app"},
		{"a stranger re-runs with my actor", env("GITHUB_TRIGGERING_ACTOR", "mallory"), "triggered this run"},
		{"another repository", env("GITHUB_REPOSITORY", "mallory/app"), "runner is for heron/app"},
		{"a similarly named repository", env("GITHUB_REPOSITORY", "heron/app-evil"), "runner is for heron/app"},
		{"issue comment", env("GITHUB_EVENT_NAME", "issue_comment"), "outside your control"},
		{"pull_request_target", env("GITHUB_EVENT_NAME", "pull_request_target"), "outside your control"},
		{"workflow_run", env("GITHUB_EVENT_NAME", "workflow_run"), "outside your control"},
		{"PR with no payload path", env("GITHUB_EVENT_NAME", "pull_request"), "without an event payload"},
		{"PR payload unreadable", env("GITHUB_EVENT_NAME", "pull_request", "GITHUB_EVENT_PATH", "/missing.json"), "could not be read"},
		{"PR payload not JSON", env("GITHUB_EVENT_NAME", "pull_request", "GITHUB_EVENT_PATH", "/bad.json"), "not valid JSON"},
		{"PR from a deleted fork", env("GITHUB_EVENT_NAME", "pull_request", "GITHUB_EVENT_PATH", "/gone.json"), "head repository is unknown"},
		{"no actor", env("GITHUB_ACTOR", ""), "did not say"},
		{"no repository", env("GITHUB_REPOSITORY", ""), "did not say"},
		{"no event", env("GITHUB_EVENT_NAME", ""), "did not say"},
		{"actor that only contains a trusted name", env("GITHUB_ACTOR", "heron-evil"), "not a trusted actor"},
		{"bot with the same prefix", env("GITHUB_ACTOR", "heron[bot]"), "not a trusted actor"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := Evaluate(repoPolicy, tc.env, read)
			if d.Allow {
				t.Fatalf("was allowed: %s", d.Reason)
			}
			if !strings.Contains(d.Reason, tc.want) {
				t.Errorf("reason %q does not mention %q", d.Reason, tc.want)
			}
		})
	}
}

func TestABrokenPolicyAllowsNothing(t *testing.T) {
	for name, p := range map[string]Policy{
		"zero":          {},
		"wrong version": {Version: 2, Scope: "repo", Repository: "heron/app", TrustedActors: []string{"heron"}},
		"no actors":     {Version: 1, Scope: "repo", Repository: "heron/app"},
		"unknown scope": {Version: 1, Scope: "galaxy", Repository: "heron/app", TrustedActors: []string{"heron"}},
	} {
		if d := Evaluate(p, env(), files(nil)); d.Allow {
			t.Errorf("%s: a policy that cannot be trusted must allow nothing", name)
		}
	}
}

func TestOrganizationScopeChecksTheOwner(t *testing.T) {
	p := Policy{Version: 1, Scope: "org", Organization: "acme", TrustedActors: []string{"alice"}}
	e := env("GITHUB_ACTOR", "alice", "GITHUB_REPOSITORY", "acme/anything")
	if d := Evaluate(p, e, files(nil)); !d.Allow {
		t.Errorf("a repository of the organization: %s", d.Reason)
	}
	for _, repo := range []string{"mallory/anything", "acme-evil/x", "acme"} {
		if d := Evaluate(p, env("GITHUB_ACTOR", "alice", "GITHUB_REPOSITORY", repo), files(nil)); d.Allow {
			t.Errorf("%s must be refused", repo)
		}
	}
}

func TestPolicyFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"scope":"repo","repository":"heron/app","trusted_actors":["heron"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := LoadPolicy(path)
	if err != nil || p.Repository != "heron/app" || len(p.TrustedActors) != 1 {
		t.Errorf("%+v %v", p, err)
	}
	if _, err := LoadPolicy(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("a missing policy must be an error")
	}
	_ = os.WriteFile(path, []byte("{"), 0o600)
	if _, err := LoadPolicy(path); err == nil {
		t.Error("a corrupt policy must be an error")
	}
}

func TestEnvMap(t *testing.T) {
	m := EnvMap([]string{"A=1", "B=x=y", "NOEQUALS", "C="})
	if m["A"] != "1" || m["B"] != "x=y" || m["C"] != "" {
		t.Errorf("%v", m)
	}
	if _, ok := m["NOEQUALS"]; ok {
		t.Error("malformed entries are skipped")
	}
	_ = errors.New
}
