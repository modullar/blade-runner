package egress

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func readLog(t *testing.T, b *bytes.Buffer) []Decision {
	t.Helper()
	var out []Decision
	sc := bufio.NewScanner(bytes.NewReader(b.Bytes()))
	for sc.Scan() {
		var d Decision
		if err := json.Unmarshal(sc.Bytes(), &d); err != nil {
			t.Fatalf("not a decision line %q: %v", sc.Text(), err)
		}
		out = append(out, d)
	}
	return out
}

func deniedFor(i int) Decision {
	return Decision{Client: "172.19.0.2:40000", Host: fmt.Sprintf("flood%d.test", i), Port: "443", Reason: ReasonNotAllowlist}
}

func TestADenialFloodCannotCrowdOutTheAllowedRecords(t *testing.T) {
	var buf bytes.Buffer
	l := NewDecisionLog(&buf)
	l.MaxDenied = 50
	allowed := Decision{Client: "172.19.0.2:1", Host: "github.com", Port: "443", Allowed: true, Reason: ReasonAllowed}

	l.Record(allowed)
	for i := 0; i < 100000; i++ {
		l.Record(deniedFor(i))
	}
	l.Record(allowed)
	l.Flush()

	// 100,000 refusals at ~120 bytes a line would be 12 MB. The log is a small fraction of that.
	if buf.Len() > 64<<10 {
		t.Errorf("the log grew to %d bytes under a denial flood", buf.Len())
	}
	ds := readLog(t, &buf)
	var nAllowed, nDenied, nSummary int
	var lastSummary Decision
	for _, d := range ds {
		switch {
		case d.Allowed:
			nAllowed++
		case d.Reason == ReasonSuppressed:
			nSummary++
			lastSummary = d
		default:
			nDenied++
		}
	}
	if nAllowed != 2 {
		t.Errorf("allowed records = %d, want both", nAllowed)
	}
	if nDenied != 50 {
		t.Errorf("itemised denials = %d, want exactly the cap, 50", nDenied)
	}
	if nSummary == 0 || !strings.Contains(lastSummary.Detail, "99950 refused requests") {
		t.Errorf("summaries = %d, last %q: the count of what was not itemised must be reported", nSummary, lastSummary.Detail)
	}
}

func TestSummariesAreRateLimitedAndFlushedAtTheEnd(t *testing.T) {
	var buf bytes.Buffer
	l := NewDecisionLog(&buf)
	l.MaxDenied = 1
	l.SummaryEvery = time.Hour // so only the first overflow writes a summary
	for i := 0; i < 10; i++ {
		l.Record(deniedFor(i))
	}
	summaries := func() (n int, last Decision) {
		for _, d := range readLog(t, &buf) {
			if d.Reason == ReasonSuppressed {
				n++
				last = d
			}
		}
		return
	}
	if n, _ := summaries(); n != 1 {
		t.Errorf("summaries during the flood = %d, want 1", n)
	}
	l.Flush()
	n, last := summaries()
	if n != 2 || !strings.Contains(last.Detail, "9 refused requests") {
		t.Errorf("after Flush: %d summaries, last %q; want a final one reporting 9", n, last.Detail)
	}
	l.Flush() // nothing new: no extra line
	if n2, _ := summaries(); n2 != 2 {
		t.Errorf("a second Flush wrote another summary")
	}
}

func TestNoSummaryWhenNothingWasSuppressed(t *testing.T) {
	var buf bytes.Buffer
	l := NewDecisionLog(&buf)
	l.Record(deniedFor(1))
	l.Flush()
	if ds := readLog(t, &buf); len(ds) != 1 || ds[0].Reason != ReasonNotAllowlist {
		t.Errorf("log = %+v", ds)
	}
}

// ---- budgets per reason, aggregation of allowed decisions, staleness ---------------------

// clockLog is a DecisionLog on a clock the test moves.
func clockLog(buf *bytes.Buffer, max int) (*DecisionLog, *time.Time) {
	l := NewDecisionLog(buf)
	l.MaxDenied = max
	l.SummaryEvery = 10 * time.Second
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }
	return l, &now
}

func count(ds []Decision, pred func(Decision) bool) (n int) {
	for _, d := range ds {
		if pred(d) {
			n++
		}
	}
	return
}

