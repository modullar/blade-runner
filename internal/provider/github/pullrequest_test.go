package github_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/provider"
	"github.com/modullar/blade-runner/internal/provider/github/githubtest"
)

func TestPullRequestDecodesTheFieldsTheSupervisorJudges(t *testing.T) {
	s := githubtest.New()
	defer s.Close()
	yes := true
	s.SetPullRequest("acme/widgets", githubtest.PullRequest{
		Info: provider.PullRequestInfo{Number: 7, State: "open", BaseRepository: "acme/widgets", BaseRef: "main", BaseSHA: strings.Repeat("a", 40),
			HeadRepository: "mallory/widgets", HeadSHA: strings.Repeat("b", 40), MergeCommitSHA: strings.Repeat("c", 40), Mergeable: &yes},
		Commits: []provider.PullRequestCommit{{SHA: strings.Repeat("d", 40), Author: "mallory"}, {SHA: strings.Repeat("b", 40), Author: "mallory"}},
	})
	got, err := newClient(s, "ghp_testtoken").PullRequest(ctx, "acme/widgets", 7)
	if err != nil {
		t.Fatal(err)
	}
	want := provider.PullRequestInfo{Number: 7, State: "open", BaseRepository: "acme/widgets", BaseRef: "main", BaseSHA: strings.Repeat("a", 40),
		HeadRepository: "mallory/widgets", HeadSHA: strings.Repeat("b", 40), MergeCommitSHA: strings.Repeat("c", 40), Mergeable: &yes, Commits: 2}
	if got.Mergeable == nil || *got.Mergeable != true {
		t.Fatalf("mergeable = %v", got.Mergeable)
	}
	got.Mergeable, want.Mergeable = nil, nil
	if got != want {
		t.Errorf("PullRequest = %+v, want %+v", got, want)
	}
}

func TestPullRequestKeepsNullAsNullAndAGoneForkAsEmpty(t *testing.T) {
	s := githubtest.New()
	defer s.Close()
	s.SetPullRequest("acme/widgets", githubtest.PullRequest{Info: provider.PullRequestInfo{Number: 8, State: "open", BaseRepository: "acme/widgets", HeadSHA: strings.Repeat("b", 40)}})
	got, err := newClient(s, "ghp_testtoken").PullRequest(ctx, "acme/widgets", 8)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mergeable != nil {
		t.Errorf("mergeable null must stay nil, never become false: %v", *got.Mergeable)
	}
	if got.MergeCommitSHA != "" || got.HeadRepository != "" {
		t.Errorf("null merge_commit_sha and head.repo must be empty: %+v", got)
	}
}

func TestPullRequestErrorsAreErrorsNotEmptyAnswers(t *testing.T) {
	s := githubtest.New()
	defer s.Close()
	c := newClient(s, "ghp_testtoken")
	if _, err := c.PullRequest(ctx, "acme/widgets", 99); err == nil {
		t.Error("a pull request that does not exist must be an error")
	}
	s.Down = true
	if _, err := c.PullRequest(ctx, "acme/widgets", 7); diag.CodeOf(err) != diag.CodeGitHubUnavailable {
		t.Errorf("GitHub down: %v", err)
	}
}

func TestListPullRequestCommitsFollowsPagesInOrderAndStopsAtGitHubsCap(t *testing.T) {
	for _, tc := range []struct {
		n, want, pages int
	}{{0, 0, 1}, {3, 3, 1}, {100, 100, 2}, {230, 230, 3}, {300, 250, 3}} {
		t.Run(fmt.Sprint(tc.n, " commits"), func(t *testing.T) {
			s := githubtest.New()
			defer s.Close()
			var all []provider.PullRequestCommit
			for i := 0; i < tc.n; i++ {
				all = append(all, provider.PullRequestCommit{SHA: fmt.Sprintf("%040x", i+1), Author: fmt.Sprintf("a%d", i)})
			}
			s.SetPullRequest("acme/widgets", githubtest.PullRequest{Info: provider.PullRequestInfo{Number: 7}, Commits: all})
			got, err := newClient(s, "ghp_testtoken").ListPullRequestCommits(ctx, "acme/widgets", 7)
			if err != nil || len(got) != tc.want {
				t.Fatalf("got %d commits, err %v; want %d", len(got), err, tc.want)
			}
			for i, c := range got {
				if c != all[i] {
					t.Fatalf("commit %d = %+v, want %+v: the order must be GitHub's", i, c, all[i])
				}
			}
			pages := 0
			for _, u := range s.URIs() {
				if strings.Contains(u, "/pulls/7/commits") {
					pages++
				}
			}
			if pages != tc.pages {
				t.Errorf("pages read = %d, want %d: %v", pages, tc.pages, s.URIs())
			}
		})
	}
}

func TestCommitReportsItsParentsInOrderAndANoSuchCommitIsRecognisable(t *testing.T) {
	s := githubtest.New()
	defer s.Close()
	c := newClient(s, "ghp_testtoken")
	tip, head, merge := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	s.AddCommit("acme/widgets", merge, "tree x\nparent "+tip+"\nparent "+head+"\nauthor a\n\nmerge\n", "")
	got, err := c.Commit(ctx, "acme/widgets", merge)
	if err != nil || len(got.Parents) != 2 || got.Parents[0] != tip || got.Parents[1] != head {
		t.Fatalf("parents = %v, %v: first parent first", got.Parents, err)
	}
	s.SetCommitParents("acme/widgets", merge, []string{head})
	if got, _ := c.Commit(ctx, "acme/widgets", merge); len(got.Parents) != 1 {
		t.Errorf("the provider's own list is what is reported: %v", got.Parents)
	}
	if _, err := c.Commit(ctx, "acme/widgets", strings.Repeat("9", 40)); !errors.Is(err, provider.ErrNoSuchCommit) || diag.CodeOf(err) != diag.CodeNotAdmitted {
		t.Errorf("a commit GitHub does not have: %v", err)
	}
	// A 503 is not "no such commit".
	s.AddCommit("acme/widgets", tip, "p", "s")
	s.SetCommitStatus(tip, 503)
	if _, err := c.Commit(ctx, "acme/widgets", tip); err == nil || errors.Is(err, provider.ErrNoSuchCommit) {
		t.Errorf("an outage must not read as a missing commit: %v", err)
	}
}
