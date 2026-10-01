// Package github implements provider.Provider for GitHub Actions.
//
// Every endpoint and response field here is an assumption from GitHub's documentation as
// recalled, not yet verified against the live API: decision records docs/decisions/0002
// (token scope) and 0003 (registration and release format) list what BR-0 must confirm.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/provider"
)

// Client talks to the GitHub REST API.
type Client struct {
	APIURL string // default https://api.github.com
	WebURL string // default https://github.com
	// Token supplies the token lazily, so commands that need none (resolving a public
	// release) never read the keychain.
	Token func(ctx context.Context) (string, error)
	HTTP  *http.Client
}

// Defaults.
const (
	DefaultAPIURL = "https://api.github.com"
	DefaultWebURL = "https://github.com"
	apiVersion    = "2022-11-28"
)

var _ provider.Provider = (*Client)(nil)

func (c *Client) api() string { return strings.TrimRight(orDefault(c.APIURL, DefaultAPIURL), "/") }
func (c *Client) web() string { return strings.TrimRight(orDefault(c.WebURL, DefaultWebURL), "/") }

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// RegistrationURL is the URL config.sh takes as --url.
func (c *Client) RegistrationURL(s provider.Scope) string {
	return c.web() + "/" + s.Target()
}

func scopePath(s provider.Scope) (string, error) {
	switch s.Kind {
	case "repo":
		if !strings.Contains(s.Repository, "/") {
			return "", fmt.Errorf("repository %q is not OWNER/REPO", s.Repository)
		}
		return "/repos/" + s.Repository, nil
	case "org":
		if s.Organization == "" {
			return "", fmt.Errorf("organization is empty")
		}
		return "/orgs/" + s.Organization, nil
	}
	return "", fmt.Errorf("unknown scope kind %q", s.Kind)
}

// do sends one request. When auth is true the token is attached; the public release
// lookup sends none, so the user's token never goes where it is not needed.
func (c *Client) do(ctx context.Context, method, path string, auth bool, out any) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.api()+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", "bladerunner")
	if auth {
		if c.Token == nil {
			return nil, diag.New(diag.CodeTokenMissing, "no GitHub token available", "no token source was configured", "run `bladerunner init`")
		}
		tok, err := c.Token(ctx)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, diag.Wrap(err, diag.CodeGitHubUnavailable, "cannot reach GitHub",
			"the network is down, or a proxy or firewall blocks "+c.api(), "check the connection, then re-run")
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		return resp, apiError(method, path, resp, body, auth)
	}
	if out != nil && len(body) > 0 {
		if err := json.Unmarshal(body, out); err != nil {
			return resp, diag.Wrap(err, diag.CodeGitHubUnavailable, fmt.Sprintf("GitHub answered %s %s with something that is not JSON", method, path),
				"a proxy or captive portal is rewriting the response", "check the network, then re-run")
		}
	}
	return resp, nil
}

// send is do() for a request with a JSON body.
func (c *Client) send(ctx context.Context, method, path string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.api()+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", "bladerunner")
	if c.Token == nil {
		return diag.New(diag.CodeTokenMissing, "no GitHub token available", "no token source was configured", "run `bladerunner init`")
	}
	tok, err := c.Token(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return diag.Wrap(err, diag.CodeGitHubUnavailable, "cannot reach GitHub", "the network is down or a proxy blocks "+c.api(), "check the connection, then re-run")
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		return apiError(method, path, resp, data, true)
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return diag.Wrap(err, diag.CodeGitHubUnavailable, "GitHub answered with something that is not JSON", "a proxy is rewriting the response", "check the network")
		}
	}
	return nil
}

