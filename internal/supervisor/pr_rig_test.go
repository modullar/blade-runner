package supervisor_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/modullar/blade-runner/internal/admit"
	"github.com/modullar/blade-runner/internal/provider"
	"github.com/modullar/blade-runner/internal/provider/github/githubtest"
	"github.com/modullar/blade-runner/internal/supervisor"
	"github.com/modullar/blade-runner/internal/testrig"
	"github.com/modullar/blade-runner/internal/trust"
)

// prWorld is a pull request made of REAL commits: the real git and ssh-keygen sign a base tip and
// the pull request's commits with each contributor's own key, and git makes the unsigned merge
// commit of the head onto the tip, the way GitHub's "test merge" is shaped (the base tip first,
// the head second). The fake GitHub serves them all.
type prWorld struct {
	*rig
	w       *testrig.GitWorld
	baseTip testrig.RealCommit
	commits []testrig.RealCommit // the pull request's commits, oldest first; the last is the head
	merge   testrig.RealCommit
	headRep string // where the pull request's head lives: repo, or fork
	number  int
}

// prSpec describes the pull request.
type prSpec struct {
	tipBy    string   // who signs the base tip ("" leaves it unsigned); default maint
	authors  []string // who signs each pull request commit ("" is unsigned); default alice, bob, alice
	fork     bool     // the head lives in the fork
	forkOnly bool     // ... and only the fork serves the pull request's commits
	trust    []string // contributors added to the trust store; default maint, alice, bob
}

func newPRWorld(t *testing.T, spec prSpec, opts ...option) *prWorld {
	t.Helper()
	if spec.tipBy == "-" {
		spec.tipBy = ""
	} else if spec.tipBy == "" {
		spec.tipBy = "maint"
	}
	if spec.authors == nil {
		spec.authors = []string{"alice", "bob", "alice"}
	}
	if spec.trust == nil {
		spec.trust = []string{"maint", "alice", "bob"}
	}
	r := newRig(t, opts...)
	p := &prWorld{rig: r, w: testrig.NewGitWorld(t), number: 7, headRep: repo}
	if spec.fork {
		p.headRep = fork
	}
	for _, who := range spec.trust {
		p.trustWorld(who)
	}
	p.w.Commit("maint", "a.txt", "1", "init")
	p.baseTip = p.w.Commit(spec.tipBy, "a.txt", "2", "base tip")
	p.w.Checkout("pr", true)
	for i, who := range spec.authors {
		if who == "-" {
			who = "" // unsigned
		}
		p.commits = append(p.commits, p.w.Commit(who, fmt.Sprintf("f%d.txt", i), fmt.Sprint(i), fmt.Sprintf("pr commit %d", i)))
	}
	p.w.Checkout("main", false)
	p.merge = p.w.MergeCommit(p.head().SHA)

	serve := func(c testrig.RealCommit, repos ...string) {
		for _, rp := range repos {
			r.srv.AddCommit(rp, c.SHA, c.Payload, c.Signature)
		}
	}
	serve(p.baseTip, repo)
	serve(p.merge, repo)
	for _, c := range p.commits {
		switch {
		case spec.forkOnly:
			serve(c, fork)
		case spec.fork:
			serve(c, repo, fork)
		default:
			serve(c, repo)
		}
	}
	p.publish()
	return p
}

func (p *prWorld) head() testrig.RealCommit { return p.commits[len(p.commits)-1] }

func (p *prWorld) trustWorld(who string) {
	p.t.Helper()
	k, err := trust.ParsePublicKey(p.w.Key(who))
	if err != nil {
		p.t.Fatal(err)
	}
	if _, err := p.trust.Add(who, k, time.Time{}); err != nil {
		p.t.Fatal(err)
	}
}

func (p *prWorld) pubKey(who string) trust.PublicKey {
	p.t.Helper()
	k, err := trust.ParsePublicKey(p.w.Key(who))
	if err != nil {
		p.t.Fatal(err)
	}
	return k
}

func cachedAdmitter(p *prWorld) supervisor.Admitter {
	return &admit.Admitter{Provider: p.client, Verifier: &trust.Verifier{Store: p.trust}, Cache: admit.NewCommitCache(100)}
}

