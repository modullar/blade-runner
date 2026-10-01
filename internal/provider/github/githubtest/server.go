// Package githubtest is a real HTTP server that behaves like the slice of GitHub Blade
// Runner uses: runners API, registration tokens, repository visibility, and the runner
// release with its checksum notes. Tests point the GitHub client at it, so the client's
// real request and response handling runs, unlike a recorded snapshot.
//
// It encodes the assumptions the code makes about GitHub (endpoint shapes, release-note
// checksum markers); it cannot confirm them. BR-0 does that against the live API.
package githubtest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/modullar/blade-runner/internal/provider"
	"strconv"
	"strings"
	"sync"
)

// Version is the runner release the server publishes.
const Version = "2.999.0"

// Server is the fake. Configure it before use; fields guarded by mu may be changed
// through the methods while a test runs.
type Server struct {
	*httptest.Server

	// Token is the only API token accepted.
	Token string
	// Repos maps OWNER/REPO to whether it is private. Orgs lists known organizations.
	Repos map[string]bool
	Orgs  map[string]bool
	// Forbidden makes every authenticated call answer 403 (a token without the scope).
	Forbidden bool
	// Down makes every call answer 503.
	Down bool
	// ForkApproval is the policy served per repository (default: the strictest).
	// ForkApprovalUnsupported makes the endpoint answer 404, as an API that lacks it would.
	ForkApproval map[string]string
	commits      map[string]commitData // "owner/repo@sha"
	commitOrder  map[string][]string   // repo -> shas, oldest first
	runs         map[string][]provider.Run
	jobs         map[string][]provider.Job
	// JITUnsupported makes generate-jitconfig answer 404, as an API without it would.
	JITUnsupported          bool
	ForkApprovalUnsupported bool
	// CancelIgnored makes the cancel endpoint answer 202 without cancelling anything, as a
	// cancellation that is slow or never takes effect would look. CancelUnsupported answers 404.
	CancelIgnored     bool
	CancelUnsupported bool
	// ResultCap makes the run and job listings return only the first ResultCap results however
	// many pages are asked for, while total_count still reports the full count: GitHub's list
	// endpoints stop at 1000 results. 0 means no cap.
	ResultCap int
	// MaxPerPage makes a listing page hold at most this many items even when per_page asks for
	// more (a short page that is not the last one). 0 means no limit.
	MaxPerPage int
	// JobRunIDOffset is added to the run_id the jobs endpoint reports, as inconsistent data would.
	JobRunIDOffset int64
	// FailPathContains makes every request whose path contains it answer 500.
	FailPathContains string
	// Tarball is the runner archive served; ChecksumOverride and OmitChecksum corrupt the
	// published checksum on purpose.
	Tarball          []byte
	ChecksumOverride string
	OmitChecksum     bool

	mu        sync.Mutex
	runners   map[string][]*Runner // by scope target
	nextID    int64
	regTokens map[string]string // registration token -> scope target
	requests  []string
	uris      []string // "METHOD /path?query"
	// ReleaseAuthHeader is the Authorization header seen on the release lookup.
	ReleaseAuthHeader string
}

// Runner is a registered runner.
type Runner struct {
	ID     int64
	Name   string
	Labels []string
	Online bool
	Busy   bool
}

