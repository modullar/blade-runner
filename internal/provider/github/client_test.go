package github_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/provider"
	"github.com/modullar/blade-runner/internal/provider/github"
	"github.com/modullar/blade-runner/internal/provider/github/githubtest"
)

var (
	ctx       = context.Background()
	repoScope = provider.Scope{Kind: "repo", Repository: "acme/widgets"}
	orgScope  = provider.Scope{Kind: "org", Organization: "acme"}
)

func newClient(s *githubtest.Server, token string) *github.Client {
	return &github.Client{
		APIURL: s.URL, WebURL: s.URL,
		Token: func(context.Context) (string, error) { return token, nil },
	}
}

func TestCheckAuthMapsEachFailureToItsCode(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*githubtest.Server)
		token string
		scope provider.Scope
		want  string // "" means success
	}{
		{"valid repo token", func(*githubtest.Server) {}, "ghp_testtoken", repoScope, ""},
		{"valid org token", func(*githubtest.Server) {}, "ghp_testtoken", orgScope, ""},
		{"wrong token", func(*githubtest.Server) {}, "ghp_nope", repoScope, diag.CodeTokenRejected},
		{"token lacks permission", func(s *githubtest.Server) { s.Forbidden = true }, "ghp_testtoken", repoScope, diag.CodeTokenRejected},
		{"unknown repository", func(*githubtest.Server) {}, "ghp_testtoken", provider.Scope{Kind: "repo", Repository: "acme/missing"}, diag.CodeTokenRejected},
		{"unknown organization", func(*githubtest.Server) {}, "ghp_testtoken", provider.Scope{Kind: "org", Organization: "ghost"}, diag.CodeTokenRejected},
		{"GitHub down", func(s *githubtest.Server) { s.Down = true }, "ghp_testtoken", repoScope, diag.CodeGitHubUnavailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := githubtest.New()
			defer s.Close()
			tc.setup(s)
			err := newClient(s, tc.token).CheckAuth(ctx, tc.scope)
			if got := diag.CodeOf(err); got != tc.want {
				t.Fatalf("code = %q, want %q (err: %v)", got, tc.want, err)
			}
		})
	}
}

func TestUnreachableGitHubIsReportedAsSuch(t *testing.T) {
	s := githubtest.New()
	c := newClient(s, "ghp_testtoken")
	s.Close() // nothing listens any more
	err := c.CheckAuth(ctx, repoScope)
	if diag.CodeOf(err) != diag.CodeGitHubUnavailable {
		t.Errorf("err = %v, want BR-E022", err)
	}
}

func TestTokenNeverAppearsInErrors(t *testing.T) {
	s := githubtest.New()
	defer s.Close()
	const secret = "ghp_verysecretvalue"
	err := newClient(s, secret).CheckAuth(ctx, repoScope)
	if err == nil {
		t.Fatal("expected a rejection")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error message leaks the token:\n%v", err)
	}
}

func TestListRunnersFollowsPagination(t *testing.T) {
	s := githubtest.New()
	defer s.Close()
	for i := 0; i < 250; i++ {
		s.AddRunner("acme/widgets", fmt.Sprintf("r%03d", i), []string{"self-hosted", "Linux"}, i%2 == 0)
	}
	got, err := newClient(s, "ghp_testtoken").ListRunners(ctx, repoScope)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 250 {
		t.Fatalf("got %d runners, want 250 (pagination must be followed)", len(got))
	}
	if !got[0].Online() || got[1].Online() || got[0].Labels[0] != "self-hosted" {
		t.Errorf("fields not decoded: %+v %+v", got[0], got[1])
	}
}

