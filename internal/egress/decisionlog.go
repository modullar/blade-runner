package egress

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"
)

// DefaultMaxLoggedDenials is how many refused requests of EACH reason a proxy itemises before it
// only counts them. It is per reason so one noisy reason (a job probing names that are not on
// the list) cannot use up the budget of the others.
const DefaultMaxLoggedDenials = 100

// defaultSummaryEvery is the least time between two summary lines, and the length of the window
// over which repeated allowed decisions are aggregated.
const defaultSummaryEvery = 10 * time.Second

// maxAllowedKeys bounds how many distinct host:port pairs get a "first" line per window. A
// wildcard allowlist entry lets a job invent unlimited names; past this the rest of the window's
// allowed decisions are only counted, under one aggregate line.
const maxAllowedKeys = 200

// neverSuppressed are the refusals that are evidence of an attempt to reach inside (an address
// the policy forbids, an IP literal that skips the name check). Every one is itemised,
// whatever the budget: they are what an operator must be able to find in the log.
var neverSuppressed = map[string]bool{ReasonForbiddenAddr: true, ReasonIPLiteral: true}

// DecisionLog writes the proxy's decisions as JSON lines and keeps floods from pushing the
// records that matter out of the log.
//
// The log is read back from Docker's size-capped, rotating log (see Session.Decisions), so what
// is written competes for a few hundred KiB. So:
//
//   - refusals are itemised up to a budget PER REASON (the first, most telling ones); later ones
//     are counted and reported in a periodic summary line (ReasonSuppressed) whose size does not
//     depend on the number of requests. forbidden-address and ip-literal are never suppressed;
//   - allowed decisions are aggregated per host:port per window: the first is written at once,
//     and the rest of the window becomes ONE line carrying the count and the LAST one's time and
//     client. A burst of short allowed CONNECTs therefore cannot rotate the log either;
//   - nothing waits for the next request: Tick (driven by a timer) and Flush write what is
//     pending, so a quiet proxy's log is never stale.
type DecisionLog struct {
	MaxDenied    int           // itemised refusals per reason; default DefaultMaxLoggedDenials
	SummaryEvery time.Duration // summary and aggregation window; default 10s

	mu      sync.Mutex
	enc     *json.Encoder
	now     func() time.Time
	denied  map[string]int // refusals itemised so far, by reason
	skipped map[string]int // refusals counted but not itemised, by reason
	told    map[string]int // of skipped, how many the last summary included
	lastSum time.Time

	winStart time.Time
	allowed  map[string]*allowedAgg // this window's allowed decisions, by host:port
	flushes  int                    // flush requests answered (see Ack)
}

type allowedAgg struct {
	n    int      // decisions counted but not itemised
	last Decision // the most recent of them
	rest bool     // the aggregate for hosts past maxAllowedKeys: nothing of it was itemised
}

// NewDecisionLog logs to w.
func NewDecisionLog(w io.Writer) *DecisionLog {
	return &DecisionLog{enc: json.NewEncoder(w), now: time.Now,
		denied: map[string]int{}, skipped: map[string]int{}, told: map[string]int{}, allowed: map[string]*allowedAgg{}}
}

func (l *DecisionLog) every() time.Duration {
	if l.SummaryEvery <= 0 {
		return defaultSummaryEvery
	}
	return l.SummaryEvery
}

func (l *DecisionLog) budget() int {
	if l.MaxDenied <= 0 {
		return DefaultMaxLoggedDenials
	}
	return l.MaxDenied
}

// Record writes d, or counts it. It is safe for concurrent use.
func (l *DecisionLog) Record(d Decision) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.roll(now)
	switch {
	case d.Allowed:
		l.recordAllowed(d)
	case neverSuppressed[d.Reason] || l.denied[d.Reason] < l.budget():
		l.denied[d.Reason]++
		_ = l.enc.Encode(d)
	default:
		l.skipped[d.Reason]++
		if now.Sub(l.lastSum) >= l.every() {
			l.summarise(now)
		}
	}
}

