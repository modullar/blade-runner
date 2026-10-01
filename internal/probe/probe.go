// Package probe checks, against the real GitHub and with read-only calls (plus one temporary
// runner registration that it always deletes), the assumptions the design depends on. It is the
// automated half of BR-0: the owner runs it once, and its report says which assumptions hold.
//
// It changes nothing on the machine it runs on, installs nothing, and starts no runner. Its
// report contains no secrets, so it can be pasted into an issue or a chat.
package probe

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/provider"
	"github.com/modullar/blade-runner/internal/trust"
)

// Status of one check.
type Status string

const (
	Pass Status = "PASS"
	Fail Status = "FAIL" // the assumption is false, or the call failed
	Skip Status = "SKIP" // cannot be judged from what exists on GitHub yet: the detail says what to do
	Note Status = "NOTE" // informational
)

// Finding is the result of one check.
type Finding struct {
	ID         string // e.g. "C1"
	Assumption string // what the design assumes
	Status     Status
	Detail     string
}

// Options configure a run.
type Options struct {
	Repository string
	SkipJIT    bool
	// NewName makes the temporary runner's name; tests fix it.
	NewName func() string
	// Wait pauses between reads of a pull request whose mergeable is still null (GitHub computes
	// it lazily); nil waits a few seconds. Tests make it instant.
	Wait func(ctx context.Context)
}

func randomName() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "br0-probe-" + hex.EncodeToString(b)
}

// Run executes every check and returns the findings in order. It never panics on an
// unexpected response: a response that does not look as assumed is a Fail with the reason.
func Run(ctx context.Context, p provider.Provider, o Options) []Finding {
	if o.NewName == nil {
		o.NewName = randomName
	}
	scope := provider.Scope{Kind: "repo", Repository: o.Repository}
	var out []Finding
	out = append(out, checkAuth(ctx, p, scope))
	out = append(out, checkCommits(ctx, p, o.Repository)...)
	out = append(out, checkRuns(ctx, p, o.Repository)...)
	out = append(out, checkPullRequests(ctx, p, o)...)
	if o.SkipJIT {
		out = append(out, Finding{"C2", "a single-use (just-in-time) runner can be registered", Skip, "skipped at your request (--skip-jit)"})
	} else {
		out = append(out, checkJIT(ctx, p, scope, o.NewName()))
	}
	out = append(out, checkForkApproval(ctx, p, o.Repository))
	return out
}

// Failed reports whether any finding failed.
func Failed(fs []Finding) bool {
	for _, f := range fs {
		if f.Status == Fail {
			return true
		}
	}
	return false
}

func describe(err error) string {
	var de *diag.Error
	if e, ok := err.(*diag.Error); ok {
		de = e
	}
	if de != nil {
		s := de.Code + ": " + de.What
		if de.Err != nil {
			s += " [" + de.Err.Error() + "]"
		}
		return s
	}
	return err.Error()
}

func checkAuth(ctx context.Context, p provider.Provider, scope provider.Scope) Finding {
	f := Finding{ID: "A2", Assumption: "the token can list this repository's runners (the permission apply, remove and the supervisor need)"}
	if err := p.CheckAuth(ctx, scope); err != nil {
		f.Status, f.Detail = Fail, describe(err)+": note which token permission this needs, and try a broader one"
		return f
	}
	f.Status, f.Detail = Pass, "GET .../actions/runners succeeded with this token"
	return f
}

func checkCommits(ctx context.Context, p provider.Provider, repo string) []Finding {
	f := Finding{ID: "C1", Assumption: "GitHub returns a commit's signed bytes and signature, and the id recomputes from them"}
	shas, err := p.ListCommits(ctx, repo, 30)
	if err != nil {
		f.Status, f.Detail = Fail, "cannot list commits: "+describe(err)
		return []Finding{f}
	}
	var signed, sshSigned, otherSigned, idMatch, sigValid, payloadMissing int
	signers := map[string]int{}
	var firstBad string
	for _, sha := range shas {
		c, err := p.Commit(ctx, repo, sha)
		if err != nil {
			f.Status, f.Detail = Fail, fmt.Sprintf("cannot fetch commit %s: %s", short(sha), describe(err))
			return []Finding{f}
		}
		if c.Signature == "" {
			continue
		}
		signed++
		if !strings.Contains(c.Signature, "BEGIN SSH SIGNATURE") {
			otherSigned++
			continue
		}
		sshSigned++
		if c.Payload == "" {
			payloadMissing++
			if firstBad == "" {
				firstBad = short(sha) + ": a signature but no payload"
			}
			continue
		}
		if id, err := trust.ObjectID([]byte(c.Payload), c.Signature); err == nil && id == sha {
			idMatch++
		} else if firstBad == "" {
			firstBad = short(sha) + ": the id does not recompute from the returned bytes"
		}
		if key, err := trust.VerifySSHSig(c.Signature, []byte(c.Payload), "git"); err == nil {
			sigValid++
			signers[key.Fingerprint()]++
		} else if firstBad == "" {
			firstBad = short(sha) + ": " + err.Error()
		}
	}
	summary := fmt.Sprintf("%d commits examined: %d signed (%d SSH, %d other: unsupported), %d with payload and signature whose id recomputes, %d with a cryptographically valid signature",
		len(shas), signed, sshSigned, otherSigned, idMatch, sigValid)
	var fps []string
	for fp, n := range signers {
		fps = append(fps, fmt.Sprintf("%s (%d commits)", fp, n))
	}
	switch {
	case signed == 0:
		f.Status, f.Detail = Skip, summary+". No signed commit exists yet: make one (git config gpg.format ssh; git config user.signingkey ~/.ssh/id_ed25519; git commit -S), push it, and run this again"
	case sshSigned == 0:
		f.Status, f.Detail = Fail, summary+". Only non-SSH (GPG) signatures found, which are not supported: sign with an SSH ed25519 key"
	case idMatch == sshSigned && sigValid == sshSigned:
		f.Status, f.Detail = Pass, summary+". Signing keys seen: "+strings.Join(fps, ", ")
	default:
		f.Status, f.Detail = Fail, summary+". First problem: "+firstBad
	}
	return []Finding{f}
}