func TestRegistrationTokenAndRemoveRunner(t *testing.T) {
	s := githubtest.New()
	defer s.Close()
	c := newClient(s, "ghp_testtoken")

	tok, err := c.RegistrationToken(ctx, repoScope)
	if err != nil || !strings.HasPrefix(tok, "REG-") {
		t.Fatalf("RegistrationToken = %q, %v", tok, err)
	}
	if _, err := c.RegistrationToken(ctx, orgScope); err != nil {
		t.Errorf("org registration token: %v", err)
	}

	s.AddRunner("acme/widgets", "mini", nil, true)
	rs, _ := c.ListRunners(ctx, repoScope)
	if err := c.RemoveRunner(ctx, repoScope, rs[0].ID); err != nil {
		t.Fatal(err)
	}
	if left := s.Runners("acme/widgets"); len(left) != 0 {
		t.Errorf("runner still registered: %+v", left)
	}
	if err := c.RemoveRunner(ctx, repoScope, rs[0].ID); diag.CodeOf(err) != diag.CodeTokenRejected {
		t.Errorf("removing an unknown runner: %v, want the not-found mapping", err)
	}
}

func TestVisibility(t *testing.T) {
	s := githubtest.New()
	defer s.Close()
	s.Repos["acme/open"] = false

	withToken := newClient(s, "ghp_testtoken")
	noToken := &github.Client{APIURL: s.URL, WebURL: s.URL} // init before any token exists

	for _, tc := range []struct {
		name   string
		c      *github.Client
		repo   string
		want   provider.Visibility
		wantEr string
	}{
		{"public, with token", withToken, "acme/open", provider.Public, ""},
		{"public, no token", noToken, "acme/open", provider.Public, ""},
		{"private, with token", withToken, "acme/widgets", provider.Private, ""},
		{"private, no token is indistinguishable from missing", noToken, "acme/widgets", provider.VisibilityUnknown, diag.CodeTokenRejected},
		{"missing", withToken, "acme/none", provider.VisibilityUnknown, diag.CodeTokenRejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.c.Visibility(ctx, tc.repo)
			if diag.CodeOf(err) != tc.wantEr || got != tc.want {
				t.Errorf("got %v, %v; want %v, code %q", got, err, tc.want, tc.wantEr)
			}
		})
	}
}

func TestReleaseResolvesAssetAndChecksumWithoutSendingTheToken(t *testing.T) {
	s := githubtest.New()
	defer s.Close()
	c := newClient(s, "ghp_testtoken")

	for _, tc := range []struct{ goos, goarch, file string }{
		{"darwin", "arm64", "actions-runner-osx-arm64-" + githubtest.Version + ".tar.gz"},
		{"darwin", "amd64", "actions-runner-osx-x64-" + githubtest.Version + ".tar.gz"},
		{"linux", "amd64", "actions-runner-linux-x64-" + githubtest.Version + ".tar.gz"},
		{"linux", "arm64", "actions-runner-linux-arm64-" + githubtest.Version + ".tar.gz"},
	} {
		rel, err := c.Release(ctx, tc.goos, tc.goarch, "")
		if err != nil {
			t.Fatalf("%s/%s: %v", tc.goos, tc.goarch, err)
		}
		sum := sha256.Sum256(s.Tarball)
		if rel.Filename != tc.file || rel.Version != githubtest.Version || rel.SHA256 != hex.EncodeToString(sum[:]) {
			t.Errorf("%s/%s: release = %+v", tc.goos, tc.goarch, rel)
		}
		if !strings.HasPrefix(rel.URL, s.URL+"/download/") {
			t.Errorf("URL = %q", rel.URL)
		}
	}
	if s.ReleaseAuthHeader != "" {
		t.Errorf("the public release lookup carried an Authorization header: %q", s.ReleaseAuthHeader)
	}
}

func TestReleaseFailsClosedWithoutAChecksum(t *testing.T) {
	s := githubtest.New()
	defer s.Close()
	s.OmitChecksum = true
	_, err := newClient(s, "x").Release(ctx, "linux", "amd64", "")
	if diag.CodeOf(err) != diag.CodeReleaseUnresolved || !strings.Contains(err.Error(), "SHA-256") {
		t.Errorf("err = %v, want BR-E030 about the missing SHA-256", err)
	}
}