// apiError turns an HTTP failure into a diag error naming the likely cause and the fix.
func apiError(method, path string, resp *http.Response, body []byte, authed bool) error {
	var msg struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &msg)
	detail := fmt.Errorf("%s %s: HTTP %d %s", method, path, resp.StatusCode, msg.Message)
	switch {
	case !authed && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) &&
		resp.Header.Get("X-RateLimit-Remaining") != "0":
		// No token was sent, so "your token is wrong" would be a false accusation.
		return diag.Wrap(detail, diag.CodeGitHubUnavailable, "GitHub refused a request that carries no token",
			"a proxy or network policy in front of GitHub is blocking it, or the resource needs a token",
			"check the network, or store a token with `bladerunner init` and re-run")
	case resp.StatusCode == http.StatusUnauthorized:
		return diag.Wrap(detail, diag.CodeTokenRejected, "GitHub rejected the token",
			"it is expired, revoked or mistyped", "create a new token (see docs/decisions/0002-token-scope.md) and store it with `bladerunner init`")
	case resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0":
		return diag.Wrap(detail, diag.CodeGitHubUnavailable, "GitHub's API rate limit is used up",
			"too many requests from this token or address", "wait until "+resetTime(resp)+", then re-run")
	case resp.StatusCode == http.StatusForbidden:
		return diag.Wrap(detail, diag.CodeTokenRejected, "the token is not allowed to do that",
			"it lacks the permission to manage self-hosted runners", "give the token the scope in docs/decisions/0002-token-scope.md, then re-run")
	case resp.StatusCode == http.StatusNotFound:
		return diag.Wrap(detail, diag.CodeTokenRejected, "GitHub says the repository or organization was not found",
			"the name is wrong, or the token cannot see it (GitHub answers 404 to hide private resources)",
			"check runner.repository / runner.organization, and that the token covers it")
	}
	return diag.Wrap(detail, diag.CodeGitHubUnavailable, "GitHub returned an error",
		"a temporary GitHub problem, or a request GitHub did not accept", "re-run in a minute; if it persists, see the detail above")
}

func resetTime(resp *http.Response) string {
	if n, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		return time.Unix(n, 0).UTC().Format(time.RFC3339)
	}
	return "the limit resets"
}

// CheckAuth lists runners: the same permission registration and removal need, without
// changing anything.
func (c *Client) CheckAuth(ctx context.Context, s provider.Scope) error {
	_, err := c.ListRunners(ctx, s)
	return err
}

// Visibility reads the repository's visibility. The token is used when present so a
// private repository answers "private" rather than 404.
func (c *Client) Visibility(ctx context.Context, repository string) (provider.Visibility, error) {
	var out struct {
		Private    bool   `json:"private"`
		Visibility string `json:"visibility"`
	}
	if _, err := c.do(ctx, http.MethodGet, "/repos/"+repository, c.Token != nil, &out); err != nil {
		return provider.VisibilityUnknown, err
	}
	switch {
	case out.Visibility == "public" || (out.Visibility == "" && !out.Private):
		return provider.Public, nil
	default:
		return provider.Private, nil
	}
}

// ForkApprovalPolicy reads the repository's fork-pull-request approval setting. The endpoint
// and the "approval_policy" field are assumptions to confirm in BR-0; any failure is returned
// as an error, never guessed, so the caller can fail closed.
func (c *Client) ForkApprovalPolicy(ctx context.Context, repository string) (string, error) {
	var out struct {
		Policy string `json:"approval_policy"`
	}
	if _, err := c.do(ctx, http.MethodGet, "/repos/"+repository+"/actions/permissions/fork-pr-contributor-approval", true, &out); err != nil {
		return "", err
	}
	if out.Policy == "" {
		return "", diag.New(diag.CodeForkApproval, "GitHub did not say what the fork-approval policy is",
			"the response had no \"approval_policy\" field", "check Settings > Actions > General by hand")
	}
	return out.Policy, nil
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Commit fetches a commit's signed payload and signature. It uses the token when one is
// configured (a private repository needs it). It returns exactly what GitHub stores; the caller
// must recompute the commit id and verify the signature, because nothing here is checked. The
// "verification.payload" and "verification.signature" fields are assumptions to confirm in BR-0.
func (c *Client) Commit(ctx context.Context, repository, sha string) (provider.Commit, error) {
	if !shaRe.MatchString(sha) {
		return provider.Commit{}, diag.New(diag.CodeNotAdmitted, fmt.Sprintf("%q is not a full commit id", sha),
			"a commit must be named by its full 40-character id", "use the full SHA")
	}
	var out struct {
		SHA          string `json:"sha"`
		Verification struct {
			Signature *string `json:"signature"`
			Payload   *string `json:"payload"`
		} `json:"verification"`
	}
	if _, err := c.do(ctx, http.MethodGet, "/repos/"+repository+"/git/commits/"+sha, c.Token != nil, &out); err != nil {
		return provider.Commit{}, err
	}
	cm := provider.Commit{SHA: out.SHA}
	if out.Verification.Signature != nil {
		cm.Signature = *out.Verification.Signature
	}
	if out.Verification.Payload != nil {
		cm.Payload = *out.Verification.Payload
	}
	return cm, nil
}

// ListCommits returns recent commit ids of the default branch.
func (c *Client) ListCommits(ctx context.Context, repository string, limit int) ([]string, error) {
	if limit < 1 || limit > 100 {
		limit = 30
	}
	var out []struct {
		SHA string `json:"sha"`
	}
	if _, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/commits?per_page=%d", repository, limit), c.Token != nil, &out); err != nil {
		return nil, err
	}
	shas := make([]string, 0, len(out))
	for _, x := range out {
		shas = append(shas, x.SHA)
	}
	return shas, nil
}

