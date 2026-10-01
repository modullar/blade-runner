package guard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var policy = Policy{
	TrustedActors: []string{"heron"},
	LocalLabels:   []string{"self-hosted", "macOS", "ARM64", "gpu"},
}

func scan(t *testing.T, src string) (jobs, local int, findings []Finding) {
	t.Helper()
	return ScanFile("ci.yml", []byte(src), policy)
}

func messages(fs []Finding) string {
	var b strings.Builder
	for _, f := range fs {
		b.WriteString(f.String() + "\n")
	}
	return b.String()
}

func TestCanonicalGuardIsAccepted(t *testing.T) {
	for _, pr := range []bool{false, true} {
		for _, actors := range [][]string{{"heron"}, {"heron", "alice"}} {
			expr := CanonicalIf(actors, pr)
			if reason := CheckIf(expr, actors, pr); reason != "" {
				t.Errorf("CanonicalIf(%v, %v) = %q is rejected by its own policy: %s", actors, pr, expr, reason)
			}
			// ...also when written the way people write it in a workflow.
			if reason := CheckIf("${{ "+expr+" }}", actors, pr); reason != "" {
				t.Errorf("wrapped in ${{ }}: %s", reason)
			}
		}
	}
}

func TestExpressionAttacksAreRefused(t *testing.T) {
	const pr = "(github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository)"
	tests := []struct {
		name, expr string
		needPR     bool
		want       string // substring of the refusal
	}{
		{"empty", "", false, "no `if:`"},
		{"no actor clause", "github.ref == 'refs/heads/main'", false, "trusted actor"},
		{"stranger", "github.actor == 'mallory'", false, "trusted actor"},
		{"top-level OR widens the job", "github.actor == 'heron' || github.event_name == 'push'", false, "top-level `||`"},
		{"OR true appended", "github.actor == 'heron' && " + pr + " || true", true, "top-level `||`"},
		{"actor OR stranger", "(github.actor == 'heron' || github.actor == 'mallory')", false, "trusted actor"},
		{"actor compared with !=", "github.actor != 'mallory'", false, "trusted actor"},
		{"actor in a contains() call", "contains(github.actor, 'heron')", false, "trusted actor"},
		{"actor equals a longer name", "github.actor == 'heron-evil'", false, "trusted actor"},
		{"pull_request without the repo clause", "github.actor == 'heron'", true, "same-repository"},
		{"wrong repo clause", "github.actor == 'heron' && github.event.pull_request.head.repo.fork == false", true, "same-repository"},
		{"repo clause is only half", "github.actor == 'heron' && github.event.pull_request.head.repo.full_name == github.repository", true, "same-repository"},
		{"guard inside a string literal", "github.actor == 'heron' && 'github.actor==heron'", false, ""}, // still accepted: the real clause is present
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckIf(tc.expr, []string{"heron"}, tc.needPR)
			if tc.want == "" {
				if got != "" {
					t.Errorf("should be accepted, got %q", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("CheckIf(%q) = %q, want a refusal mentioning %q", tc.expr, got, tc.want)
			}
		})
	}
}

func TestExpressionsThatAreSafeAreAccepted(t *testing.T) {
	const pr = "(github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository)"
	for name, tc := range map[string]struct {
		expr   string
		needPR bool
	}{
		"actor only, no PR trigger":         {"github.actor == 'heron'", false},
		"case-insensitive login":            {"github.actor == 'Heron'", false},
		"double quotes":                     {`github.actor == "heron"`, false},
		"extra clause only narrows":         {"github.actor == 'heron' && github.ref == 'refs/heads/main'", false},
		"with PR clause, either order":      {pr + " && github.actor == 'heron'", true},
		"extra function call":               {"github.actor == 'heron' && !cancelled()", false},
		"wrapped in an expression marker":   {"${{ github.actor == 'heron' }}", false},
		"PR clause present though unneeded": {"github.actor == 'heron' && " + pr, false},
	} {
		if reason := CheckIf(tc.expr, []string{"heron"}, tc.needPR); reason != "" {
			t.Errorf("%s: %q refused: %s", name, tc.expr, reason)
		}
	}
}

func TestHostedOnlyWorkflowsNeedNothing(t *testing.T) {
	_, local, findings := scan(t, `
name: ci
on:
  pull_request_target:
  issue_comment:
jobs:
  lint:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
  build:
    runs-on: [ubuntu-24.04, windows-2022]
    steps:
      - run: echo hi
  mac:
    runs-on: macos-14
    steps:
      - run: echo hi
`)
	if local != 0 || len(findings) != 0 {
		t.Errorf("hosted-only jobs must pass even on dangerous triggers (they never reach this machine): local=%d findings:\n%s", local, messages(findings))
	}
}

