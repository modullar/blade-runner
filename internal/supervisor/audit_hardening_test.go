package supervisor_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/supervisor"
)

func record(t *testing.T, l *supervisor.AuditLog, msg string) {
	t.Helper()
	if err := l.Record(supervisor.Entry{Kind: supervisor.KindStartup, Message: msg}); err != nil {
		t.Fatal(err)
	}
}

func appendRaw(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func TestAFailedFlushAfterAWrittenLineNeverForksTheChain(t *testing.T) {
	l, path := openLog(t)
	record(t, l, "one")
	l.SyncFn = func() error { return errors.New("fsync: input/output error") }
	if err := l.Record(supervisor.Entry{Kind: supervisor.KindStartup, Message: "two"}); diag.CodeOf(err) != diag.CodeAuditFailed {
		t.Fatalf("a failed flush must be reported as BR-E079, got %v", err)
	}
	l.SyncFn = nil // the disk answers again
	// The second line is on disk. Whatever the writer does next, it must not write a third entry
	// that claims to follow the FIRST one (a fork), and it must not pretend all is well.
	err := l.Record(supervisor.Entry{Kind: supervisor.KindStartup, Message: "three"})
	if n, verr := supervisor.VerifyAuditLog(path); verr != nil || n < 2 {
		t.Fatalf("the chain no longer verifies after a failed flush: %d entries, %v", n, verr)
	}
	if err == nil {
		t.Error("after a flush failed the durability of the log is unknown: later writes must fail closed")
	}
}

func TestAHalfWrittenLastLineIsReportedAndRecoveryIsExplicit(t *testing.T) {
	l, path := openLog(t)
	record(t, l, "one")
	record(t, l, "two")
	l.Close()
	appendRaw(t, path, `{"seq":3,"time":"2026-10`)

	// Before anyone reopens it: the entries verify, and the damage is named, not hidden.
	n, err := supervisor.VerifyAuditLog(path)
	if !errors.Is(err, supervisor.ErrTornTail) || n != 2 {
		t.Fatalf("verify = %d, %v: want the two good entries and ErrTornTail", n, err)
	}

	// Reopening is the recovery: it writes a record that accounts for the fragment by hash.
	l2, err := supervisor.OpenAuditLog(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	record(t, l2, "after the crash")
	if n, err := supervisor.VerifyAuditLog(path); err != nil || n != 4 {
		t.Fatalf("after recovery: %d entries, %v: the log must verify again, with the recovery on record", n, err)
	}
	es, err := supervisor.ReadAuditLog(path)
	if err != nil || len(es) != 4 {
		t.Fatalf("%d entries, %v", len(es), err)
	}
	sum := sha256.Sum256([]byte(`{"seq":3,"time":"2026-10`))
	if es[2].Kind != supervisor.KindRecovered || es[2].Fragment != hex.EncodeToString(sum[:]) || es[2].Seq != 3 || es[3].Message != "after the crash" || es[3].Seq != 4 {
		t.Errorf("entries = %+v", es)
	}
}

func TestAnUnaccountedForGarbageLineStillBreaksVerification(t *testing.T) {
	l, path := openLog(t)
	record(t, l, "one")
	record(t, l, "two")
	l.Close()
	good, _ := os.ReadFile(path)
	lines := bytes.Split(bytes.TrimSpace(good), []byte("\n"))

	garbage := bytes.Join([][]byte{lines[0], []byte("not json at all"), lines[1]}, []byte("\n"))
	p := filepath.Join(t.TempDir(), "g.jsonl")
	if err := os.WriteFile(p, append(garbage, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.VerifyAuditLog(p); err == nil || errors.Is(err, supervisor.ErrTornTail) {
		t.Errorf("a garbage line in the middle with no recovery record: %v", err)
	}
}

func TestARecoveryRecordThatNamesTheWrongFragmentIsRefused(t *testing.T) {
	l, path := openLog(t)
	record(t, l, "one")
	l.Close()
	appendRaw(t, path, `{"seq":2,"tim`)
	l2, err := supervisor.OpenAuditLog(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	l2.Close()
	raw, _ := os.ReadFile(path)
	forged := bytes.Replace(raw, []byte(`{"seq":2,"tim`), []byte(`{"seq":2,"TIM`), 1) // a different fragment than the one recorded
	if err := os.WriteFile(path, forged, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.VerifyAuditLog(path); err == nil {
		t.Error("the recovery record vouches for a fragment that is not the one in the file")
	}
}

func TestTwoWritersCannotShareOneAuditLog(t *testing.T) {
	l, path := openLog(t)
	record(t, l, "one")
	if _, err := supervisor.OpenAuditLog(path, nil); diag.CodeOf(err) != diag.CodeAuditFailed {
		t.Fatalf("a second writer on the same file must be refused, got %v", err)
	}
	l.Close()
	l2, err := supervisor.OpenAuditLog(path, nil)
	if err != nil {
		t.Fatalf("once the first writer is gone the log is free: %v", err)
	}
	l2.Close()
}

func TestTheHeadAnchorCatchesWhatTheChainCannotSee(t *testing.T) {
	l, path := openLog(t)
	for _, m := range []string{"one", "two", "three", "four"} {
		record(t, l, m)
	}
	l.Close()
	good, _ := os.ReadFile(path)
	lines := bytes.Split(bytes.TrimSpace(good), []byte("\n"))

	// The last entry has no successor, so the chain alone cannot see it edited.
	edited := append(bytes.Join([][]byte{lines[0], lines[1], lines[2], bytes.Replace(lines[3], []byte("four"), []byte("FOUR"), 1)}, []byte("\n")), '\n')
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.VerifyAuditLog(path); err == nil {
		t.Error("an edited last entry went unnoticed although the head anchor records its hash")
	}

	// Cutting entries off the end leaves a valid chain, but a shorter one than the anchor says.
	cut := append(bytes.Join(lines[:2], []byte("\n")), '\n')
	if err := os.WriteFile(path, cut, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.VerifyAuditLog(path); err == nil {
		t.Error("a truncated log went unnoticed")
	}
	if _, err := supervisor.OpenAuditLog(path, nil); diag.CodeOf(err) != diag.CodeAuditFailed {
		t.Errorf("the supervisor must not carry on a log that is shorter than its anchor: %v", err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.VerifyAuditLog(path); err == nil {
		t.Error("an emptied log went unnoticed")
	}

	// Restoring the original makes everything verify again.
	if err := os.WriteFile(path, good, 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := supervisor.VerifyAuditLog(path); err != nil || n != 4 {
		t.Errorf("the untouched log: %d, %v", n, err)
	}
}

// What is NOT detected, stated as a test (see the AuditLog comment and docs/decisions/0007): the
// log and its anchor are both just files. Someone who removes or rewrites both leaves a log that
// verifies clean, and an empty log verifies clean as well.
func TestKnownLimit_AnEmptyOrWhollyReplacedLogVerifiesClean(t *testing.T) {
	l, path := openLog(t)
	record(t, l, "one")
	record(t, l, "two")
	l.Close()
	for _, p := range []string{path, path + ".head"} {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := supervisor.VerifyAuditLog(path); err != nil || n != 0 {
		t.Errorf("verify = %d, %v: with both files gone nothing remembers what was there", n, err)
	}
}