func TestReleaseCanBePinnedToAVersion(t *testing.T) {
	s := githubtest.New()
	defer s.Close()
	c := newClient(s, "x")
	rel, err := c.Release(ctx, "linux", "amd64", "v"+githubtest.Version)
	if err != nil || rel.Version != githubtest.Version {
		t.Fatalf("pinned release = %+v, %v", rel, err)
	}
	if !contains(s.Requests(), "GET /repos/actions/runner/releases/tags/v"+githubtest.Version) {
		t.Errorf("requests = %v, want the tags endpoint", s.Requests())
	}
	_, err = c.Release(ctx, "linux", "amd64", "1.0.0")
	if diag.CodeOf(err) != diag.CodeReleaseUnresolved || !strings.Contains(err.Error(), "v1.0.0") {
		t.Errorf("unknown pin: %v, want BR-E030 naming the version", err)
	}
}

func contains(list []string, want string) bool {
	for _, x := range list {
		if x == want {
			return true
		}
	}
	return false
}

func TestReleaseRejectsUnsupportedPlatforms(t *testing.T) {
	s := githubtest.New()
	defer s.Close()
	c := newClient(s, "x")
	for _, p := range [][2]string{{"windows", "amd64"}, {"linux", "386"}, {"freebsd", "arm64"}} {
		if _, err := c.Release(ctx, p[0], p[1], ""); diag.CodeOf(err) != diag.CodeUnsupportedPlatform {
			t.Errorf("%v: err = %v, want BR-E011", p, err)
		}
	}
	for _, r := range s.Requests() {
		if strings.Contains(r, "releases") {
			t.Errorf("an unsupported platform must be refused before any request: %s", r)
		}
	}
}

func TestRegistrationURL(t *testing.T) {
	c := &github.Client{}
	if got := c.RegistrationURL(repoScope); got != "https://github.com/acme/widgets" {
		t.Errorf("repo URL = %q", got)
	}
	if got := c.RegistrationURL(orgScope); got != "https://github.com/acme" {
		t.Errorf("org URL = %q", got)
	}
}

func TestMalformedScopesAreRejectedBeforeAnyRequest(t *testing.T) {
	s := githubtest.New()
	defer s.Close()
	c := newClient(s, "ghp_testtoken")
	for _, sc := range []provider.Scope{{Kind: "repo", Repository: "noslash"}, {Kind: "org"}, {Kind: "team"}} {
		if _, err := c.ListRunners(ctx, sc); err == nil {
			t.Errorf("scope %+v should be rejected", sc)
		}
	}
	if n := len(s.Requests()); n != 0 {
		t.Errorf("%d requests were made for invalid scopes", n)
	}
}

func TestRefusalWithoutATokenIsNotBlamedOnTheToken(t *testing.T) {
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden) // e.g. a proxy policy, answering before GitHub does
	}))
	defer blocked.Close()
	c := &github.Client{APIURL: blocked.URL, WebURL: blocked.URL} // no token configured

	_, err := c.Visibility(ctx, "acme/widgets")
	if diag.CodeOf(err) != diag.CodeGitHubUnavailable || !strings.Contains(err.Error(), "no token") {
		t.Errorf("err = %v, want BR-E022 saying the request carried no token", err)
	}
	if strings.Contains(err.Error(), "rejected the token") {
		t.Errorf("a request that sent no token must not claim the token was rejected:\n%v", err)
	}

	// With a token, the same 403 is correctly about the token.
	withToken := &github.Client{APIURL: blocked.URL, WebURL: blocked.URL, Token: func(context.Context) (string, error) { return "ghp_x", nil }}
	if _, err := withToken.ListRunners(ctx, repoScope); diag.CodeOf(err) != diag.CodeTokenRejected {
		t.Errorf("authenticated 403: %v, want BR-E021", err)
	}
}