// Pagination bounds for the run and job listings. The supervisor decides whether it is safe to
// start a runner from these lists, so a list that was cut short must be an error, never a
// silently shorter answer.
const (
	listPerPage  = 100
	listMaxPages = 20
)

var repoURLRe = regexp.MustCompile(`/repos/([A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9._-]+)$`)

// ListRuns returns workflow runs, newest first, following pagination. The field names are
// assumptions to confirm in BR-0 (C3). A listing longer than listMaxPages pages is an error.
func (c *Client) ListRuns(ctx context.Context, repository, status string) ([]provider.Run, error) {
	var runs []provider.Run
	total := 0
	for page := 1; page <= listMaxPages; page++ {
		q := url.Values{"per_page": {strconv.Itoa(listPerPage)}, "page": {strconv.Itoa(page)}}
		if status != "" {
			q.Set("status", status)
		}
		var out struct {
			Total int `json:"total_count"`
			Runs  []struct {
				ID       int64  `json:"id"`
				HeadSHA  string `json:"head_sha"`
				Event    string `json:"event"`
				Status   string `json:"status"`
				HeadRepo *struct {
					FullName string `json:"full_name"`
				} `json:"head_repository"`
				Actor *struct {
					Login string `json:"login"`
				} `json:"actor"`
				PullRequests []struct {
					Number int `json:"number"`
					Head   *struct {
						SHA  string `json:"sha"`
						Repo *struct {
							URL string `json:"url"`
						} `json:"repo"`
					} `json:"head"`
				} `json:"pull_requests"`
			} `json:"workflow_runs"`
		}
		if _, err := c.do(ctx, http.MethodGet, "/repos/"+repository+"/actions/runs?"+q.Encode(), true, &out); err != nil {
			return nil, err
		}
		for _, r := range out.Runs {
			run := provider.Run{ID: r.ID, HeadSHA: r.HeadSHA, Event: r.Event, Status: r.Status}
			if r.HeadRepo != nil {
				run.HeadRepository = r.HeadRepo.FullName
			}
			if r.Actor != nil {
				run.Actor = r.Actor.Login
			}
			for _, pr := range r.PullRequests {
				ref := provider.PullRequest{Number: pr.Number}
				if pr.Head != nil {
					ref.HeadSHA = pr.Head.SHA
					if pr.Head.Repo != nil {
						if m := repoURLRe.FindStringSubmatch(pr.Head.Repo.URL); m != nil {
							ref.HeadRepository = m[1]
						}
					}
				}
				run.PullRequests = append(run.PullRequests, ref)
			}
			runs = append(runs, run)
		}
		total = out.Total
		if len(runs) >= total {
			break
		}
	}
	if len(runs) != total {
		return nil, incompleteList("workflow runs", len(runs), total, "a very long queue (GitHub lists at most 1000 results), or a runaway workflow", "clear the queue (cancel old runs), then re-run")
	}
	return runs, nil
}

// incompleteList is the error for a listing whose results do not add up to its total_count.
// The supervisor decides whether it is safe to start a runner from these lists, so a short or
// capped list is never returned as if it were the whole.
func incompleteList(what string, got, total int, cause, fix string) error {
	return diag.New(diag.CodeGitHubUnavailable, fmt.Sprintf("read %d of %d %s: the list cannot be read completely", got, total, what), cause, fix)
}