// New starts a server. Call Close when done.
func New() *Server {
	s := &Server{
		Token:     "ghp_testtoken",
		Repos:     map[string]bool{"acme/widgets": true},
		Orgs:      map[string]bool{"acme": true},
		Tarball:   RunnerTarball(),
		runners:   map[string][]*Runner{},
		regTokens: map[string]string{},
		nextID:    100,
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

type commitData struct{ payload, signature string }

// AddCommit serves a commit (its signed payload and signature) for repo at sha. Pass the
// signature "" for an unsigned commit. The server returns whatever it is given: it does not
// check that the sha is the hash of the content, so tests can model a dishonest server.
func (s *Server) AddCommit(repo, sha, payload, signature string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.commits == nil {
		s.commits = map[string]commitData{}
	}
	s.commits[repo+"@"+sha] = commitData{payload, signature}
	if s.commitOrder == nil {
		s.commitOrder = map[string][]string{}
	}
	s.commitOrder[repo] = append(s.commitOrder[repo], sha)
}

// AddRun makes a workflow run visible for repo (newest last; the API lists newest first).
func (s *Server) AddRun(repo string, r provider.Run) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runs == nil {
		s.runs = map[string][]provider.Run{}
	}
	s.runs[repo] = append(s.runs[repo], r)
}

// AddJob makes a job visible under its run.
func (s *Server) AddJob(repo string, j provider.Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.jobs == nil {
		s.jobs = map[string][]provider.Job{}
	}
	s.jobs[repo] = append(s.jobs[repo], j)
}

// page returns the slice of n items that the request's page/per_page select (default: all).
func (s *Server) page(r *http.Request, n int) (lo, hi int) {
	if s.ResultCap > 0 && n > s.ResultCap {
		n = s.ResultCap
	}
	lo, hi = pageOf(r, n)
	if s.MaxPerPage > 0 && hi-lo > s.MaxPerPage {
		hi = lo + s.MaxPerPage
	}
	return lo, hi
}

func pageOf(r *http.Request, n int) (lo, hi int) {
	perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	pg, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if perPage < 1 {
		return 0, n
	}
	if pg < 1 {
		pg = 1
	}
	lo, hi = (pg-1)*perPage, pg*perPage
	if lo > n {
		lo = n
	}
	if hi > n {
		hi = n
	}
	return lo, hi
}

func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request, repo string, rest []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Method == http.MethodPost && len(rest) == 3 && rest[2] == "cancel" { // POST .../actions/runs/{id}/cancel
		id, _ := strconv.ParseInt(rest[1], 10, 64)
		if s.CancelUnsupported {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
			return
		}
		found := false
		for i := range s.runs[repo] {
			if s.runs[repo][i].ID == id {
				found = true
				if !s.CancelIgnored {
					s.runs[repo][i].Status = "completed"
					for j := range s.jobs[repo] {
						if s.jobs[repo][j].RunID == id && s.jobs[repo][j].Status != "in_progress" {
							s.jobs[repo][j].Status = "completed"
						}
					}
				}
			}
		}
		if !found {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{})
		return
	}
	if r.Method == http.MethodGet && len(rest) == 1 { // GET .../actions/runs[?status=]
		status := r.URL.Query().Get("status")
		var matching []provider.Run
		all := s.runs[repo]
		for i := len(all) - 1; i >= 0; i-- { // newest first
			if status == "" || all[i].Status == status {
				matching = append(matching, all[i])
			}
		}
		lo, hi := s.page(r, len(matching))
		out := []map[string]any{}
		for _, run := range matching[lo:hi] {
			prs := []map[string]any{}
			for _, pr := range run.PullRequests {
				prs = append(prs, map[string]any{"number": pr.Number, "head": map[string]any{
					"sha": pr.HeadSHA, "repo": map[string]string{"url": s.URL + "/repos/" + pr.HeadRepository}}})
			}
			entry := map[string]any{
				"id": run.ID, "head_sha": run.HeadSHA, "event": run.Event, "status": run.Status,
				"actor": map[string]string{"login": run.Actor}, "pull_requests": prs,
			}
			if run.HeadRepository != "" {
				entry["head_repository"] = map[string]string{"full_name": run.HeadRepository}
			}
			out = append(out, entry)
		}
		writeJSON(w, http.StatusOK, map[string]any{"total_count": len(matching), "workflow_runs": out})
		return
	}
	if r.Method == http.MethodGet && len(rest) == 3 && rest[2] == "jobs" { // GET .../actions/runs/{id}/jobs
		id, _ := strconv.ParseInt(rest[1], 10, 64)
		var mine []provider.Job
		for _, j := range s.jobs[repo] {
			if j.RunID == id {
				mine = append(mine, j)
			}
		}
		lo, hi := s.page(r, len(mine))
		out := []map[string]any{}
		for _, j := range mine[lo:hi] {
			var runner any
			if j.RunnerName != "" {
				runner = j.RunnerName
			}
			out = append(out, map[string]any{"id": j.ID, "run_id": j.RunID + s.JobRunIDOffset, "status": j.Status, "labels": j.Labels, "runner_name": runner, "head_sha": j.HeadSHA})
		}
		writeJSON(w, http.StatusOK, map[string]any{"total_count": len(mine), "jobs": out})
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
}

// SetFailPath sets FailPathContains safely while requests are in flight.
func (s *Server) SetFailPath(contains string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.FailPathContains = contains
}

// SetRunStatus changes a run's status, as GitHub does when a run starts or finishes.
func (s *Server) SetRunStatus(repo string, runID int64, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.runs[repo] {
		if s.runs[repo][i].ID == runID {
			s.runs[repo][i].Status = status
		}
	}
}

// UpdateJob edits a job in place, as GitHub does when a runner takes it or it finishes.
func (s *Server) UpdateJob(repo string, jobID int64, edit func(*provider.Job)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.jobs[repo] {
		if s.jobs[repo][i].ID == jobID {
			edit(&s.jobs[repo][i])
		}
	}
}

// Job returns a copy of a job, and whether it exists.
func (s *Server) Job(repo string, jobID int64) (provider.Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs[repo] {
		if j.ID == jobID {
			return j, true
		}
	}
	return provider.Job{}, false
}

// Run returns a copy of a run, and whether it exists.
func (s *Server) Run(repo string, runID int64) (provider.Run, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.runs[repo] {
		if r.ID == runID {
			return r, true
		}
	}
	return provider.Run{}, false
}

func (s *Server) handleCommitList(w http.ResponseWriter, r *http.Request, repo string) {
	s.mu.Lock()
	order := append([]string(nil), s.commitOrder[repo]...)
	s.mu.Unlock()
	if _, known := s.Repos[repo]; !known {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	out := []map[string]string{}
	for i := len(order) - 1; i >= 0; i-- {
		out = append(out, map[string]string{"sha": order[i]})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCommit(w http.ResponseWriter, r *http.Request, repo, sha string) {
	private, known := s.Repos[repo]
	authed := r.Header.Get("Authorization") == "Bearer "+s.Token
	s.mu.Lock()
	cd, have := s.commits[repo+"@"+sha]
	s.mu.Unlock()
	if !known || !have || (private && !authed) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	v := map[string]any{"verified": false, "reason": "unknown_key", "signature": nil, "payload": nil}
	if cd.signature != "" {
		v["signature"], v["payload"] = cd.signature, cd.payload
	} else {
		v["reason"] = "unsigned"
	}
	writeJSON(w, http.StatusOK, map[string]any{"sha": sha, "verification": v})
}

// Requests returns "METHOD /path" for every request so far.
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

// URIs returns "METHOD /path?query" for every request so far, to assert how a client paged.
func (s *Server) URIs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.uris...)
}

// Runners returns a copy of the runners registered at target.
func (s *Server) Runners(target string) []Runner {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Runner
	for _, r := range s.runners[target] {
		out = append(out, *r)
	}
	return out
}

// SetOnline flips a runner's connection state, as a crash or restart would.
func (s *Server) SetOnline(target, name string, online bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.runners[target] {
		if r.Name == name {
			r.Online = online
		}
	}
}

// AddRunner registers a runner directly.
func (s *Server) AddRunner(target, name string, labels []string, online bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.register(target, name, labels, online)
}

func (s *Server) register(target, name string, labels []string, online bool) {
	kept := s.runners[target][:0]
	for _, r := range s.runners[target] { // --replace semantics
		if r.Name != name {
			kept = append(kept, r)
		}
	}
	s.nextID++
	s.runners[target] = append(kept, &Runner{ID: s.nextID, Name: name, Labels: labels, Online: online})
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, r.Method+" "+r.URL.Path)
	s.uris = append(s.uris, r.Method+" "+r.URL.RequestURI())
	down := s.Down
	failPath := s.FailPathContains
	s.mu.Unlock()
	if failPath != "" && strings.Contains(r.URL.Path, failPath) {
		http.Error(w, `{"message":"Internal Server Error"}`, http.StatusInternalServerError)
		return
	}
	if down {
		http.Error(w, `{"message":"Service Unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	p := r.URL.Path
	switch {
	case p == "/_runner/register":
		s.handleRegister(w, r)
		return
	case strings.HasPrefix(p, "/download/"):
		s.handleDownload(w, r)
		return
	case p == "/repos/actions/runner/releases/latest", p == "/repos/actions/runner/releases/tags/v"+Version:
		s.handleRelease(w, r)
		return
	case strings.HasPrefix(p, "/repos/actions/runner/releases/tags/"):
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}

	// Repository visibility is readable without a token for public repositories.
	if parts := strings.Split(strings.Trim(p, "/"), "/"); len(parts) == 4 && parts[0] == "repos" && parts[3] == "commits" && r.Method == http.MethodGet {
		if !s.authorized(w, r) {
			return
		}
		s.handleCommitList(w, r, parts[1]+"/"+parts[2])
		return
	}
	if parts := strings.Split(strings.Trim(p, "/"), "/"); len(parts) == 6 && parts[0] == "repos" && parts[3] == "git" && parts[4] == "commits" && r.Method == http.MethodGet {
		s.handleCommit(w, r, parts[1]+"/"+parts[2], parts[5])
		return
	}
	if parts := strings.Split(strings.Trim(p, "/"), "/"); len(parts) == 3 && parts[0] == "repos" && r.Method == http.MethodGet {
		s.handleRepo(w, r, parts[1]+"/"+parts[2])
		return
	}
	if !s.authorized(w, r) {
		return
	}
	s.handleRunners(w, r)
}

func (s *Server) authorized(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Authorization") != "Bearer "+s.Token {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
		return false
	}
	s.mu.Lock()
	forbidden := s.Forbidden
	s.mu.Unlock()
	if forbidden {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "Resource not accessible by personal access token"})
		return false
	}
	return true
}

func (s *Server) handleRepo(w http.ResponseWriter, r *http.Request, repo string) {
	private, ok := s.Repos[repo]
	authed := r.Header.Get("Authorization") == "Bearer "+s.Token
	if !ok || (private && !authed) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	vis := "public"
	if private {
		vis = "private"
	}
	writeJSON(w, http.StatusOK, map[string]any{"full_name": repo, "private": private, "visibility": vis})
}

// scopeTarget maps /repos/o/r/actions/... or /orgs/o/actions/... to a scope target.
func (s *Server) scopeTarget(parts []string) (target string, rest []string, ok bool) {
	switch {
	case len(parts) >= 4 && parts[0] == "repos" && parts[3] == "actions":
		if _, exists := s.Repos[parts[1]+"/"+parts[2]]; !exists {
			return "", nil, false
		}
		return parts[1] + "/" + parts[2], parts[4:], true
	case len(parts) >= 3 && parts[0] == "orgs" && parts[2] == "actions":
		if !s.Orgs[parts[1]] {
			return "", nil, false
		}
		return parts[1], parts[3:], true
	}
	return "", nil, false
}

func (s *Server) handleRunners(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) == 6 && parts[0] == "repos" && parts[3] == "actions" && parts[4] == "permissions" && parts[5] == "fork-pr-contributor-approval" {
		s.mu.Lock()
		unsupported := s.ForkApprovalUnsupported
		policy := s.ForkApproval[parts[1]+"/"+parts[2]]
		s.mu.Unlock()
		if _, known := s.Repos[parts[1]+"/"+parts[2]]; !known || unsupported {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
			return
		}
		if policy == "" {
			policy = "all_external_contributors"
		}
		writeJSON(w, http.StatusOK, map[string]string{"approval_policy": policy})
		return
	}
	target, rest, ok := s.scopeTarget(parts)
	if ok && len(rest) >= 1 && rest[0] == "runs" && (r.Method == http.MethodGet || r.Method == http.MethodPost) {
		s.handleRuns(w, r, parts[1]+"/"+parts[2], rest)
		return
	}
	if !ok || len(rest) == 0 || rest[0] != "runners" {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && len(rest) == 2 && rest[1] == "generate-jitconfig":
		if s.JITUnsupported {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
			return
		}
		var req struct {
			Name   string   `json:"name"`
			Labels []string `json:"labels"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Name == "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "name is required"})
			return
		}
		s.register(target, req.Name, req.Labels, false) // registered, but nothing has started it
		id := s.runners[target][len(s.runners[target])-1].ID
		writeJSON(w, http.StatusCreated, map[string]any{"runner": map[string]any{"id": id, "name": req.Name}, "encoded_jit_config": fmt.Sprintf("JIT-%d", id)})
	case r.Method == http.MethodGet && len(rest) == 1:
		perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if perPage < 1 {
			perPage = 30
		}
		if page < 1 {
			page = 1
		}
		all := s.runners[target]
		lo, hi := (page-1)*perPage, page*perPage
		if lo > len(all) {
			lo = len(all)
		}
		if hi > len(all) {
			hi = len(all)
		}
		out := []map[string]any{}
		for _, rn := range all[lo:hi] {
			status := "offline"
			if rn.Online {
				status = "online"
			}
			labels := []map[string]string{}
			for _, l := range rn.Labels {
				labels = append(labels, map[string]string{"name": l})
			}
			out = append(out, map[string]any{"id": rn.ID, "name": rn.Name, "status": status, "busy": rn.Busy, "labels": labels})
		}
		writeJSON(w, http.StatusOK, map[string]any{"total_count": len(all), "runners": out})
	case r.Method == http.MethodPost && len(rest) == 2 && rest[1] == "registration-token":
		tok := fmt.Sprintf("REG-%d", len(s.regTokens)+1)
		s.regTokens[tok] = target
		writeJSON(w, http.StatusCreated, map[string]string{"token": tok, "expires_at": "2099-01-01T00:00:00Z"})
	case r.Method == http.MethodDelete && len(rest) == 2:
		id, _ := strconv.ParseInt(rest[1], 10, 64)
		kept := s.runners[target][:0]
		found := false
		for _, rn := range s.runners[target] {
			if rn.ID == id {
				found = true
				continue
			}
			kept = append(kept, rn)
		}
		s.runners[target] = kept
		if !found {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
	}
}

// handleRegister stands in for the runner talking to GitHub when config.sh runs: it only
// accepts a registration token minted by this server, and only once.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tok := r.Header.Get("X-Reg-Token")
	s.mu.Lock()
	defer s.mu.Unlock()
	target, ok := s.regTokens[tok]
	if !ok || target != q.Get("target") {
		http.Error(w, "invalid or reused registration token", http.StatusUnauthorized)
		return
	}
	delete(s.regTokens, tok)
	s.register(target, q.Get("name"), strings.Split(q.Get("labels"), ","), true)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	data := s.Tarball
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/gzip")
	_, _ = w.Write(data)
}

func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.ReleaseAuthHeader = r.Header.Get("Authorization")
	sum := s.ChecksumOverride
	omit := s.OmitChecksum
	data := s.Tarball
	s.mu.Unlock()
	if sum == "" {
		h := sha256.Sum256(data)
		sum = hex.EncodeToString(h[:])
	}
	var body strings.Builder
	body.WriteString("## Release notes\n\n## SHA-256 Checksums\n\n")
	var assets []map[string]string
	for _, plat := range []string{"osx-arm64", "osx-x64", "linux-arm64", "linux-x64"} {
		name := fmt.Sprintf("actions-runner-%s-%s.tar.gz", plat, Version)
		if !omit {
			fmt.Fprintf(&body, "- %s <!-- BEGIN SHA %s -->%s<!-- END SHA %s -->\n", name, plat, sum, plat)
		}
		assets = append(assets, map[string]string{"name": name, "browser_download_url": s.URL + "/download/" + name})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tag_name": "v" + Version, "body": body.String(), "assets": assets})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// fakeConfigSh plays the role of the runner's config.sh. It is a real script that real
// processes execute, and it is strict about the two things the spec demands: the
// registration token arrives in the environment (never argv), and registration only
// succeeds with a token GitHub issued.
const fakeConfigSh = `#!/bin/sh
set -eu
url=""; name=""; labels=""; work=""
: > .config-args
for a in "$@"; do printf '%s\n' "$a" >> .config-args; done
while [ $# -gt 0 ]; do
  case "$1" in
    --url) url="$2"; shift 2;;
    --name) name="$2"; shift 2;;
    --labels) labels="$2"; shift 2;;
    --work) work="$2"; shift 2;;
    --unattended|--replace) shift;;
    --token) echo "fake config.sh: token in argv" >&2; exit 9;;
    *) echo "fake config.sh: unknown argument $1" >&2; exit 2;;
  esac
done
[ -n "${ACTIONS_RUNNER_INPUT_TOKEN:-}" ] || { echo "fake config.sh: no token in the environment" >&2; exit 3; }
root=$(printf '%s' "$url" | sed -E 's#^(https?://[^/]+).*#\1#')
target=${url#"$root"/}
# The real runner adds self-hosted, the OS and the architecture to whatever --labels says
# (an assumption recorded for BR-0); this double does the same, for a Linux/X64 "machine".
all="self-hosted,Linux,X64"
[ -z "$labels" ] || all="$all,$labels"
curl -fsS -X POST -H "X-Reg-Token: $ACTIONS_RUNNER_INPUT_TOKEN" \
  "$root/_runner/register?target=$target&name=$name&labels=$all" >/dev/null \
  || { echo "fake config.sh: GitHub refused the registration token" >&2; exit 4; }
printf '\357\273\277{"agentName": "%s", "gitHubUrl": "%s", "workFolder": "%s"}\n' "$name" "$url" "$work" > .runner
`

// RunnerTarball builds a small actions-runner-shaped archive: config.sh (see fakeConfigSh),
// run.sh and a bin directory, with the executable bits the real archive has.
func RunnerTarball() []byte { return RunnerTarballWith(fakeConfigSh) }

// RunnerTarballWith is RunnerTarball with a chosen config.sh, to simulate a configure
// script that fails.
func RunnerTarballWith(configSh string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	add := func(name string, mode int64, body string, typ byte) {
		h := &tar.Header{Name: name, Mode: mode, Size: int64(len(body)), Typeflag: typ}
		if typ == tar.TypeDir {
			h.Size = 0
		}
		_ = tw.WriteHeader(h)
		if typ == tar.TypeReg {
			_, _ = tw.Write([]byte(body))
		}
	}
	add("bin/", 0o755, "", tar.TypeDir)
	add("bin/Runner.Listener", 0o755, "#!/bin/sh\nexit 0\n", tar.TypeReg)
	add("config.sh", 0o755, configSh, tar.TypeReg)
	add("run.sh", 0o755, "#!/bin/sh\nexec sleep 86400\n", tar.TypeReg)
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}
