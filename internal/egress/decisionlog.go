package egress

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

// DefaultMaxLoggedDenials is how many refused requests a proxy itemises before it only counts.
const DefaultMaxLoggedDenials = 1000

// defaultSummaryEvery is the least time between two summary lines.
const defaultSummaryEvery = 10 * time.Second

// DecisionLog writes the proxy's decisions as JSON lines and keeps a flood of refusals from
// pushing the records that matter out of the log.
//
// The log is read back from Docker's size-capped, rotating log (see Session.Decisions), so what
// is written competes for a few hundred KiB. A job that hammers the proxy with forbidden
// requests would otherwise evict the record of what it was allowed to do. So: every ALLOWED
// decision is written; refused ones are itemised only up to a cap (the first, most telling
// ones), after which they are counted and reported in a periodic summary line
// (ReasonSuppressed) whose size does not depend on the number of requests.
//
// Allowed decisions are not capped: each one is a real tunnel, already bounded by the proxy's
// connection cap and the allowlist, and they are the audit trail.
type DecisionLog struct {
	MaxDenied    int           // itemised refusals; default DefaultMaxLoggedDenials
	SummaryEvery time.Duration // at most one summary line per this long; default 10s

	mu       sync.Mutex
	enc      *json.Encoder
	now      func() time.Time
	denied   int // refusals itemised so far
	skipped  int // refusals counted but not itemised
	reported int // of skipped, how many the last summary included
	lastSum  time.Time
}

// NewDecisionLog logs to w.
func NewDecisionLog(w io.Writer) *DecisionLog {
	return &DecisionLog{enc: json.NewEncoder(w), now: time.Now}
}

// Record writes d, or counts it. It is safe for concurrent use.
func (l *DecisionLog) Record(d Decision) {
	l.mu.Lock()
	defer l.mu.Unlock()
	max := l.MaxDenied
	if max <= 0 {
		max = DefaultMaxLoggedDenials
	}
	switch {
	case d.Allowed:
		_ = l.enc.Encode(d)
	case l.denied < max:
		l.denied++
		_ = l.enc.Encode(d)
	default:
		l.skipped++
		every := l.SummaryEvery
		if every <= 0 {
			every = defaultSummaryEvery
		}
		if l.now().Sub(l.lastSum) >= every {
			l.summarise()
		}
	}
}

// Flush writes a final summary if refusals were counted since the last one.
func (l *DecisionLog) Flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.skipped > l.reported {
		l.summarise()
	}
}

func (l *DecisionLog) summarise() {
	now := l.now()
	l.lastSum, l.reported = now, l.skipped
	_ = l.enc.Encode(Decision{
		Time: now.UTC(), Reason: ReasonSuppressed,
		Detail: fmt.Sprintf("%d refused requests were counted but not itemised (only the first %d are)", l.skipped, l.denied),
	})
}
