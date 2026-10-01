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
	return &Supervisor{cfg: Config{Audit: rec}, noted: map[int64]string{}, runNoted: map[string]bool{}, attempts: map[int64]int{}}, rec
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
	if len(s.runNoted) > maxNoted || len(s.noted) > maxNoted {
		t.Errorf("runNoted %d, noted %d: the sets grew past %d", len(s.runNoted), len(s.noted), maxNoted)
	}
	if rec.n != 6*maxNoted {
		t.Errorf("%d records written, want %d: forgetting must never swallow a record", rec.n, 6*maxNoted)
	}
}

func TestBookkeepingForJobsThatLeftTheQueueIsDropped(t *testing.T) {
	s, _ := bareSupervisor()
	s.noted[1], s.noted[2] = "k", "k"
	s.attempts[1], s.attempts[2] = 3, 3
	waiting := assessment{Waiting: []judgement{{Obs: observed{Job: provider.Job{ID: 2}}}}}
	s.prune(waiting)
	if _, ok := s.noted[1]; ok {
		t.Error("a refusal remembered for a job that is gone")
	}
	if _, ok := s.attempts[1]; ok {
		t.Error("attempts remembered for a job that is gone")
	}
	if s.noted[2] != "k" || s.attempts[2] != 3 {
		t.Errorf("a job still waiting lost its bookkeeping: noted %v attempts %v", s.noted, s.attempts)
	}
}
