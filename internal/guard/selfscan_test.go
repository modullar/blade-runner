package guard

import "testing"

func TestThisRepositoryOwnWorkflowsPass(t *testing.T) {
	rep, err := ScanDir("../..", Policy{TrustedActors: []string{"modullar"}, LocalLabels: []string{"self-hosted", "macOS", "ARM64", "X64", "Linux"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d files, %d jobs, %d may run locally, findings: %v", rep.Files, rep.Jobs, rep.LocalJobs, rep.Findings)
	if rep.Files == 0 || rep.LocalJobs != 0 || len(rep.Findings) != 0 {
		t.Errorf("blade-runner's own CI uses only GitHub-hosted images and must pass untouched: %+v", rep)
	}
}