func TestForkApprovalPolicy(t *testing.T) {
	s := githubtest.New()
	defer s.Close()
	c := newClient(s, "ghp_testtoken")

	if got, err := c.ForkApprovalPolicy(ctx, "acme/widgets"); err != nil || got != provider.StrictForkApproval {
		t.Fatalf("default = %q, %v", got, err)
	}
	s.ForkApproval = map[string]string{"acme/widgets": "first_time_contributors"}
	if got, _ := c.ForkApprovalPolicy(ctx, "acme/widgets"); got != "first_time_contributors" {
		t.Errorf("policy = %q", got)
	}
	s.ForkApprovalUnsupported = true
	if _, err := c.ForkApprovalPolicy(ctx, "acme/widgets"); err == nil {
		t.Error("an endpoint that answers 404 must be an error, never a guessed policy")
	}
	if _, err := newClient(s, "ghp_wrong").ForkApprovalPolicy(ctx, "acme/widgets"); diag.CodeOf(err) != diag.CodeTokenRejected {
		t.Errorf("wrong token: %v", err)
	}
}

func TestCommitReturnsTheSignedBytesExactlyAsStored(t *testing.T) {
	s := githubtest.New()
	defer s.Close()
	sha := strings.Repeat("ab", 20)
	s.AddCommit("acme/widgets", sha, "tree x\n\nmsg\n", "-----BEGIN SSH SIGNATURE-----\nAAA\n-----END SSH SIGNATURE-----")
	s.AddCommit("acme/widgets", strings.Repeat("cd", 20), "tree y\n\nunsigned\n", "")
	c := newClient(s, "ghp_testtoken")

	got, err := c.Commit(ctx, "acme/widgets", sha)
	if err != nil || got.SHA != sha || got.Payload != "tree x\n\nmsg\n" || !strings.HasPrefix(got.Signature, "-----BEGIN SSH SIGNATURE-----") {
		t.Fatalf("Commit = %+v, %v", got, err)
	}
	unsigned, err := c.Commit(ctx, "acme/widgets", strings.Repeat("cd", 20))
	if err != nil || unsigned.Signature != "" || unsigned.Payload != "" {
		t.Errorf("an unsigned commit has no signature or payload to offer: %+v, %v", unsigned, err)
	}
}

func TestCommitRefusesAnythingButAFullCommitId(t *testing.T) {
	s := githubtest.New()
	defer s.Close()
	c := newClient(s, "ghp_testtoken")
	for _, bad := range []string{"", "main", "abc123", strings.Repeat("A", 40), strings.Repeat("g", 40), "../../x", strings.Repeat("a", 41), "a/b"} {
		if _, err := c.Commit(ctx, "acme/widgets", bad); diag.CodeOf(err) != diag.CodeNotAdmitted {
			t.Errorf("%q: err = %v, want BR-E067", bad, err)
		}
	}
	if n := len(s.Requests()); n != 0 {
		t.Errorf("a malformed id must be refused before any request (made %d)", n)
	}
}

func TestCommitErrorsAreMappedNotSwallowed(t *testing.T) {
	s := githubtest.New()
	defer s.Close()
	sha := strings.Repeat("ab", 20)
	s.AddCommit("acme/widgets", sha, "p", "s")
	if _, err := newClient(s, "ghp_testtoken").Commit(ctx, "acme/widgets", strings.Repeat("ef", 20)); err == nil {
		t.Error("an unknown commit must be an error")
	}
	s.Down = true
	if _, err := newClient(s, "ghp_testtoken").Commit(ctx, "acme/widgets", sha); diag.CodeOf(err) != diag.CodeGitHubUnavailable {
		t.Errorf("GitHub down: %v", err)
	}
}

