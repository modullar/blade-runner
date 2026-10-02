package supervisor

import (
	"fmt"
	"testing"

	"github.com/modullar/blade-runner/internal/provider"
)

// These look at the supervisor's in-memory bookkeeping, which is not visible from outside the
// package: it must stay bounded however long the supervisor runs.

type nullRecorder struct{ n int }

func (r *nullRecorder) Record(Entry) error { r.n++; return nil }

func bareSupervisor() (*Supervisor, *nullRecorder) {
	rec := &nullRecorder{}
	return &Supervisor{cfg: Config{Audit: rec, MaxAttempts: DefaultMaxAttempts},
		noted: newLRU[int64, string](maxNoted), runNoted: newLRU[string, bool](maxNoted),
		attempts: map[int64]int{}, refunds: map[int64]int{}, absent: map[int64]int{}}, rec
}

func TestTheAlreadyRecordedSetsAreBounded(t *testing.T) {
	s, rec := bareSupervisor()
	for i := 0; i < 3*maxNoted; i++ {
		if err := s.recordOnce(fmt.Sprintf("transient|%d|reason", i), Entry{Kind: KindWithheld}); err != nil {
			t.Fatal(err)
		}
		jd := judgement{Obs: observed{Run: provider.Run{ID: int64(i)}, Job: provider.Job{ID: int64(i)}}, Code: "BR-E072", Reason: "r"}
		if err := s.recordRefusal(jd); err != nil {
			t.Fatal(err)
		}
	}
	if s.runNoted.Len() > maxNoted || s.noted.Len() > maxNoted {
		t.Errorf("runNoted %d, noted %d: the sets grew past %d", s.runNoted.Len(), s.noted.Len(), maxNoted)
	}
	if rec.n != 6*maxNoted {
		t.Errorf("%d records written, want %d: forgetting must never swallow a record", rec.n, 6*maxNoted)
	}
}

func TestBookkeepingForJobsThatLeftTheQueueIsDropped(t *testing.T) {
	s, _ := bareSupervisor()
	s.noted.Put(1, "k")
	s.noted.Put(2, "k")
	s.attempts[1], s.attempts[2] = 1, 3 // job 1 had failed once (not given up on); job 2 is still waiting
	waiting := assessment{Waiting: []judgement{{Obs: observed{Job: provider.Job{ID: 2}}}}}
	s.prune(waiting)
	if _, ok := s.noted.Get(1); ok {
		t.Error("a refusal remembered for a job that is gone")
	}
	if _, ok := s.attempts[1]; ok {
		t.Error("attempts remembered for a job that is gone")
	}
	if v, _ := s.noted.Get(2); v != "k" || s.attempts[2] != 3 {
		t.Errorf("a job still waiting lost its bookkeeping: noted %v attempts %v", s.noted.Keys(), s.attempts)
	}
}

func TestAFullSetEvictsTheOldestEntryInsteadOfEmptying(t *testing.T) {
	c := newLRU[int, string](3)
	for i := 1; i <= 3; i++ {
		c.Put(i, "v")
	}
	c.Get(1) // 1 is now the most recently used; 2 is the oldest
	c.Put(4, "v")
	if _, ok := c.Get(2); ok {
		t.Error("the oldest entry was kept")
	}
	for _, k := range []int{1, 3, 4} {
		if _, ok := c.Get(k); !ok {
			t.Errorf("entry %d was lost when the set filled up: emptying it would refire every record", k)
		}
	}
	if c.Len() != 3 {
		t.Errorf("len = %d", c.Len())
	}
}

func TestAGaveUpJobIsKeptUntilItHasBeenMissingForSeveralScans(t *testing.T) {
	s, _ := bareSupervisor()
	s.attempts[7] = DefaultMaxAttempts
	s.refunds[7] = 2
	none := assessment{}
	for i := 0; i < giveUpForgetScans-1; i++ {
		s.prune(none)
		if s.attempts[7] != DefaultMaxAttempts {
			t.Fatalf("forgotten after %d missing scans, want %d", i+1, giveUpForgetScans)
		}
	}
	s.prune(none)
	if _, ok := s.attempts[7]; ok || len(s.refunds) != 0 || len(s.absent) != 0 {
		t.Errorf("still remembered after %d missing scans: attempts %v refunds %v absent %v", giveUpForgetScans, s.attempts, s.refunds, s.absent)
	}
	// Seen again in between, the count starts over.
	s.attempts[8] = DefaultMaxAttempts
	for i := 0; i < giveUpForgetScans-1; i++ {
		s.prune(none)
	}
	s.prune(assessment{Waiting: []judgement{{Obs: observed{Job: provider.Job{ID: 8}}}}})
	for i := 0; i < giveUpForgetScans-1; i++ {
		s.prune(none)
	}
	if s.attempts[8] != DefaultMaxAttempts {
		t.Error("the missing-scan count was not reset by the job being seen again")
	}
}
