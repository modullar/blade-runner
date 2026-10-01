package supervisor_test

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modullar/blade-runner/internal/admit"
	"github.com/modullar/blade-runner/internal/isolation"
	"github.com/modullar/blade-runner/internal/provider"
	"github.com/modullar/blade-runner/internal/provider/github"
	"github.com/modullar/blade-runner/internal/provider/github/githubtest"
	"github.com/modullar/blade-runner/internal/supervisor"
	"github.com/modullar/blade-runner/internal/testrig"
	"github.com/modullar/blade-runner/internal/trust"
)

const (
	repo = "acme/widgets"
	fork = "mallory/widgets"
	// testImage is pinned by content, as the supervisor requires.
	testImage = "registry.example/runner@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

var ctx = context.Background()

// lockedBuffer is a log sink safe for use from several goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// scriptedRuntime stands in for Docker where a test needs to play the part of the GitHub runner
// inside the container (taking a job at the fake GitHub): that needs the network, which a real
// isolated container does not have. It still applies the real isolation.Spec validation, so a
// spec Docker would refuse is refused here too. Real-Docker tests are in docker_test.go.
type scriptedRuntime struct {
	mu      sync.Mutex
	specs   []isolation.Spec
	removes int
	// OnRun plays the runner. nil means "ran and exited 0, took nothing".
	OnRun func(ctx context.Context, spec isolation.Spec) (isolation.Result, error)
}

func (s *scriptedRuntime) Run(ctx context.Context, spec isolation.Spec) (isolation.Result, error) {
	v, err := spec.Validate()
	if err != nil {
		return isolation.Result{}, err
	}
	s.mu.Lock()
	s.specs = append(s.specs, v)
	hook := s.OnRun
	s.mu.Unlock()
	if hook == nil {
		return isolation.Result{}, nil
	}
	return hook(ctx, v)
}

func (s *scriptedRuntime) RemoveStale(context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removes++
	return 0, nil
}

func (s *scriptedRuntime) Specs() []isolation.Spec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]isolation.Spec(nil), s.specs...)
}

type rig struct {
	t         *testing.T
	srv       *githubtest.Server
	client    *github.Client
	trust     *trust.Store
	log       *lockedBuffer
	auditPath string
	audit     *supervisor.AuditLog
	rt        *scriptedRuntime
	commits   map[string]testrig.RealCommit
	pub       map[string]string
	cfg       supervisor.Config
	sup       *supervisor.Supervisor
	prov      provider.Provider // what the supervisor talks to (the real client, possibly wrapped)
}

type option func(*rig)

func withConfig(f func(*supervisor.Config)) option { return func(r *rig) { f(&r.cfg) } }
func withProvider(f func(provider.Provider) provider.Provider) option {
	return func(r *rig) { r.prov = f(r.prov) }
}

// newRig wires the real client against the fake GitHub, the real trust store and verifier, the
// real signed commits (made by real git and ssh-keygen) and a real audit log. Only the container
// runtime is scripted (see scriptedRuntime).
func newRig(t *testing.T, opts ...option) *rig {
	t.Helper()
	srv := githubtest.New()
	t.Cleanup(srv.Close)
	srv.Repos[fork] = false // a public fork
	dir := t.TempDir()
	commits, pub := testrig.RealFixtures(t)
	for _, c := range commits {
		srv.AddCommit(repo, c.SHA, c.Payload, c.Signature)
		srv.AddCommit(fork, c.SHA, c.Payload, c.Signature)
	}
	client := &github.Client{APIURL: srv.URL, WebURL: srv.URL, Token: func(context.Context) (string, error) { return testrig.Token, nil }}
	store := &trust.Store{Path: filepath.Join(dir, "trust.json")}
	r := &rig{t: t, srv: srv, client: client, trust: store, log: &lockedBuffer{}, commits: commits, pub: pub,
		auditPath: filepath.Join(dir, "audit", "supervisor.jsonl"), rt: &scriptedRuntime{}, prov: client}
	r.trustKey("owner")
	audit, err := supervisor.OpenAuditLog(r.auditPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { audit.Close() })
	r.audit = audit
	r.cfg = supervisor.Config{
		Admitter: &admit.Admitter{Provider: client, Verifier: &trust.Verifier{Store: store}},
		Runtime:  r.rt,
		Audit:    audit,
		Log:      r.log,
		Scope:    provider.Scope{Kind: "repo", Repository: repo},
		// A runner started here is a Linux X64 runner with the extra label "gpu".
		RunnerName: "mini",
		Labels:     []string{"self-hosted", "Linux", "X64", "gpu"},
		Image:      testImage,
		// Fast timings: the watcher polls often, and nothing really sleeps.
		WatchInterval: 10 * time.Millisecond,
		Sleep:         func(context.Context, time.Duration) {},
	}
	for _, o := range opts {
		o(r)
	}
	r.cfg.Provider = r.prov
	r.sup = r.newSupervisor()
	return r
}