// ListJobs returns every job of a run, following pagination.
func (c *Client) ListJobs(ctx context.Context, repository string, runID int64) ([]provider.Job, error) {
	var jobs []provider.Job
	total := 0
	for page := 1; page <= listMaxPages; page++ {
		q := url.Values{"per_page": {strconv.Itoa(listPerPage)}, "page": {strconv.Itoa(page)}}
		var out struct {
			Total int `json:"total_count"`
			Jobs  []struct {
				ID         int64    `json:"id"`
				RunID      int64    `json:"run_id"`
				Status     string   `json:"status"`
				Labels     []string `json:"labels"`
				RunnerName *string  `json:"runner_name"`
				HeadSHA    string   `json:"head_sha"`
			} `json:"jobs"`
		}
		if _, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/actions/runs/%d/jobs?%s", repository, runID, q.Encode()), true, &out); err != nil {
			return nil, err
		}
		for _, j := range out.Jobs {
			job := provider.Job{ID: j.ID, RunID: j.RunID, Status: j.Status, Labels: j.Labels, HeadSHA: j.HeadSHA}
			if j.RunnerName != nil {
				job.RunnerName = *j.RunnerName
			}
			jobs = append(jobs, job)
		}
		total = out.Total
		if len(jobs) >= total {
			break
		}
	}
	if len(jobs) != total {
		return nil, incompleteList(fmt.Sprintf("jobs of run %d", runID), len(jobs), total, "an unusually large matrix, or GitHub cutting the list short", "cancel the run")
	}
	return jobs, nil
}

// CancelRun asks GitHub to cancel a run. The endpoint (POST .../actions/runs/{id}/cancel,
// answering 202) is assumption C4, to confirm in BR-0.
func (c *Client) CancelRun(ctx context.Context, repository string, runID int64) error {
	return c.send(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/actions/runs/%d/cancel", repository, runID), struct{}{}, nil)
}

// GenerateJITConfig registers a single-use runner. The endpoint and fields are assumptions to
// confirm in BR-0 (spec C2).
func (c *Client) GenerateJITConfig(ctx context.Context, s provider.Scope, name string, labels []string) (provider.JITConfig, error) {
	base, err := scopePath(s)
	if err != nil {
		return provider.JITConfig{}, err
	}
	body := map[string]any{"name": name, "runner_group_id": 1, "labels": labels, "work_folder": "_work"}
	var out struct {
		Runner struct {
			ID int64 `json:"id"`
		} `json:"runner"`
		Encoded string `json:"encoded_jit_config"`
	}
	if err := c.send(ctx, http.MethodPost, base+"/actions/runners/generate-jitconfig", body, &out); err != nil {
		return provider.JITConfig{}, err
	}
	if out.Encoded == "" {
		return provider.JITConfig{}, diag.New(diag.CodeRegistrationFailed, "GitHub returned no just-in-time runner config",
			"the response had no \"encoded_jit_config\"", "see docs/decisions/0006 (C2)")
	}
	return provider.JITConfig{RunnerID: out.Runner.ID, Encoded: out.Encoded}, nil
}

// RegistrationToken mints a registration token. The token is returned, never stored.
func (c *Client) RegistrationToken(ctx context.Context, s provider.Scope) (string, error) {
	base, err := scopePath(s)
	if err != nil {
		return "", err
	}
	var out struct {
		Token string `json:"token"`
	}
	if _, err := c.do(ctx, http.MethodPost, base+"/actions/runners/registration-token", true, &out); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", diag.New(diag.CodeRegistrationFailed, "GitHub returned no registration token",
			"the response had no \"token\" field", "re-run; if it persists, the API may have changed: see docs/decisions/0003")
	}
	return out.Token, nil
}