func TestEachRefusalReasonHasItsOwnItemisingBudget(t *testing.T) {
	var buf bytes.Buffer
	l, _ := clockLog(&buf, 5)
	for i := 0; i < 500; i++ { // one noisy reason...
		l.Record(deniedFor(i))
	}
	for i := 0; i < 3; i++ { // ...must not use up the budget of another
		l.Record(Decision{Host: "x.test", Port: "22", Reason: ReasonBadPort})
	}
	ds := readLog(t, &buf)
	if n := count(ds, func(d Decision) bool { return d.Reason == ReasonBadPort }); n != 3 {
		t.Errorf("bad-port records = %d, want all 3", n)
	}
	if n := count(ds, func(d Decision) bool { return d.Reason == ReasonNotAllowlist }); n != 5 {
		t.Errorf("not-allowlisted records = %d, want its budget, 5", n)
	}
}

func TestForbiddenAddressAndIPLiteralRefusalsAreNeverSuppressed(t *testing.T) {
	var buf bytes.Buffer
	l, _ := clockLog(&buf, 3)
	for i := 0; i < 400; i++ {
		l.Record(Decision{Host: fmt.Sprintf("h%d.test", i), Port: "443", Reason: ReasonForbiddenAddr, Resolved: []string{"10.0.0.1"}})
		l.Record(Decision{Host: "10.0.0.1", Port: "443", Reason: ReasonIPLiteral})
	}
	l.Flush()
	ds := readLog(t, &buf)
	for _, reason := range []string{ReasonForbiddenAddr, ReasonIPLiteral} {
		if n := count(ds, func(d Decision) bool { return d.Reason == reason }); n != 400 {
			t.Errorf("%s records = %d, want all 400 itemised despite a budget of 3", reason, n)
		}
	}
	if n := count(ds, func(d Decision) bool { return d.Reason == ReasonSuppressed }); n != 0 {
		t.Errorf("%d summary lines: nothing here was suppressible", n)
	}
}

func TestABurstOfAllowedDecisionsIsAggregatedKeepingFirstAndLast(t *testing.T) {
	var buf bytes.Buffer
	l, now := clockLog(&buf, 5)
	for i := 0; i < 20000; i++ {
		*now = now.Add(time.Millisecond) // 20s: two windows
		l.Record(Decision{Time: *now, Client: fmt.Sprintf("172.19.0.2:%d", 40000+i%1000), Host: "github.com", Port: "443", Allowed: true, Reason: ReasonAllowed})
	}
	l.Flush()
	ds := readLog(t, &buf)
	if buf.Len() > 8<<10 || len(ds) > 8 {
		t.Fatalf("20000 allowed decisions wrote %d lines / %d bytes: the log can be rotated by a burst", len(ds), buf.Len())
	}
	total := 0
	for _, d := range ds {
		if !d.Allowed || d.Host != "github.com" && d.Host != "" {
			t.Errorf("unexpected record %+v", d)
		}
		if d.Count == 0 {
			total++ // an itemised first
		} else {
			total += d.Count
		}
	}
	if total != 20000 {
		t.Errorf("records stand for %d decisions, want 20000: aggregation must not lose count", total)
	}
	last := ds[len(ds)-1]
	if last.Count == 0 || last.Client != "172.19.0.2:"+fmt.Sprint(40000+19999%1000) {
		t.Errorf("the last record %+v must carry the LAST decision's client", last)
	}
	if ds[0].Count != 0 || ds[0].Client != "172.19.0.2:40000" {
		t.Errorf("the first record %+v must be the first decision itself", ds[0])
	}
}

func TestAllowedDecisionsToDistinctHostsAreBoundedPerWindow(t *testing.T) {
	var buf bytes.Buffer
	l, _ := clockLog(&buf, 5)
	for i := 0; i < 5000; i++ { // a wildcard entry lets a job invent names
		l.Record(Decision{Host: fmt.Sprintf("n%d.example.test", i), Port: "443", Allowed: true, Reason: ReasonAllowed})
	}
	l.Flush()
	ds := readLog(t, &buf)
	if len(ds) > maxAllowedKeys+2 {
		t.Errorf("%d lines for 5000 distinct allowed hosts, want at most %d", len(ds), maxAllowedKeys+2)
	}
	total := 0
	for _, d := range ds {
		if d.Count == 0 {
			total++
		} else {
			total += d.Count
		}
	}
	if total != 5000 {
		t.Errorf("records stand for %d decisions, want 5000", total)
	}
}

