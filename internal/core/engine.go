// Package core is the planner and idempotent step runner behind apply and remove.
//
// A step's Check reads the machine, never the state file, so re-running after a crash, a
// partial run or an out-of-band change converges to the same end state. Gates are
// read-only preconditions that run first, in dry-run too, so a refused run changes nothing.
package core

import (
	"context"
	"fmt"
	"strings"
)

// Status is a step's answer to "is the machine already in the desired state?".
type Status struct {
	Done   bool
	Reason string // why it is (or is not) done, shown to the user
}

// Step is one convergent change.
type Step interface {
	ID() string
	// Describe says what Apply does, for dry-run output.
	Describe() string
	// Check reports whether the desired state already holds. It must not change anything,
	// and when a prerequisite is missing it returns Done=false rather than an error, so a
	// dry-run can list every pending step.
	Check(ctx context.Context) (Status, error)
	// Apply makes the change. The engine re-runs Check afterwards and fails if the step did
	// not converge.
	Apply(ctx context.Context) error
}

// Gate is a read-only precondition. A failing gate stops the run before any change.
type Gate interface {
	ID() string
	Run(ctx context.Context, rep Reporter) error
}

// Outcome is what happened to one step.
type Outcome int

const (
	Converged Outcome = iota // already in the desired state; nothing done
	Pending                  // dry-run: would be applied
	Applied                  // changed by this run
	Failed
)

func (o Outcome) String() string {
	return [...]string{"ok", "would change", "changed", "failed"}[o]
}

// Reporter receives progress. Implementations print; tests collect.
type Reporter interface {
	Note(msg string)
	GatePassed(id string)
	StepResult(id string, o Outcome, msg string)
}

// Engine runs gates and then steps in order.
type Engine struct {
	Gates []Gate
	Steps []Step
	Store *StateStore // optional: records step outcomes
	Rep   Reporter
}

// PlanEntry is one step's line in a plan.
type PlanEntry struct {
	ID      string
	Outcome Outcome // Converged or Pending
	Reason  string
}

// Plan is the ordered result of a dry run.
type Plan []PlanEntry

// Pending returns the IDs a real run would apply, in order.
func (p Plan) Pending() []string {
	var ids []string
	for _, e := range p {
		if e.Outcome == Pending {
			ids = append(ids, e.ID)
		}
	}
	return ids
}

func (e *Engine) runGates(ctx context.Context) error {
	for _, g := range e.Gates {
		if err := g.Run(ctx, e.Rep); err != nil {
			return err
		}
		e.Rep.GatePassed(g.ID())
	}
	return nil
}

// DryRun runs the gates and checks every step without applying any.
func (e *Engine) DryRun(ctx context.Context) (Plan, error) {
	if err := e.runGates(ctx); err != nil {
		return nil, err
	}
	var plan Plan
	for _, s := range e.Steps {
		st, err := s.Check(ctx)
		if err != nil {
			return plan, fmt.Errorf("check %s: %w", s.ID(), err)
		}
		entry := PlanEntry{ID: s.ID(), Outcome: Converged, Reason: st.Reason}
		if !st.Done {
			entry.Outcome = Pending
			e.Rep.StepResult(s.ID(), Pending, s.Describe()+pendingReason(st.Reason))
		} else {
			e.Rep.StepResult(s.ID(), Converged, st.Reason)
		}
		plan = append(plan, entry)
	}
	return plan, nil
}

func pendingReason(r string) string {
	if r == "" {
		return ""
	}
	return " (" + r + ")"
}

// Apply runs the gates, then for each step: Check, Apply if needed, Check again. It stops
// at the first failure. It returns the IDs it changed.
func (e *Engine) Apply(ctx context.Context) ([]string, error) {
	if err := e.runGates(ctx); err != nil {
		return nil, err
	}
	var changed []string
	for _, s := range e.Steps {
		if err := ctx.Err(); err != nil {
			return changed, err
		}
		st, err := s.Check(ctx)
		if err != nil {
			e.fail(s, err)
			return changed, fmt.Errorf("check %s: %w", s.ID(), err)
		}
		if st.Done {
			e.Rep.StepResult(s.ID(), Converged, st.Reason)
			continue
		}
		if err := s.Apply(ctx); err != nil {
			e.fail(s, err)
			return changed, err
		}
		after, err := s.Check(ctx)
		if err == nil && !after.Done {
			err = fmt.Errorf("step %s ran but the machine is not in the desired state: %s", s.ID(), after.Reason)
		}
		if err != nil {
			e.fail(s, err)
			return changed, err
		}
		changed = append(changed, s.ID())
		e.record(s.ID(), "done", "")
		e.Rep.StepResult(s.ID(), Applied, s.Describe())
	}
	return changed, nil
}

func (e *Engine) fail(s Step, err error) {
	e.Rep.StepResult(s.ID(), Failed, firstLine(err.Error()))
	e.record(s.ID(), "failed", firstLine(err.Error()))
}

func (e *Engine) record(id, status, msg string) {
	if e.Store == nil {
		return
	}
	now := e.Store.Now
	_ = e.Store.Update(func(st *State) {
		if st.Steps == nil {
			st.Steps = map[string]StepRecord{}
		}
		rec := StepRecord{Status: status, Error: msg}
		if now != nil {
			rec.At = now().UTC()
		}
		st.Steps[id] = rec
	})
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
