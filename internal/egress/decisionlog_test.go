package egress

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
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
