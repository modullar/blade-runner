package github_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/provider"
	"github.com/modullar/blade-runner/internal/provider/github/githubtest"
)

func addQueuedRuns(s *githubtest.Server, n int) {
	for i := 1; i <= n; i++ {
		s.AddRun("acme/widgets", provider.Run{ID: int64(i), HeadSHA: strings.Repeat("a", 40), Event: "push", Status: "queued", HeadRepository: "acme/widgets"})
	}
}

func addJobs(s *githubtest.Server, n int) {
	for i := 1; i <= n; i++ {
		s.AddJob("acme/widgets", provider.Job{ID: int64(1000 + i), RunID: 1, Status: "queued", Labels: []string{"self-hosted"}})
	}
}

func TestListRunsErrorsWhenGitHubCapsTheListBelowItsTotalCount(t *testing.T) {
	// GitHub stops its list endpoints at 1000 results but still reports the real total_count.
	// Reading to the empty page and calling that "the end" would hide every run past the cap.
	s := githubtest.New()
	defer s.Close()
	addQueuedRuns(s, 1500)
	s.ResultCap = 1000
	runs, err := newClient(s, "ghp_testtoken").ListRuns(ctx, "acme/widgets", "queued")
	if err == nil || diag.CodeOf(err) != diag.CodeGitHubUnavailable {
		t.Fatalf("got %d runs and err %v: a list shorter than total_count must be an error", len(runs), err)
	}
	if runs != nil {
		t.Errorf("a partial list must not be returned alongside the error: %d runs", len(runs))
	}
}

func TestListJobsErrorsWhenGitHubCapsTheListBelowItsTotalCount(t *testing.T) {
	s := githubtest.New()
	defer s.Close()
	addJobs(s, 1200)
	s.ResultCap = 1000
	if jobs, err := newClient(s, "ghp_testtoken").ListJobs(ctx, "acme/widgets", 1); err == nil {
		t.Fatalf("got %d jobs and no error", len(jobs))
	}
}

func TestListRunsAndJobsErrorOnAShortPageThatIsNotTheLast(t *testing.T) {
	s := githubtest.New()
	defer s.Close()
	addQueuedRuns(s, 250)
	addJobs(s, 250)
	s.MaxPerPage = 40 // every page is short, and 250 results are advertised
	c := newClient(s, "ghp_testtoken")
	if runs, err := c.ListRuns(ctx, "acme/widgets", "queued"); err == nil {
		t.Errorf("a short page was taken as the end of the list: %d of 250 runs and no error", len(runs))
	}
	if jobs, err := c.ListJobs(ctx, "acme/widgets", 1); err == nil {
		t.Errorf("a short page was taken as the end of the list: %d of 250 jobs and no error", len(jobs))
	}
}

func TestListRunsAndJobsAcceptAListWhoseLastPageIsShort(t *testing.T) {
	s := githubtest.New()
	defer s.Close()
	addQueuedRuns(s, 101) // one full page and a page of one
	addJobs(s, 101)
	c := newClient(s, "ghp_testtoken")
	if runs, err := c.ListRuns(ctx, "acme/widgets", "queued"); err != nil || len(runs) != 101 {
		t.Errorf("%d runs, %v", len(runs), err)
	}
	if jobs, err := c.ListJobs(ctx, "acme/widgets", 1); err != nil || len(jobs) != 101 {
		t.Errorf("%d jobs, %v", len(jobs), err)
	}
}

func TestListRunnersErrorsWhenTheListCannotBeReadCompletely(t *testing.T) {
	s := githubtest.New()
	defer s.Close()
	for i := 0; i < 5100; i++ {
		s.AddRunner("acme/widgets", fmt.Sprintf("r-%d", i), nil, true)
	}
	if got, err := newClient(s, "ghp_testtoken").ListRunners(ctx, repoScope); err == nil {
		t.Errorf("%d runners and no error: a list cut off at the page limit must not look complete", len(got))
	}
}

func TestAListThatEndsShortOfItsTotalStopsAfterOneEmptyPageThenErrors(t *testing.T) {
	s := githubtest.New()
	defer s.Close()
	addQueuedRuns(s, 1500)
	s.ResultCap = 1000
	if _, err := newClient(s, "ghp_testtoken").ListRuns(ctx, "acme/widgets", "queued"); err == nil {
		t.Fatal("a capped list must be an error")
	}
	pages := 0
	for _, u := range s.URIs() {
		if strings.Contains(u, "/actions/runs?") {
			pages++
		}
	}
	if pages != 11 { // ten full pages and the one empty page that shows the cap
		t.Errorf("%d page requests, want 11: reading on after an empty page only repeats the answer", pages)
	}
}

func TestATotalCountThatLagsBehindTheListDoesNotBlindTheCaller(t *testing.T) {
	// A run created after total_count was counted: the list holds more than the total says. The
	// extra runs must be returned (a queue that hides a run is unsafe), not turned into an error,
	// and not cut at the total (which would hide the last of them).
	s := githubtest.New()
	defer s.Close()
	addQueuedRuns(s, 250)
	addJobs(s, 250)
	s.TotalCountOffset = -200 // total_count says 50 while 250 exist
	c := newClient(s, "ghp_testtoken")
	if runs, err := c.ListRuns(ctx, "acme/widgets", "queued"); err != nil || len(runs) != 250 {
		t.Errorf("%d runs, %v: want all 250", len(runs), err)
	}
	if jobs, err := c.ListJobs(ctx, "acme/widgets", 1); err != nil || len(jobs) != 250 {
		t.Errorf("%d jobs, %v: want all 250", len(jobs), err)
	}
}