func TestTickFlushesAStaleSummaryAndAggregateWithoutAnotherRequest(t *testing.T) {
	var buf bytes.Buffer
	l, now := clockLog(&buf, 1)
	l.Record(Decision{Host: "github.com", Port: "443", Allowed: true, Reason: ReasonAllowed})
	l.Record(Decision{Host: "github.com", Port: "443", Allowed: true, Reason: ReasonAllowed})
	for i := 0; i < 50; i++ {
		l.Record(deniedFor(i))
	}
	// The job went quiet. Only 1 refusal was summarised so far, and the repeat is unwritten.
	*now = now.Add(11 * time.Second)
	l.Tick()
	ds := readLog(t, &buf)
	var last Decision
	for _, d := range ds {
		if d.Reason == ReasonSuppressed {
			last = d
		}
	}
	if last.Count != 49 || !strings.Contains(last.Detail, "49 refused requests") {
		t.Errorf("after a quiet interval the newest summary is %+v, want the full count 49", last)
	}
	if n := count(ds, func(d Decision) bool { return d.Allowed && d.Count == 1 }); n != 1 {
		t.Errorf("the repeated allowed decision was not aggregated by Tick: %+v", ds)
	}
}

func TestRunFlushesOnATimerWithNoFurtherRequests(t *testing.T) {
	var mu sync.Mutex
	var buf bytes.Buffer
	l := NewDecisionLog(lockedWriter{&mu, &buf})
	l.MaxDenied = 1
	l.SummaryEvery = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Run(ctx)
	for i := 0; i < 30; i++ {
		l.Record(deniedFor(i))
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		ds := readLog(t, &buf)
		mu.Unlock()
		for _, d := range ds {
			if d.Reason == ReasonSuppressed && d.Count == 29 {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the timer never wrote the final summary")
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func TestAckWritesAMarkerWhoseCountRisesAndFlushesFirst(t *testing.T) {
	var buf bytes.Buffer
	l, _ := clockLog(&buf, 1)
	for i := 0; i < 5; i++ {
		l.Record(deniedFor(i))
	}
	l.Ack()
	l.Ack()
	ds := readLog(t, &buf)
	if got := flushMarker(ds); got != 2 {
		t.Errorf("flush marker = %d, want 2", got)
	}
	var sawSummaryBeforeMarker bool
	for _, d := range ds {
		if d.Reason == ReasonSuppressed && d.Count == 4 {
			sawSummaryBeforeMarker = true
		}
		if d.Reason == ReasonFlushed && !sawSummaryBeforeMarker {
			t.Error("the marker was written before the lines it vouches for")
		}
	}
}

func TestResolvedAddressesAreClippedInCountAndBytes(t *testing.T) {
	var got Decision
	p := &Proxy{Observe: func(d Decision) { got = d }}
	var many []string
	for i := 0; i < 500; i++ {
		many = append(many, fmt.Sprintf("2001:db8:aaaa:bbbb:cccc:dddd:eeee:%04x", i))
	}
	p.observe(Decision{Reason: ReasonForbiddenAddr, Resolved: many})
	size := 0
	for _, r := range got.Resolved {
		size += len(r)
	}
	if len(got.Resolved) > maxLoggedResolved+1 || size > maxLoggedResolvedBytes+16 {
		t.Errorf("resolved = %d entries / %d bytes, want it capped", len(got.Resolved), size)
	}
	if last := got.Resolved[len(got.Resolved)-1]; !strings.HasPrefix(last, "+") {
		t.Errorf("the cut must be marked, last entry %q", last)
	}
	p.observe(Decision{Reason: ReasonForbiddenAddr, Resolved: []string{"1.2.3.4"}})
	if len(got.Resolved) != 1 || got.Resolved[0] != "1.2.3.4" {
		t.Errorf("a short list must pass unchanged: %v", got.Resolved)
	}
}

func TestParseDecisionLinesSkipsOverlongAndBrokenLinesAndKeepsReading(t *testing.T) {
	good := func(host string) string {
		b, _ := json.Marshal(Decision{Host: host, Reason: ReasonAllowed, Allowed: true})
		return string(b) + "\n"
	}
	text := `ng"} cut by log rotation` + "\n" + good("a.test") +
		strings.Repeat("z", 3<<20) + "\n" + // far past any buffer
		good("b.test") + "not json\n" + `{"host":"no-reason"}` + "\n" + good("c.test") + strings.TrimSuffix(good("d.test"), "\n") // no final newline
	var hosts []string
	for _, d := range parseDecisionLines(text) {
		hosts = append(hosts, d.Host)
	}
	if got := strings.Join(hosts, ","); got != "a.test,b.test,c.test,d.test" {
		t.Errorf("parsed hosts %q, want a.test,b.test,c.test,d.test", got)
	}
}