func TestLocalCapableJobsAreDetected(t *testing.T) {
	for name, runsOn := range map[string]string{
		"self-hosted literal":      "self-hosted",
		"self-hosted list":         "[self-hosted, macOS, ARM64]",
		"block list":               "\n      - self-hosted\n      - gpu",
		"only a custom label":      "gpu",
		"only an OS label":         "macOS",
		"forge routing expression": "${{ github.ref != 'refs/heads/main' && vars.FORGE_RUNNER || 'ubuntu-latest' }}",
		"matrix expression":        "${{ matrix.os }}",
		"runner group mapping":     "\n      group: ci-fleet",
		"unknown image name":       "my-private-image",
		"mixed hosted and local":   "[ubuntu-latest, self-hosted]",
		"case differs":             "Self-Hosted",
	} {
		src := "on: push\njobs:\n  j:\n    runs-on: " + runsOn + "\n    steps:\n      - run: x\n"
		_, local, findings := scan(t, src)
		if local != 1 {
			t.Errorf("%s: runs-on %q should count as possibly local", name, runsOn)
		}
		if len(findings) == 0 {
			t.Errorf("%s: an unguarded local-capable job must be reported", name)
		}
	}
}

func TestUnsafeTriggersAreRefusedForLocalJobs(t *testing.T) {
	for _, trig := range []string{"pull_request_target", "workflow_run", "issue_comment", "issues", "pull_request_review", "fork", "repository_dispatch", "watch"} {
		src := "on:\n  " + trig + ":\njobs:\n  j:\n    runs-on: self-hosted\n    if: github.actor == 'heron'\n    steps:\n      - run: x\n"
		_, _, findings := scan(t, src)
		if !strings.Contains(messages(findings), `trigger "`+trig+`"`) {
			t.Errorf("trigger %s must be refused even with an actor guard:\n%s", trig, messages(findings))
		}
	}
}

func TestTriggerSpellings(t *testing.T) {
	job := "jobs:\n  j:\n    runs-on: self-hosted\n    if: github.actor == 'heron'\n    steps:\n      - run: x\n"
	ok := map[string]string{
		"scalar":         "on: push\n",
		"inline list":    "on: [push, workflow_dispatch]\n",
		"block map":      "on:\n  push:\n    branches: [main]\n  workflow_dispatch:\n",
		"block list":     "on:\n  - push\n  - schedule\n",
		"quoted key":     "\"on\":\n  push:\n",
		"comment inside": "on:\n  push: # only mine\n",
	}
	for name, on := range ok {
		if _, _, f := scan(t, on+job); len(f) != 0 {
			t.Errorf("%s should be accepted:\n%s", name, messages(f))
		}
	}
	bad := map[string]string{
		"flow map":         "on: {push: {}, issue_comment: {}}\n",
		"missing on":       "",
		"dangerous scalar": "on: issue_comment\n",
		"dangerous list":   "on: [push, issues]\n",
	}
	for name, on := range bad {
		if _, _, f := scan(t, on+job); len(f) == 0 {
			t.Errorf("%s must be refused (fail closed)", name)
		}
	}
}

func TestPullRequestNeedsTheSameRepoClause(t *testing.T) {
	const head = "on: [push, pull_request]\njobs:\n  j:\n    runs-on: self-hosted\n"
	_, _, f := scan(t, head+"    if: github.actor == 'heron'\n    steps:\n      - run: x\n")
	if !strings.Contains(messages(f), "same-repository") {
		t.Errorf("an actor check alone is not enough on pull_request:\n%s", messages(f))
	}
	good := head + "    if: " + CanonicalIf([]string{"heron"}, true) + "\n    steps:\n      - run: x\n"
	if _, _, f := scan(t, good); len(f) != 0 {
		t.Errorf("the canonical guard must pass:\n%s", messages(f))
	}
}

func TestFindingsTellYouTheLineToWrite(t *testing.T) {
	_, _, f := scan(t, "on: [push, pull_request]\njobs:\n  test:\n    runs-on: self-hosted\n    steps:\n      - run: x\n")
	if len(f) != 1 || f[0].Job != "test" {
		t.Fatalf("findings = %v", f)
	}
	want := "if: github.actor == 'heron' && (github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository)"
	if !strings.Contains(f[0].Fix, want) {
		t.Errorf("fix should contain the exact line:\n%s\nwant it to contain\n%s", f[0].Fix, want)
	}
}