func (l *DecisionLog) recordAllowed(d Decision) {
	key := d.Host + ":" + d.Port
	a := l.allowed[key]
	if a == nil {
		if len(l.allowed) >= maxAllowedKeys {
			if a = l.allowed[""]; a == nil {
				a = &allowedAgg{rest: true}
				l.allowed[""] = a
			}
		} else {
			l.allowed[key] = &allowedAgg{}
			_ = l.enc.Encode(d) // the first of the window: written at once
			return
		}
	}
	a.n++
	a.last = d
}

// roll closes the aggregation window when it has run its length.
func (l *DecisionLog) roll(now time.Time) {
	if l.winStart.IsZero() {
		l.winStart = now
		return
	}
	if now.Sub(l.winStart) >= l.every() {
		l.writeAllowedAggregates()
		l.winStart = now
	}
}

// writeAllowedAggregates writes one line per host:port that repeated in the window, then starts
// the next window empty (so the next decision of each is a "first" again).
func (l *DecisionLog) writeAllowedAggregates() {
	keys := make([]string, 0, len(l.allowed))
	for k := range l.allowed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a := l.allowed[k]
		if a.n == 0 {
			continue
		}
		d := a.last
		d.Count = a.n
		d.Allowed, d.Reason = true, ReasonAllowed
		if a.rest {
			d.Host, d.Port, d.Resolved = "", "", nil
			d.Detail = fmt.Sprintf("%d more allowed tunnels to other hosts this window (not itemised; time and client are the last one's)", a.n)
		} else {
			d.Detail = fmt.Sprintf("%d more allowed tunnels this window after the first (time and client are the last one's)", a.n)
		}
		_ = l.enc.Encode(d)
	}
	l.allowed = map[string]*allowedAgg{}
}

// Tick writes what is pending if its time has come. A timer drives it, so that a proxy that has
// gone quiet still leaves a current log.
func (l *DecisionLog) Tick() {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.roll(now)
	if l.pending() && now.Sub(l.lastSum) >= l.every() {
		l.summarise(now)
	}
}

// Run calls Tick every SummaryEvery (at most every second) until ctx ends.
func (l *DecisionLog) Run(ctx context.Context) {
	step := l.every()
	if step > time.Second {
		step = time.Second
	}
	t := time.NewTicker(step)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.Tick()
		}
	}
}

func (l *DecisionLog) pending() bool {
	for r, n := range l.skipped {
		if n > l.told[r] {
			return true
		}
	}
	return false
}

// Flush writes everything pending: the open window's aggregates and a summary of refusals counted
// since the last one.
func (l *DecisionLog) Flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.writeAllowedAggregates()
	l.winStart = l.now()
	if l.pending() {
		l.summarise(l.now())
	}
}

// Ack is Flush plus a marker line (ReasonFlushed) saying how many flushes have been answered. A
// reader that asked for a flush (by signal) waits for the marker's count to rise, which is the
// only proof that the lines it wanted have been written.
func (l *DecisionLog) Ack() {
	l.Flush()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.flushes++
	_ = l.enc.Encode(Decision{Time: l.now().UTC(), Reason: ReasonFlushed, Count: l.flushes})
}

// summarise writes one line per reason with refusals not yet reported.
func (l *DecisionLog) summarise(now time.Time) {
	l.lastSum = now
	reasons := make([]string, 0, len(l.skipped))
	for r := range l.skipped {
		reasons = append(reasons, r)
	}
	sort.Strings(reasons)
	for _, r := range reasons {
		if l.skipped[r] <= l.told[r] {
			continue
		}
		l.told[r] = l.skipped[r]
		_ = l.enc.Encode(Decision{
			Time: now.UTC(), Reason: ReasonSuppressed, Count: l.skipped[r],
			Detail: fmt.Sprintf("%d refused requests (%s) were counted but not itemised (only the first %d of that reason are)", l.skipped[r], r, l.denied[r]),
		})
	}
}
