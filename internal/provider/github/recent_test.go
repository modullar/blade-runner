package github_test

import (
	"testing"

	"github.com/modullar/blade-runner/internal/provider/github/githubtest"
)

func runListRequests(s *githubtest.Server) int {
	n := 0
	for _, r := range s.Requests() {
		if r == "GET /repos/acme/widgets/actions/runs" {
			n++
		}
	}
	return n
}

func TestListRecentRunsReadsOnlyAsManyPagesAsTheLimitNeeds(t *testing.T) {
	s := githubtest.New()
	defer s.Close()
	addQueuedRuns(s, 250)
	c := newClient(s, "ghp_testtoken")
	runs, err := c.ListRecentRuns(ctx, "acme/widgets", "queued", 30)
	if err != nil || len(runs) != 30 || runs[0].ID != 250 || runs[29].ID != 221 {
		t.Fatalf("%d runs, %v", len(runs), err)
	}
	if n := runListRequests(s); n != 1 {
		t.Errorf("%d requests to keep 30 of 250 runs, want 1", n)
	}
	runs, err = c.ListRecentRuns(ctx, "acme/widgets", "queued", 150)
	if err != nil || len(runs) != 150 {
		t.Fatalf("%d runs, %v", len(runs), err)
	}
	if n := runListRequests(s); n != 3 {
		t.Errorf("%d requests in all, want 1 + 2", n)
	}
	if runs, err := c.ListRecentRuns(ctx, "acme/widgets", "queued", 500); err != nil || len(runs) != 250 {
		t.Errorf("a limit above the total returns everything: %d, %v", len(runs), err)
	}
	if _, err := c.ListRecentRuns(ctx, "acme/widgets", "queued", 0); err == nil {
		t.Error("a non-positive limit must be refused")
	}
}

func TestListRecentRunsStillRefusesACappedListShorterThanTheLimit(t *testing.T) {
	s := githubtest.New()
	defer s.Close()
	addQueuedRuns(s, 1500)
	s.ResultCap = 1000
	if runs, err := newClient(s, "ghp_testtoken").ListRecentRuns(ctx, "acme/widgets", "queued", 1200); err == nil {
		t.Errorf("%d runs and no error: the cap hid results the caller asked for", len(runs))
	}
}