func TestStepLevelIfDoesNotCount(t *testing.T) {
	src := "on: push\njobs:\n  j:\n    runs-on: self-hosted\n    steps:\n      - run: x\n        if: github.actor == 'heron'\n"
	if _, _, f := scan(t, src); len(f) != 1 {
		t.Errorf("a step-level `if` must not satisfy the job-level requirement:\n%s", messages(f))
	}
}

func TestBlockScalarIfIsRead(t *testing.T) {
	src := "on: push\njobs:\n  j:\n    runs-on: self-hosted\n    if: >\n      github.actor == 'heron' &&\n      github.ref == 'refs/heads/main'\n    steps:\n      - run: x\n"
	if _, _, f := scan(t, src); len(f) != 0 {
		t.Errorf("a folded `if` with the actor clause should pass:\n%s", messages(f))
	}
}

func TestReusableWorkflowCallersAreScannedAtTheCallee(t *testing.T) {
	src := "on: push\njobs:\n  call:\n    uses: ./.github/workflows/build.yml\n"
	jobs, local, f := scan(t, src)
	if jobs != 1 || local != 0 || len(f) != 0 {
		t.Errorf("jobs=%d local=%d findings=%v", jobs, local, f)
	}
	// The callee uses workflow_call, which is not an allowed trigger for a local job.
	callee := "on:\n  workflow_call:\njobs:\n  b:\n    runs-on: self-hosted\n    if: github.actor == 'heron'\n    steps:\n      - run: x\n"
	if _, _, f := scan(t, callee); !strings.Contains(messages(f), "workflow_call") {
		t.Errorf("a called workflow with a local job must be refused:\n%s", messages(f))
	}
}

func TestScanDirReadsRealFiles(t *testing.T) {
	dir := t.TempDir()
	wf := filepath.Join(dir, ".github", "workflows")
	if err := os.MkdirAll(wf, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(wf, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("good.yml", "on: push\njobs:\n  a:\n    runs-on: self-hosted\n    if: github.actor == 'heron'\n    steps:\n      - run: x\n")
	write("bad.yaml", "on: push\njobs:\n  b:\n    runs-on: self-hosted\n    steps:\n      - run: x\n")
	write("hosted.yml", "on: issue_comment\njobs:\n  c:\n    runs-on: ubuntu-latest\n    steps:\n      - run: x\n")
	write("notes.txt", "runs-on: self-hosted") // not a workflow file

	rep, err := ScanDir(dir, policy)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Files != 3 || rep.Jobs != 3 || rep.LocalJobs != 2 {
		t.Errorf("report = %+v", rep)
	}
	if len(rep.Findings) != 1 || rep.Findings[0].File != "bad.yaml" || rep.Findings[0].Job != "b" {
		t.Errorf("findings = %v", rep.Findings)
	}

	empty, err := ScanDir(t.TempDir(), policy)
	if err != nil || empty.Files != 0 || len(empty.Findings) != 0 {
		t.Errorf("a project without workflows: %+v, %v", empty, err)
	}
}

// The two real forge workflows are committed as fixtures. Their routing expression can send
// a job to the local runner, and they carry no actor guard: exactly what the scan must catch.
func TestRealForgeWorkflows(t *testing.T) {
	dir := t.TempDir()
	wf := filepath.Join(dir, ".github", "workflows")
	if err := os.MkdirAll(wf, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ci.yml", "frontend.yml"} {
		data, err := os.ReadFile(filepath.Join("testdata", "forge", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wf, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := ScanDir(dir, Policy{TrustedActors: []string{"heron"}, LocalLabels: []string{"forge-mac"}})
	if err != nil {
		t.Fatal(err)
	}
	if rep.LocalJobs == 0 || len(rep.Findings) == 0 {
		t.Fatalf("the forge workflows route jobs to the local runner with no actor guard; scan found local=%d findings=%d", rep.LocalJobs, len(rep.Findings))
	}
	got := messages(rep.Findings)
	for _, hosted := range []string{"docker-build", "e2e"} { // the two jobs forge pins to ubuntu-latest
		if strings.Contains(got, "job "+hosted+":") {
			t.Errorf("job %s runs only on GitHub-hosted runners and must not be reported:\n%s", hosted, got)
		}
	}
	if !strings.Contains(got, "github.actor == 'heron'") {
		t.Errorf("findings should show the line to add:\n%s", got)
	}
	t.Logf("forge: %d files, %d jobs, %d may run locally, %d findings", rep.Files, rep.Jobs, rep.LocalJobs, len(rep.Findings))
}