func (p *prWorld) fingerprint(who string) string {
	k, err := trust.ParsePublicKey(p.w.Key(who))
	if err != nil {
		p.t.Fatal(err)
	}
	return k.Fingerprint()
}

// publish (re)serves the pull request as the world stands now: open, mergeable, at the head,
// merged at p.merge onto p.baseTip.
func (p *prWorld) publish() {
	yes := true
	var list []provider.PullRequestCommit
	for i, c := range p.commits {
		list = append(list, provider.PullRequestCommit{SHA: c.SHA, Author: fmt.Sprintf("author-%d", i)})
	}
	p.srv.SetPullRequest(repo, githubtest.PullRequest{
		Info: provider.PullRequestInfo{
			Number: p.number, State: "open", BaseRepository: repo, BaseRef: "main", BaseSHA: p.baseTip.SHA,
			HeadRepository: p.headRep, HeadSHA: p.head().SHA, MergeCommitSHA: p.merge.SHA, Mergeable: &yes,
		},
		Commits: list,
	})
}

// queue adds the pull_request run and its job. Jobs are queued with the labels this runner has.
func (p *prWorld) queuePR(run, job int64, mutate ...func(*provider.Run)) {
	p.t.Helper()
	r := provider.Run{
		ID: run, HeadSHA: p.head().SHA, Event: "pull_request", Status: "queued", HeadRepository: p.headRep, Actor: "alice",
		PullRequests: []provider.PullRequest{{Number: p.number, HeadSHA: p.head().SHA, HeadRepository: p.headRep}},
	}
	for _, m := range mutate {
		m(&r)
	}
	p.srv.AddRun(repo, r)
	p.srv.AddJob(repo, provider.Job{ID: job, RunID: run, Status: "queued", Labels: []string{"self-hosted", "gpu"}, HeadSHA: r.HeadSHA})
}

// moveBase is what happens when someone pushes to the base branch after the pull request was
// admitted: a new, signed base tip, and GitHub merges the head onto it again.
func (p *prWorld) moveBase() {
	p.t.Helper()
	p.w.Detach(p.baseTip.SHA) // the base branch as it was, before the first merge commit was made on it
	p.baseTip = p.w.Commit("maint", "a.txt", "3", "base moved")
	p.merge = p.w.MergeCommit(p.head().SHA)
	p.srv.AddCommit(repo, p.baseTip.SHA, p.baseTip.Payload, p.baseTip.Signature)
	p.srv.AddCommit(repo, p.merge.SHA, p.merge.Payload, p.merge.Signature)
	p.srv.UpdatePullRequest(repo, p.number, func(pr *githubtest.PullRequest) {
		pr.Info.BaseSHA, pr.Info.MergeCommitSHA = p.baseTip.SHA, p.merge.SHA
	})
}

func (p *prWorld) editPR(f func(*provider.PullRequestInfo)) {
	p.srv.UpdatePullRequest(repo, p.number, func(pr *githubtest.PullRequest) { f(&pr.Info) })
}

// commitFetches lists the "GET .../git/commits/<sha>" requests made so far.
func (p *prWorld) commitFetches() []string {
	var out []string
	for _, req := range p.srv.Requests() {
		if strings.Contains(req, "/git/commits/") {
			out = append(out, req)
		}
	}
	return out
}

func (p *prWorld) requestsContaining(sub string) int {
	n := 0
	for _, req := range p.srv.Requests() {
		if strings.Contains(req, sub) {
			n++
		}
	}
	return n
}

// afterJIT wraps the provider so that hook runs once the single-use runner has been registered:
// the moment between the supervisor's first judgement and its second look.
type afterJIT struct {
	provider.Provider
	hook func()
}

func (a *afterJIT) GenerateJITConfig(ctx context.Context, s provider.Scope, name string, labels []string) (provider.JITConfig, error) {
	cfg, err := a.Provider.GenerateJITConfig(ctx, s, name, labels)
	if a.hook != nil {
		a.hook()
		a.hook = nil
	}
	return cfg, err
}
