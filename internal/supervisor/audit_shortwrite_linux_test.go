//go:build linux

package supervisor_test

import (
	"errors"
	"os"
	"os/signal"
	"syscall"
	"testing"

	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/supervisor"
)

// limitFileSize makes the kernel refuse to grow any file of this process past n bytes
// (RLIMIT_FSIZE): a write that crosses it is a SHORT write, part of the line reaches the disk and
// the call returns an error, which is how a full disk looks too. The returned func lifts it.
// Tests in this package run one at a time, so the process-wide limit is not seen by another test.
func limitFileSize(t *testing.T, n uint64) (lift func()) {
	t.Helper()
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		t.Skip("cannot read RLIMIT_FSIZE:", err)
	}
	signal.Ignore(syscall.SIGXFSZ) // otherwise crossing the limit kills the test binary
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: n, Max: old.Max}); err != nil {
		signal.Reset(syscall.SIGXFSZ)
		t.Skip("cannot set RLIMIT_FSIZE:", err)
	}
	lifted := false
	lift = func() {
		if !lifted {
			lifted = true
			_ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old)
			signal.Reset(syscall.SIGXFSZ)
		}
	}
	t.Cleanup(lift)
	return lift
}

func fileSize(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return uint64(fi.Size())
}

func TestAShortWritePoisonsTheLogSoNothingIsAppendedAfterTheFragment(t *testing.T) {
	l, path := openLog(t)
	record(t, l, "one")
	lift := limitFileSize(t, fileSize(t, path)+25) // room for a piece of the next line only
	err := l.Record(supervisor.Entry{Kind: supervisor.KindStartup, Message: "two, which does not fit"})
	lift()
	if diag.CodeOf(err) != diag.CodeAuditFailed {
		t.Fatalf("a short write: %v, want BR-E079", err)
	}
	if got := fileSize(t, path); got < 1 {
		t.Fatal("the test did not produce a partial line")
	}
	// The disk is fine again. The writer cannot know what is in the file, so it must refuse: a
	// line appended now would be glued to the fragment and the chain would be forked or garbled.
	if err := l.Record(supervisor.Entry{Kind: supervisor.KindStartup, Message: "three"}); diag.CodeOf(err) != diag.CodeAuditFailed {
		t.Fatalf("a write after a short write: %v, want BR-E079 (fail closed)", err)
	}
	n, err := supervisor.VerifyAuditLog(path)
	if !errors.Is(err, supervisor.ErrTornTail) || n != 1 {
		t.Fatalf("verify = %d, %v: want the one good entry and the fragment named as a torn tail", n, err)
	}
	// A reopen is the recovery.
	l.Close()
	l2, err := supervisor.OpenAuditLog(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	record(t, l2, "after the reopen")
	if n, err := supervisor.VerifyAuditLog(path); err != nil || n != 3 {
		t.Errorf("after recovery: %d, %v", n, err)
	}
}

func TestACrashDuringTheRecoveryRecordIsItselfRecoveredByTheNextOpen(t *testing.T) {
	// The real thing, not a hand-made file: a torn line, then a reopen whose recovery record is
	// itself cut short by the disk, then a reopen that finds two torn lines.
	l, path := openLog(t)
	record(t, l, "one")
	l.Close()
	appendRaw(t, path, `{"seq":2,"time":"2026-10`)

	lift := limitFileSize(t, fileSize(t, path)+40)
	_, err := supervisor.OpenAuditLog(path, nil)
	lift()
	if diag.CodeOf(err) != diag.CodeAuditFailed {
		t.Fatalf("a recovery record that does not fit: %v, want BR-E079", err)
	}
	if _, err := supervisor.VerifyAuditLog(path); !errors.Is(err, supervisor.ErrTornTail) {
		t.Fatalf("verify: %v, want a torn tail", err)
	}
	l2, err := supervisor.OpenAuditLog(path, nil)
	if err != nil {
		t.Fatalf("the second reopen: %v", err)
	}
	record(t, l2, "alive again")
	l2.Close()
	n, err := supervisor.VerifyAuditLog(path)
	if err != nil || n != 3 {
		t.Fatalf("after the second recovery: %d, %v", n, err)
	}
	es, _ := supervisor.ReadAuditLog(path)
	if es[1].Kind != supervisor.KindRecovered || len(es[1].Fragments) != 2 {
		t.Errorf("recovery record = %+v: it must account for both torn lines", es[1])
	}
}
