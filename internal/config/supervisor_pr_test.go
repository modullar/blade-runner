package config

import (
	"strings"
	"testing"

	"github.com/modullar/blade-runner/internal/diag"
)

func TestTheRemovedPullRequestSwitchIsAnErrorThatSaysWhyAndWhatToDo(t *testing.T) {
	// A config written for the earlier refuse-by-default switch must not load, and must not fail
	// as a bare "unknown key": the owner needs to know the decision changed and what to do now.
	for _, value := range []string{"true", "false", ""} {
		_, err := Parse([]byte(supervisorBase+"supervisor:\n  allow_pull_request_merge: "+value+"\n"), macDefaults)
		if diag.CodeOf(err) != diag.CodeConfigInvalid {
			t.Fatalf("value %q: %v, want BR-E001", value, err)
		}
		msg := err.Error()
		for _, want := range []string{"supervisor.allow_pull_request_merge", "was removed", "every commit", "trust store", "delete this line", "bladerunner trust add"} {
			if !strings.Contains(msg, want) {
				t.Errorf("value %q: the error should mention %q:\n%s", value, want, msg)
			}
		}
		if strings.Contains(msg, "allow_pull_request_merge: unknown key") {
			t.Errorf("value %q: a removed key is not an unknown one:\n%s", value, msg)
		}
	}
}

func TestACommentedOutPullRequestSwitchIsNotAKey(t *testing.T) {
	if _, err := Parse([]byte(supervisorBase+"supervisor:\n  # allow_pull_request_merge: true\n  network: bridge\n"), macDefaults); err != nil {
		t.Fatal(err)
	}
}

func TestEveryConfigWithoutAnExplicitOptInLeavesCancellingOff(t *testing.T) {
	for name, yaml := range map[string]string{
		"no supervisor section":     supervisorBase,
		"a section about the image": supervisorBase + "supervisor:\n  image: ghcr.io/acme/runner@sha256:" + strings.Repeat("ab", 32) + "\n",
		"a key with no value":       supervisorBase + "supervisor:\n  cancel_unadmitted:\n",
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := Parse([]byte(yaml), macDefaults)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Supervisor.CancelUnadmitted {
				t.Errorf("supervisor = %+v: cancelling runs is opt-in", cfg.Supervisor)
			}
			back, err := Parse(Render(cfg), macDefaults)
			if err != nil || back.Supervisor.CancelUnadmitted {
				t.Errorf("after Render: %+v, %v", back.Supervisor, err)
			}
		})
	}
	if (Supervisor{}).CancelUnadmitted {
		t.Error("the zero Supervisor must be the safe one")
	}
}