func TestListRunsAndJobsDecodeWhatTheSupervisorNeeds(t *testing.T) {
	s := githubtest.New()
	defer s.Close()
	s.AddRun("acme/widgets", provider.Run{ID: 1, HeadSHA: strings.Repeat("a", 40), Event: "push", Status: "completed", HeadRepository: "acme/widgets", Actor: "acme"})
	s.AddRun("acme/widgets", provider.Run{ID: 2, HeadSHA: strings.Repeat("b", 40), Event: "pull_request", Status: "queued", HeadRepository: "mallory/widgets", Actor: "mallory"})
	s.AddJob("acme/widgets", provider.Job{ID: 20, RunID: 2, Status: "queued", Labels: []string{"self-hosted", "gpu"}, HeadSHA: strings.Repeat("b", 40)})
	s.AddJob("acme/widgets", provider.Job{ID: 21, RunID: 2, Status: "in_progress", Labels: []string{"self-hosted"}, RunnerName: "r1", HeadSHA: strings.Repeat("b", 40)})
	c := newClient(s, "ghp_testtoken")

	all, err := c.ListRuns(ctx, "acme/widgets", "")
	if err != nil || len(all) != 2 || all[0].ID != 2 {
		t.Fatalf("runs = %+v, %v (newest first)", all, err)
	}
	if all[0].HeadRepository != "mallory/widgets" || all[0].Actor != "mallory" || all[0].Event != "pull_request" {
		t.Errorf("a fork's pull request must be distinguishable: %+v", all[0])
	}
	queued, _ := c.ListRuns(ctx, "acme/widgets", "queued")
	if len(queued) != 1 || queued[0].ID != 2 {
		t.Errorf("status filter: %+v", queued)
	}
	jobs, err := c.ListJobs(ctx, "acme/widgets", 2)
	if err != nil || len(jobs) != 2 || jobs[0].Labels[1] != "gpu" || jobs[1].RunnerName != "r1" || jobs[0].RunnerName != "" {
		t.Errorf("jobs = %+v, %v", jobs, err)
	}
	if none, err := c.ListJobs(ctx, "acme/widgets", 999); err != nil || len(none) != 0 {
		t.Errorf("a run with no jobs: %+v %v", none, err)
	}
	if _, err := newClient(s, "ghp_wrong").ListRuns(ctx, "acme/widgets", ""); diag.CodeOf(err) != diag.CodeTokenRejected {
		t.Errorf("wrong token: %v", err)
	}
}

func TestListCommitsNewestFirst(t *testing.T) {
	s := githubtest.New()
	defer s.Close()
	s.AddCommit("acme/widgets", strings.Repeat("1", 40), "p", "s")
	s.AddCommit("acme/widgets", strings.Repeat("2", 40), "p", "s")
	got, err := newClient(s, "ghp_testtoken").ListCommits(ctx, "acme/widgets", 10)
	if err != nil || len(got) != 2 || got[0] != strings.Repeat("2", 40) {
		t.Errorf("commits = %v, %v", got, err)
	}
	if _, err := newClient(s, "ghp_testtoken").ListCommits(ctx, "ghost/none", 10); err == nil {
		t.Error("an unknown repository must be an error")
	}
}

func TestGenerateJITConfigRegistersAnOfflineSingleUseRunner(t *testing.T) {
	s := githubtest.New()
	defer s.Close()
	c := newClient(s, "ghp_testtoken")
	scope := provider.Scope{Kind: "repo", Repository: "acme/widgets"}

	cfg, err := c.GenerateJITConfig(ctx, scope, "job-runner-1", []string{"self-hosted", "br"})
	if err != nil || cfg.Encoded == "" || cfg.RunnerID == 0 {
		t.Fatalf("cfg = %+v, %v", cfg, err)
	}
	got := s.Runners("acme/widgets")
	if len(got) != 1 || got[0].Name != "job-runner-1" || got[0].Online || got[0].Labels[1] != "br" {
		t.Errorf("registered runners = %+v: it exists but nothing has started it", got)
	}
	if err := c.RemoveRunner(ctx, scope, cfg.RunnerID); err != nil {
		t.Fatal(err)
	}
	s.JITUnsupported = true
	if _, err := c.GenerateJITConfig(ctx, scope, "x", nil); err == nil {
		t.Error("an API without JIT must be an error")
	}
	if _, err := c.GenerateJITConfig(ctx, provider.Scope{Kind: "repo", Repository: "noslash"}, "x", nil); err == nil {
		t.Error("a malformed scope must be refused")
	}
	if _, err := newClient(s, "ghp_wrong").GenerateJITConfig(ctx, scope, "x", nil); diag.CodeOf(err) != diag.CodeTokenRejected {
		t.Errorf("wrong token: %v", err)
	}
}
