package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modullar/blade-runner/internal/diag"
)

// fileStep is a real step: desired state is "this file exists", applying creates it.
// failOnce makes the first Apply fail after doing nothing, to simulate a crash.
type fileStep struct {
	id       string
	path     string
	failOnce bool
	applies  int
	noop     bool // Apply does nothing: the step cannot converge
}

func (f *fileStep) ID() string       { return f.id }
func (f *fileStep) Describe() string { return "create " + filepath.Base(f.path) }
func (f *fileStep) Check(context.Context) (Status, error) {
	if _, err := os.Stat(f.path); err == nil {
		return Status{Done: true, Reason: "already there"}, nil
	}
	return Status{Reason: "missing"}, nil
}
func (f *fileStep) Apply(context.Context) error {
	f.applies++
	if f.failOnce {
		f.failOnce = false
		return errors.New("disk on fire\nsecond line")
	}
	if f.noop {
		return nil
	}
	return os.WriteFile(f.path, []byte("x"), 0o600)
}

type gateFunc struct {
	id  string
	err error
	ran *int
}

func (g gateFunc) ID() string { return g.id }
func (g gateFunc) Run(context.Context, Reporter) error {
	if g.ran != nil {
		*g.ran++
	}
	return g.err
}

type collect struct {
	notes   []string
	gates   []string
	results map[string]Outcome
}

func newCollect() *collect { return &collect{results: map[string]Outcome{}} }

func (c *collect) Note(m string)        { c.notes = append(c.notes, m) }
func (c *collect) GatePassed(id string) { c.gates = append(c.gates, id) }
func (c *collect) StepResult(id string, o Outcome, _ string) {
	c.results[id] = o
}

func steps(dir string, names ...string) []*fileStep {
	out := make([]*fileStep, len(names))
	for i, n := range names {
		out[i] = &fileStep{id: n, path: filepath.Join(dir, n)}
	}
	return out
}

func asSteps(in []*fileStep) []Step {
	out := make([]Step, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

func TestApplyConvergesAndSecondRunChangesNothing(t *testing.T) {
	dir := t.TempDir()
	ss := steps(dir, "a", "b", "c")
	rep := newCollect()
	e := &Engine{Steps: asSteps(ss), Rep: rep}

	changed, err := e.Apply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a", "b", "c"}; !reflect.DeepEqual(changed, want) {
		t.Errorf("changed = %v, want %v", changed, want)
	}

	rep2 := newCollect()
	e.Rep = rep2
	changed, err = e.Apply(context.Background())
	if err != nil || len(changed) != 0 {
		t.Fatalf("second run: changed=%v err=%v, want no changes", changed, err)
	}
	for id, o := range rep2.results {
		if o != Converged {
			t.Errorf("second run: step %s = %v, want converged", id, o)
		}
	}
	for _, s := range ss {
		if s.applies != 1 {
			t.Errorf("step %s applied %d times across two runs, want 1", s.id, s.applies)
		}
	}
}

func TestResumeAfterFailureSkipsFinishedSteps(t *testing.T) {
	dir := t.TempDir()
	ss := steps(dir, "a", "b", "c")
	ss[1].failOnce = true
	store := &StateStore{Path: filepath.Join(dir, "state", "state.json"), Now: func() time.Time { return time.Unix(1700000000, 0) }}
	e := &Engine{Steps: asSteps(ss), Rep: newCollect(), Store: store}

	changed, err := e.Apply(context.Background())
	if err == nil || !strings.Contains(err.Error(), "disk on fire") {
		t.Fatalf("first run err = %v, want the step's failure", err)
	}
	if !reflect.DeepEqual(changed, []string{"a"}) {
		t.Errorf("first run changed = %v, want [a]", changed)
	}
	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Steps["a"].Status != "done" || st.Steps["b"].Status != "failed" {
		t.Errorf("recorded steps = %+v", st.Steps)
	}
	if got := st.Steps["b"].Error; got != "disk on fire" {
		t.Errorf("recorded error = %q, want only the first line", got)
	}
	if _, ok := st.Steps["c"]; ok {
		t.Error("step c never ran and must not be recorded")
	}

	changed, err = e.Apply(context.Background())
	if err != nil {
		t.Fatalf("resumed run: %v", err)
	}
	if want := []string{"b", "c"}; !reflect.DeepEqual(changed, want) {
		t.Errorf("resumed run changed = %v, want %v", changed, want)
	}
	if ss[0].applies != 1 {
		t.Errorf("step a re-applied on resume (%d applies)", ss[0].applies)
	}
}

func TestDryRunPlanMatchesWhatARealRunChanges(t *testing.T) {
	dir := t.TempDir()
	ss := steps(dir, "a", "b", "c")
	if err := os.WriteFile(ss[1].path, nil, 0o600); err != nil { // b already converged
		t.Fatal(err)
	}
	rep := newCollect()
	e := &Engine{Steps: asSteps(ss), Rep: rep}

	plan, err := e.DryRun(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range ss {
		if s.applies != 0 {
			t.Fatalf("dry run applied step %s", s.id)
		}
	}
	if _, err := os.Stat(ss[0].path); err == nil {
		t.Fatal("dry run created a file")
	}
	changed, err := e.Apply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.Pending(), changed) {
		t.Errorf("dry-run pending %v != real run changes %v", plan.Pending(), changed)
	}
	if want := []string{"a", "c"}; !reflect.DeepEqual(changed, want) {
		t.Errorf("changed = %v, want %v", changed, want)
	}
}

func TestGateFailureStopsBeforeAnyChange(t *testing.T) {
	dir := t.TempDir()
	ss := steps(dir, "a")
	refused := diag.New(diag.CodePublicRepoRefused, "refused", "public", "pass the flag")
	ran := 0
	e := &Engine{
		Gates: []Gate{gateFunc{id: "ok", ran: &ran}, gateFunc{id: "no", err: refused}, gateFunc{id: "never", ran: &ran}},
		Steps: asSteps(ss),
		Rep:   newCollect(),
	}
	for name, run := range map[string]func() error{
		"apply":   func() error { _, err := e.Apply(context.Background()); return err },
		"dry-run": func() error { _, err := e.DryRun(context.Background()); return err },
	} {
		ran = 0
		err := run()
		if diag.CodeOf(err) != diag.CodePublicRepoRefused {
			t.Errorf("%s: err = %v, want the gate's diag error", name, err)
		}
		if ran != 1 {
			t.Errorf("%s: gates after the failing one must not run (ran=%d, want 1)", name, ran)
		}
		if ss[0].applies != 0 {
			t.Errorf("%s: a step ran although a gate failed", name)
		}
	}
}

func TestStepThatDoesNotConvergeFails(t *testing.T) {
	dir := t.TempDir()
	ss := steps(dir, "liar")
	ss[0].noop = true
	rep := newCollect()
	e := &Engine{Steps: asSteps(ss), Rep: rep}
	_, err := e.Apply(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not in the desired state") {
		t.Fatalf("err = %v, want a convergence failure", err)
	}
	if rep.results["liar"] != Failed {
		t.Errorf("outcome = %v, want failed", rep.results["liar"])
	}
}

func TestCancelledContextStopsBetweenSteps(t *testing.T) {
	dir := t.TempDir()
	ss := steps(dir, "a", "b")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	changed, err := (&Engine{Steps: asSteps(ss), Rep: newCollect()}).Apply(ctx)
	if !errors.Is(err, context.Canceled) || len(changed) != 0 {
		t.Errorf("changed=%v err=%v, want nothing done and context.Canceled", changed, err)
	}
}