func (r *rig) newSupervisor() *supervisor.Supervisor {
	r.t.Helper()
	cfg := r.cfg
	cfg.Provider = r.prov
	s, err := supervisor.New(cfg)
	if err != nil {
		r.t.Fatal(err)
	}
	return s
}

func (r *rig) trustKey(who string) {
	r.t.Helper()
	k, err := trust.ParsePublicKey(r.pub[who])
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.trust.Add(who, k, time.Time{}); err != nil {
		r.t.Fatal(err)
	}
}

// q describes a queued job; zero fields take the defaults of an owner's push.
type q struct {
	run, job int64
	event    string
	who      string // whose commit: owner, mallory or unsigned
	headRepo string
	status   string // the job's status; default queued
	runState string // the run's status; default queued
	labels   []string
	prs      []provider.PullRequest
	jobSHA   string // override the job's own head_sha
	sha      string // override the commit id entirely
	noRepo   bool   // omit the head repository
}

// queue adds a run with one job to the fake GitHub and returns the commit id it is for.
func (r *rig) queue(x q) string {
	r.t.Helper()
	if x.event == "" {
		x.event = "push"
	}
	if x.who == "" {
		x.who = "owner"
	}
	if x.headRepo == "" {
		x.headRepo = repo
	}
	if x.status == "" {
		x.status = "queued"
	}
	if x.runState == "" {
		x.runState = "queued"
	}
	if x.labels == nil {
		x.labels = []string{"self-hosted", "gpu"}
	}
	sha := r.commits[x.who].SHA
	if x.sha != "" {
		sha = x.sha
	}
	hr := x.headRepo
	if x.noRepo {
		hr = ""
	}
	r.srv.AddRun(repo, provider.Run{ID: x.run, HeadSHA: sha, Event: x.event, Status: x.runState, HeadRepository: hr, Actor: x.who, PullRequests: x.prs})
	jobSHA := sha
	if x.jobSHA != "" {
		jobSHA = x.jobSHA
	}
	r.srv.AddJob(repo, provider.Job{ID: x.job, RunID: x.run, Status: x.status, Labels: x.labels, HeadSHA: jobSHA})
	return sha
}

// takes returns a runner script that behaves like the GitHub runner for one job: it connects,
// is given the job, runs it, and exits.
func (r *rig) takes(repoName string, jobID, runID int64) func(context.Context, isolation.Spec) (isolation.Result, error) {
	return func(_ context.Context, spec isolation.Spec) (isolation.Result, error) {
		r.srv.UpdateJob(repoName, jobID, func(j *provider.Job) { j.Status, j.RunnerName = "in_progress", spec.Name })
		r.srv.SetRunStatus(repoName, runID, "in_progress")
		r.srv.UpdateJob(repoName, jobID, func(j *provider.Job) { j.Status = "completed" })
		r.srv.SetRunStatus(repoName, runID, "completed")
		return isolation.Result{}, nil
	}
}

func (r *rig) entries() []supervisor.Entry {
	r.t.Helper()
	es, err := supervisor.ReadAuditLog(r.auditPath)
	if err != nil {
		r.t.Fatal(err)
	}
	return es
}

func (r *rig) entriesOfKind(kind string) []supervisor.Entry {
	var out []supervisor.Entry
	for _, e := range r.entries() {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// requestedJIT reports whether any single-use runner was ever registered at the fake GitHub.
func (r *rig) requestedJIT() bool {
	for _, req := range r.srv.Requests() {
		if strings.HasSuffix(req, "/generate-jitconfig") {
			return true
		}
	}
	return false
}

func (r *rig) cancelRequests() []string {
	var out []string
	for _, req := range r.srv.Requests() {
		if strings.HasPrefix(req, "POST ") && strings.HasSuffix(req, "/cancel") {
			out = append(out, req)
		}
	}
	return out
}

// mustStartNothing asserts the run left no trace on the machine or at GitHub: no container, no
// registration.
func (r *rig) mustStartNothing() {
	r.t.Helper()
	if n := len(r.rt.Specs()); n != 0 {
		r.t.Errorf("%d container(s) were started, want none", n)
	}
	if r.requestedJIT() {
		r.t.Error("a single-use runner was registered, want none")
	}
	if rs := r.srv.Runners(repo); len(rs) != 0 {
		r.t.Errorf("runners registered: %+v", rs)
	}
}
