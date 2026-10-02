package supervisor

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/modullar/blade-runner/internal/admit"
	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/provider"
	"github.com/modullar/blade-runner/internal/trust"
)

func TestAnUnwrittenResultSlotIsNeverAdmitted(t *testing.T) {
	var zero commitResult
	if zero.class == classAdmitted {
		t.Fatal("the zero value of a commit result reads as admitted")
	}
	results := []commitResult{{class: classAdmitted}, {}, {class: classAdmitted}} // slot 1 never written
	if i, ok := firstNotAdmitted(results); !ok || i != 1 {
		t.Errorf("firstNotAdmitted = %d, %v: an unwritten slot must hold the pull request back", i, ok)
	}
	if _, ok := firstRefused(results); ok {
		t.Error("an unwritten slot is not a refusal either: it is \"not now\"")
	}
}

func TestAnAuditRecordAlwaysKeepsTheBaseTipAndTheHeadWithTheirSigners(t *testing.T) {
	const n = 120
	head := fmt.Sprintf("%040x", n)
	v := []VerifiedCommit{{Role: "base-tip", SHA: fmt.Sprintf("%040x", 0), Signer: "maint"}}
	for i := 1; i <= n; i++ {
		v = append(v, VerifiedCommit{Role: "pr-commit", SHA: fmt.Sprintf("%040x", i), Signer: fmt.Sprintf("s%d", i)})
	}
	listed, total := auditedCommits(v, head)
	if total != n+1 || len(listed) != maxAuditedCommits {
		t.Fatalf("listed %d of %d", len(listed), total)
	}
	if listed[0].Role != "base-tip" || listed[0].Signer != "maint" {
		t.Errorf("first = %+v: the base tip must be listed", listed[0])
	}
	if last := listed[len(listed)-1]; last.SHA != head || last.Signer != fmt.Sprintf("s%d", n) {
		t.Errorf("last = %+v: the head commit and its signer must be listed", last)
	}
	// the rest are the oldest others, in order, each once
	for i := 1; i < len(listed)-1; i++ {
		if want := fmt.Sprintf("%040x", i); listed[i].SHA != want {
			t.Fatalf("listed[%d] = %s, want the %dth oldest %s", i, listed[i].SHA, i, want)
		}
	}
}

func TestASmallAuditRecordIsListedWhole(t *testing.T) {
	v := []VerifiedCommit{{Role: "base-tip", SHA: "t"}, {Role: "pr-commit", SHA: "h"}}
	if l, n := auditedCommits(v, "h"); len(l) != 2 || n != 2 {
		t.Errorf("%+v %d", l, n)
	}
}

// refusing admits everything except the shas it is told to refuse.
type refusing struct {
	mu      sync.Mutex
	refuse  map[string]bool
	fetched map[string]int
}

func (r *refusing) Admit(_ context.Context, s admit.Subject) (trust.Verdict, error) {
	r.mu.Lock()
	r.fetched[s.SHA]++
	bad := r.refuse[s.SHA]
	r.mu.Unlock()
	if bad {
		return trust.Verdict{}, diag.New(diag.CodeNotAdmitted, "commit "+s.SHA+" is bad", "bad", "fix")
	}
	return trust.Verdict{}, nil
}

func TestTheLowestRefusedCommitIsNamedWhateverTheWorkersDo(t *testing.T) {
	// Commit 1 and commit 3 are bad. A worker claims commit 1 but is held back until commit 3 has
	// been refused (the refusal sets the stop): commit 1 must still be judged, and be the one named.
	var commits []provider.PullRequestCommit
	for i := 0; i < 8; i++ {
		commits = append(commits, provider.PullRequestCommit{SHA: fmt.Sprintf("%040x", i+1)})
	}
	bad := map[string]bool{commits[1].SHA: true, commits[3].SHA: true}
	adm := &refusing{refuse: bad, fetched: map[string]int{}}
	j := &judger{s: &Supervisor{cfg: Config{Admitter: adm}}, cache: map[string]admission{}}

	threeRefused := make(chan struct{})
	var once sync.Once
	admitClaimHook = func(i int) {
		switch i {
		case 1:
			<-threeRefused
		case 3:
			// release commit 1's worker only after commit 3 has been fully admitted (refused)
			go func() {
				for {
					adm.mu.Lock()
					done := adm.fetched[commits[3].SHA] > 0
					adm.mu.Unlock()
					if done {
						once.Do(func() { close(threeRefused) })
						return
					}
				}
			}()
		}
	}
	defer func() { admitClaimHook = nil }()

	results := j.admitAll(context.Background(), []string{"o/r"}, commits)
	i, ok := firstRefused(results)
	if !ok || i != 1 {
		t.Fatalf("lowest refused = %d, %v; classes: %v", i, ok, classes(results))
	}
	for k := 5; k < 8; k++ {
		if results[k].class == classAdmitted && adm.fetched[commits[k].SHA] == 0 {
			t.Errorf("commit %d claims admitted without being fetched", k)
		}
	}
}

func classes(rs []commitResult) []admissionClass {
	var out []admissionClass
	for _, r := range rs {
		out = append(out, r.class)
	}
	return out
}