func checkRuns(ctx context.Context, p provider.Provider, repo string) []Finding {
	f := Finding{ID: "C3", Assumption: "a workflow run says which commit it executes, for which event, from which repository, on behalf of whom"}
	runs, err := p.ListRecentRuns(ctx, repo, "", 30)
	if err != nil {
		f.Status, f.Detail = Fail, "cannot list runs: "+describe(err)
		return []Finding{f}
	}
	if len(runs) == 0 {
		f.Status, f.Detail = Skip, "no workflow runs exist yet: push a commit that triggers a workflow, then run this again"
		return []Finding{f}
	}
	var noSHA, noEvent, noRepo, noActor, forks int
	events := map[string]int{}
	for _, r := range runs {
		if r.HeadSHA == "" {
			noSHA++
		}
		if r.Event == "" {
			noEvent++
		} else {
			events[r.Event]++
		}
		if r.HeadRepository == "" {
			noRepo++
		} else if !strings.EqualFold(r.HeadRepository, repo) {
			forks++
		}
		if r.Actor == "" {
			noActor++
		}
	}
	detail := fmt.Sprintf("%d runs examined; missing head_sha: %d, event: %d, head repository: %d, actor: %d; runs from another repository (forks): %d; events seen: %v",
		len(runs), noSHA, noEvent, noRepo, noActor, forks, events)
	out := []Finding{f}
	if noSHA+noEvent+noRepo+noActor > 0 {
		out[0].Status, out[0].Detail = Fail, detail
		return out
	}
	out[0].Status, out[0].Detail = Pass, detail

	jobs, err := p.ListJobs(ctx, repo, runs[0].ID)
	jf := Finding{ID: "C3b", Assumption: "a run's jobs list their labels and commit, so a job can be matched to the commit that was admitted"}
	switch {
	case err != nil:
		jf.Status, jf.Detail = Fail, "cannot list jobs: "+describe(err)
	case len(jobs) == 0:
		jf.Status, jf.Detail = Skip, "the newest run has no jobs yet"
	default:
		noLabels, noJobSHA := 0, 0
		for _, j := range jobs {
			if len(j.Labels) == 0 {
				noLabels++
			}
			if j.HeadSHA == "" {
				noJobSHA++
			}
		}
		jf.Detail = fmt.Sprintf("%d jobs in run %d; missing labels: %d, head_sha: %d", len(jobs), runs[0].ID, noLabels, noJobSHA)
		if noLabels+noJobSHA > 0 {
			jf.Status = Fail
		} else {
			jf.Status = Pass
		}
	}
	return append(out, jf)
}

func checkJIT(ctx context.Context, p provider.Provider, scope provider.Scope, name string) Finding {
	f := Finding{ID: "C2", Assumption: "a single-use (just-in-time) runner can be registered and then removed"}
	cfg, err := p.GenerateJITConfig(ctx, scope, name, []string{"self-hosted", "br0-probe"})
	if err != nil {
		f.Status, f.Detail = Fail, "generate-jitconfig failed: "+describe(err)
		return f
	}
	// From here the runner exists on GitHub. Whatever happens, try to remove it, and say so
	// plainly if that fails, because a leftover runner is something the owner must delete.
	cleanup := p.RemoveRunner(ctx, scope, cfg.RunnerID)
	switch {
	case cfg.Encoded == "" || cfg.RunnerID == 0:
		f.Status, f.Detail = Fail, "GitHub accepted the request but returned no runner id or config"
	case cleanup != nil:
		f.Status, f.Detail = Fail, fmt.Sprintf("a config was returned, but the temporary runner %q could NOT be removed (%s): delete it under Settings > Actions > Runners", name, describe(cleanup))
	default:
		f.Status, f.Detail = Pass, fmt.Sprintf("registered runner %q (id %d), received a config, and removed it again", name, cfg.RunnerID)
	}
	return f
}

func checkForkApproval(ctx context.Context, p provider.Provider, repo string) Finding {
	f := Finding{ID: "H6", Assumption: "the fork-pull-request approval setting can be read through the API"}
	policy, err := p.ForkApprovalPolicy(ctx, repo)
	if err != nil {
		f.Status, f.Detail = Note, "not readable through the API ("+describe(err)+"): `apply` will ask you to confirm the setting by hand with --fork-approval-confirmed"
		return f
	}
	f.Status, f.Detail = Pass, "approval policy is "+policy
	if policy != provider.StrictForkApproval {
		f.Status, f.Detail = Note, "approval policy is "+policy+", not the strictest ("+provider.StrictForkApproval+"): set \"Require approval for all outside collaborators\""
	}
	return f
}

func short(sha string) string {
	if len(sha) > 10 {
		return sha[:10]
	}
	return sha
}

// Render formats findings as a table that is safe to share: it never includes a token or a
// runner config.
func Render(fs []Finding) string {
	var b strings.Builder
	for _, f := range fs {
		fmt.Fprintf(&b, "[%s] %s: %s\n        %s\n", f.Status, f.ID, f.Assumption, f.Detail)
	}
	return b.String()
}
