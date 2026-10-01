package supervisor_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/supervisor"
)

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func anchorSeq(t *testing.T, path string) int64 {
	t.Helper()
	raw, err := os.ReadFile(path + ".head")
	if err != nil {
		return 0
	}
	var a struct{ Seq int64 }
	if err := json.Unmarshal(raw, &a); err != nil {
		return -1
	}
	return a.Seq
}

func TestTheHeadAnchorIsFlushedBeforeTheRenameAndItsDirectoryAfter(t *testing.T) {
	l, path := openLog(t)
	record(t, l, "one")
	type call struct {
		path       string
		anchorSeq  int64 // what the anchor file held when the flush was asked for
		tmpPresent bool
	}
	var calls []call
	l.AnchorSyncFn = func(p string) error {
		_, err := os.Stat(path + ".head.tmp")
		calls = append(calls, call{p, anchorSeq(t, path), err == nil})
		return nil
	}
	record(t, l, "two")
	if len(calls) != 2 {
		t.Fatalf("%d flushes of the anchor, want 2 (the file, then its directory): %+v", len(calls), calls)
	}
	// 1: the new anchor is on disk under its temporary name, and the old one is still in place.
	if calls[0].path != path+".head.tmp" || !calls[0].tmpPresent || calls[0].anchorSeq != 1 {
		t.Errorf("first flush = %+v: the temporary file must be flushed before it replaces the anchor", calls[0])
	}
	// 2: the rename has happened; now the directory entry is made durable.
	if calls[1].path != filepath.Dir(path) || calls[1].tmpPresent || calls[1].anchorSeq != 2 {
		t.Errorf("second flush = %+v: the directory must be flushed after the rename", calls[1])
	}

	// A flush that fails is an audit failure (BR-E079), and the log still verifies: the anchor is
	// merely behind the log, which the check allows.
	l.AnchorSyncFn = func(string) error { return errors.New("fsync: input/output error") }
	if err := l.Record(supervisor.Entry{Kind: supervisor.KindStartup, Message: "three"}); diag.CodeOf(err) != diag.CodeAuditFailed {
		t.Fatalf("a failed anchor flush: %v, want BR-E079", err)
	}
	if n, err := supervisor.VerifyAuditLog(path); err != nil || n != 3 {
		t.Errorf("verify after a failed anchor flush: %d, %v", n, err)
	}
}

func TestAnEmptyHeadAnchorAfterAPowerCutIsRecoveredWithARecordNotRefusedForEver(t *testing.T) {
	l, path := openLog(t)
	for _, m := range []string{"one", "two", "three"} {
		record(t, l, m)
	}
	l.Close()
	// What a power cut leaves when the anchor's rename reached the disk before its data.
	if err := os.WriteFile(path+".head", nil, 0o600); err != nil {
		t.Fatal(err)
	}

	// Verification names it and does not call the log damaged.
	n, err := supervisor.VerifyAuditLog(path)
	if !errors.Is(err, supervisor.ErrEmptyAnchor) || n != 3 {
		t.Fatalf("verify = %d, %v: want the three good entries and ErrEmptyAnchor", n, err)
	}

	// Reopening is the recovery, and it is on the record.
	l2, err := supervisor.OpenAuditLog(path, nil)
	if err != nil {
		t.Fatalf("an empty anchor over a log that verifies must not be a permanent refusal: %v", err)
	}
	record(t, l2, "after the power cut")
	l2.Close()
	es, err := supervisor.ReadAuditLog(path)
	if err != nil || len(es) != 5 {
		t.Fatalf("%d entries, %v", len(es), err)
	}
	if es[3].Kind != supervisor.KindRecovered || !strings.Contains(es[3].Message, "anchor was empty") || es[4].Message != "after the power cut" {
		t.Errorf("entries = %+v", es[3:])
	}
	if n, err := supervisor.VerifyAuditLog(path); err != nil || n != 5 {
		t.Errorf("after recovery: %d, %v", n, err)
	}
	if anchorSeq(t, path) != 5 {
		t.Errorf("the anchor was not rewritten: %d", anchorSeq(t, path))
	}
}

func TestAnEmptyHeadAnchorDoesNotExcuseALogThatDoesNotVerify(t *testing.T) {
	l, path := openLog(t)
	for _, m := range []string{"one", "two", "three"} {
		record(t, l, m)
	}
	l.Close()
	raw, _ := os.ReadFile(path)
	if err := os.WriteFile(path, []byte(strings.Replace(string(raw), `"two"`, `"TWO"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".head", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.OpenAuditLog(path, nil); err == nil {
		t.Error("an edited log was accepted because its anchor was emptied")
	}
	// And an anchor that holds something that is not an anchor is still damage, not a crash.
	if err := os.WriteFile(path+".head", []byte("not an anchor"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.VerifyAuditLog(path); err == nil || errors.Is(err, supervisor.ErrEmptyAnchor) {
		t.Errorf("a garbage anchor: %v", err)
	}
}

func TestACrashWhileWritingTheRecoveryRecordLeavesTwoTornLinesAndASecondRecoveryAccountsForBoth(t *testing.T) {
	l, path := openLog(t)
	record(t, l, "one")
	record(t, l, "two")
	l.Close()
	first := `{"seq":3,"time":"2026-10`
	second := `{"seq":3,"time":"2026-10-01T00:00:00Z","prev_hash":"ab","kind":"recovered","mess`
	// The first crash tore entry 3; reopening began to write the recovery record and the machine
	// died again, half way through it.
	appendRaw(t, path, first+"\n"+second)

	n, err := supervisor.VerifyAuditLog(path)
	if !errors.Is(err, supervisor.ErrTornTail) || n != 2 {
		t.Fatalf("verify = %d, %v: want the two good entries and ErrTornTail", n, err)
	}
	l2, err := supervisor.OpenAuditLog(path, nil)
	if err != nil {
		t.Fatalf("two torn lines in a row must be recoverable: %v", err)
	}
	record(t, l2, "after the second crash")
	l2.Close()

	if n, err := supervisor.VerifyAuditLog(path); err != nil || n != 4 {
		t.Fatalf("after recovery: %d entries, %v", n, err)
	}
	es, _ := supervisor.ReadAuditLog(path)
	rec := es[2]
	if rec.Kind != supervisor.KindRecovered || len(rec.Fragments) != 2 || rec.Fragments[0] != sha(first) || rec.Fragments[1] != sha(second) {
		t.Errorf("recovery record = %+v: it must name both torn lines, in order, by hash", rec)
	}
	// Naming only one of them is not enough.
	raw, _ := os.ReadFile(path)
	bad := strings.Replace(string(raw), sha(first), sha("something else"), 1)
	if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.VerifyAuditLog(path); err == nil {
		t.Error("a recovery record that names the wrong fragment was accepted")
	}
}

func TestThreeTornLinesInARowAreDamageNotACrash(t *testing.T) {
	l, path := openLog(t)
	record(t, l, "one")
	l.Close()
	appendRaw(t, path, "x1\nx2\nx3")
	if _, err := supervisor.OpenAuditLog(path, nil); err == nil {
		t.Error("three unparseable lines cannot be two crashes")
	}
}