type runnerJSON struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Busy   bool   `json:"busy"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

// ListRunners returns every runner in scope, following pagination.
func (c *Client) ListRunners(ctx context.Context, s provider.Scope) ([]provider.Runner, error) {
	base, err := scopePath(s)
	if err != nil {
		return nil, err
	}
	const perPage, maxPages = 100, 50
	var all []provider.Runner
	total := 0
	for page := 1; page <= maxPages; page++ {
		var out struct {
			TotalCount int          `json:"total_count"`
			Runners    []runnerJSON `json:"runners"`
		}
		q := url.Values{"per_page": {strconv.Itoa(perPage)}, "page": {strconv.Itoa(page)}}
		if _, err := c.do(ctx, http.MethodGet, base+"/actions/runners?"+q.Encode(), true, &out); err != nil {
			return nil, err
		}
		for _, r := range out.Runners {
			pr := provider.Runner{ID: r.ID, Name: r.Name, Status: r.Status, Busy: r.Busy}
			for _, l := range r.Labels {
				pr.Labels = append(pr.Labels, l.Name)
			}
			all = append(all, pr)
		}
		total = out.TotalCount
		if len(all) >= total {
			break
		}
	}
	if len(all) != total {
		return nil, incompleteList("runners", len(all), total, "more runners than the page limit allows, or GitHub cutting the list short", "remove runners you no longer use")
	}
	return all, nil
}

// RemoveRunner deregisters one runner by ID.
func (c *Client) RemoveRunner(ctx context.Context, s provider.Scope, id int64) error {
	base, err := scopePath(s)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, http.MethodDelete, base+"/actions/runners/"+strconv.FormatInt(id, 10), true, nil)
	return err
}

// Release resolves an actions/runner build for goos/goarch (the latest unless version is
// given) and the SHA-256 GitHub publishes in that release's notes. It never sends the
// user's token: the release is public.
func (c *Client) Release(ctx context.Context, goos, goarch, pin string) (provider.Release, error) {
	osName, archName, err := runnerPlatform(goos, goarch)
	if err != nil {
		return provider.Release{}, err
	}
	var rel struct {
		TagName string `json:"tag_name"`
		Body    string `json:"body"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	path := "/repos/actions/runner/releases/latest"
	if pin != "" {
		path = "/repos/actions/runner/releases/tags/v" + strings.TrimPrefix(pin, "v")
	}
	if _, err := c.do(ctx, http.MethodGet, path, false, &rel); err != nil {
		if pin != "" && diag.CodeOf(err) == diag.CodeTokenRejected { // a 404 here means no such release
			return provider.Release{}, diag.Wrap(err, diag.CodeReleaseUnresolved, "there is no runner release v"+strings.TrimPrefix(pin, "v"),
				"the pinned version was withdrawn, or the pin in state is wrong",
				"delete the runner_version line from the state file to install the latest, or run `bladerunner upgrade`")
		}
		return provider.Release{}, err
	}
	version := strings.TrimPrefix(rel.TagName, "v")
	want := fmt.Sprintf("actions-runner-%s-%s-%s.tar.gz", osName, archName, version)
	var assetURL string
	for _, a := range rel.Assets {
		if a.Name == want {
			assetURL = a.URL
		}
	}
	if assetURL == "" {
		return provider.Release{}, diag.New(diag.CodeReleaseUnresolved,
			fmt.Sprintf("runner release %s has no build named %s", rel.TagName, want),
			"GitHub has no runner build for this OS/architecture in that release, or renamed its files",
			"check https://github.com/actions/runner/releases, or file an issue with this message")
	}
	sum, ok := checksumFor(rel.Body, osName+"-"+archName)
	if !ok {
		return provider.Release{}, diag.New(diag.CodeReleaseUnresolved,
			fmt.Sprintf("runner release %s publishes no SHA-256 for %s-%s", rel.TagName, osName, archName),
			"the release notes do not carry the checksum in the format Blade Runner expects; it refuses to install without one",
			"see docs/decisions/0003-registration-and-release.md, or file an issue with this message")
	}
	return provider.Release{Version: version, Filename: want, URL: assetURL, SHA256: sum}, nil
}

func runnerPlatform(goos, goarch string) (osName, archName string, err error) {
	switch goos {
	case "darwin":
		osName = "osx"
	case "linux":
		osName = "linux"
	default:
		return "", "", diag.New(diag.CodeUnsupportedPlatform, "no runner build for OS "+goos, "v1 supports macOS and Linux", "run on macOS or Linux")
	}
	switch goarch {
	case "arm64":
		archName = "arm64"
	case "amd64":
		archName = "x64"
	default:
		return "", "", diag.New(diag.CodeUnsupportedPlatform, "no runner build for architecture "+goarch, "v1 supports arm64 and amd64", "run on an arm64 or amd64 machine")
	}
	return osName, archName, nil
}

// checksumFor reads <!-- BEGIN SHA key -->hex<!-- END SHA key --> from release notes.
func checksumFor(body, key string) (string, bool) {
	re := regexp.MustCompile(`<!--\s*BEGIN SHA ` + regexp.QuoteMeta(key) + `\s*-->\s*([0-9a-fA-F]{64})\s*<!--\s*END SHA ` + regexp.QuoteMeta(key) + `\s*-->`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		return "", false
	}
	return strings.ToLower(m[1]), true
}
