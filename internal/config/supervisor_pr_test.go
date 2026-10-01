package config

import (
	"strings"
	"testing"

	"github.com/modullar/blade-runner/internal/diag"
)

func TestAllowPullRequestMergeIsOffUnlessAskedFor(t *testing.T) {
	cfg, err := Parse([]byte(supervisorBase), macDefaults)
	if err != nil || cfg.Supervisor.AllowPullRequestMerge {
		t.Fatalf("default: %+v, %v: pull_request runs are refused unless the owner opts in", cfg.Supervisor, err)
	}
	cfg, err = Parse([]byte(supervisorBase+"supervisor:\n  allow_pull_request_merge: true\n"), macDefaults)
	if err != nil || !cfg.Supervisor.AllowPullRequestMerge {
		t.Fatalf("opted in: %+v, %v", cfg.Supervisor, err)
	}
	cfg, err = Parse([]byte(supervisorBase+"supervisor:\n  allow_pull_request_merge: false\n"), macDefaults)
	if err != nil || cfg.Supervisor.AllowPullRequestMerge {
		t.Fatalf("explicitly off: %+v, %v", cfg.Supervisor, err)
	}
	_, err = Parse([]byte(supervisorBase+"supervisor:\n  allow_pull_request_merge: maybe\n"), macDefaults)
	if diag.CodeOf(err) != diag.CodeConfigInvalid || !strings.Contains(err.Error(), "supervisor.allow_pull_request_merge") {
		t.Errorf("a value that is no bool: %v", err)
	}
}
