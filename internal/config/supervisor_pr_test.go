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

func TestEveryConfigWithoutAnExplicitOptInLeavesTheSupervisorsRiskySwitchesOff(t *testing.T) {
	// The default is the safe answer through every way a file can fail to say "true": no section,
	// a section about something else, a key with no value, and a file Render wrote.
	for name, yaml := range map[string]string{
		"no supervisor section":     supervisorBase,
		"a section about the image": supervisorBase + "supervisor:\n  image: ghcr.io/acme/runner@sha256:" + strings.Repeat("ab", 32) + "\n",
		"a key with no value":       supervisorBase + "supervisor:\n  allow_pull_request_merge:\n  cancel_unadmitted:\n",
		"a comment only":            supervisorBase + "supervisor:\n  # allow_pull_request_merge: true\n  network: bridge\n",
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := Parse([]byte(yaml), macDefaults)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Supervisor.AllowPullRequestMerge || cfg.Supervisor.CancelUnadmitted {
				t.Errorf("supervisor = %+v: pull_request merges and cancelling runs are both opt-in", cfg.Supervisor)
			}
			back, err := Parse(Render(cfg), macDefaults)
			if err != nil || back.Supervisor.AllowPullRequestMerge || back.Supervisor.CancelUnadmitted {
				t.Errorf("after Render: %+v, %v", back.Supervisor, err)
			}
		})
	}
	if (Supervisor{}).AllowPullRequestMerge || (Supervisor{}).CancelUnadmitted {
		t.Error("the zero Supervisor must be the safe one")
	}
}
