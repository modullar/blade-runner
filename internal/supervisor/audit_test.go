package supervisor_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/supervisor"
)

func openLog(t *testing.T) (*supervisor.AuditLog, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit", "supervisor.jsonl")
	l, err := supervisor.OpenAuditLog(path, func() time.Time { return time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC) })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l, path
}

func TestAuditLogIsPrivateAppendOnlyJSONLines(t *testing.T) {
	l, path := openLog(t)
	for i := 0; i < 3; i++ {
		if err := l.Record(supervisor.Entry{Kind: supervisor.KindRefused, Code: diag.CodeJobRefused, Message: "m", RunID: int64(i + 1)}); err != nil {
			t.Fatal(err)
		}
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("file: %v %v, want 0600", info, err)
	}
	if info, err := os.Stat(filepath.Dir(path)); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("directory: %v %v, want 0700", info, err)
	}
	raw, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 {
		t.Fatalf("%d lines", len(lines))
	}
	for i, ln := range lines {
		var e supervisor.Entry
		if err := json.Unmarshal([]byte(ln), &e); err != nil {
			t.Fatalf("line %d is not JSON: %v", i, err)
		}
		if e.Seq != int64(i+1) || e.Time != "2026-10-01T12:00:00Z" || e.RunID != int64(i+1) {
			t.Errorf("entry %d = %+v", i, e)
		}
	}
	if n, err := supervisor.VerifyAuditLog(path); err != nil || n != 3 {
		t.Errorf("verify: %d, %v", n, err)
	}
}

func TestAuditLogChainDetectsEditsAndDeletions(t *testing.T) {
	l, path := openLog(t)
	for i := 0; i < 4; i++ {
		if err := l.Record(supervisor.Entry{Kind: supervisor.KindRefused, Message: "refused run " + string(rune('A'+i))}); err != nil {
			t.Fatal(err)
		}
	}
	good, _ := os.ReadFile(path)
	lines := bytes.Split(bytes.TrimSpace(good), []byte("\n"))

	edited := bytes.Join([][]byte{lines[0], bytes.Replace(lines[1], []byte("run B"), []byte("run X"), 1), lines[2], lines[3]}, []byte("\n"))
	deleted := bytes.Join([][]byte{lines[0], lines[2], lines[3]}, []byte("\n"))
	truncatedHead := bytes.Join([][]byte{lines[1], lines[2], lines[3]}, []byte("\n"))
	for name, content := range map[string][]byte{"an edited entry": edited, "a deleted entry": deleted, "a deleted first entry": truncatedHead} {
		p := filepath.Join(t.TempDir(), "x.jsonl")
		if err := os.WriteFile(p, append(content, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := supervisor.VerifyAuditLog(p); err == nil {
			t.Errorf("%s was not detected", name)
		}
	}
	if _, err := supervisor.VerifyAuditLog(path); err != nil {
		t.Errorf("the untouched log fails verification: %v", err)
	}
}

func TestAuditLogContinuesAcrossReopenAndSurvivesAHalfWrittenLine(t *testing.T) {
	l, path := openLog(t)
	if err := l.Record(supervisor.Entry{Kind: supervisor.KindStartup, Message: "one"}); err != nil {
		t.Fatal(err)
	}
	l.Close()

	// A crash in the middle of a write leaves half a line with no newline.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(`{"seq":2,"time":"2026-10`)
	f.Close()

	l2, err := supervisor.OpenAuditLog(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if err := l2.Record(supervisor.Entry{Kind: supervisor.KindStartup, Message: "after the crash"}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %q", lines)
	}
	var last supervisor.Entry
	if err := json.Unmarshal([]byte(lines[2]), &last); err != nil || last.Message != "after the crash" || last.Seq != 3 {
		t.Errorf("the entry after the crash is not on its own line: %q (%v)", lines[2], err)
	}
	var first supervisor.Entry
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil || first.Message != "one" {
		t.Errorf("earlier entries were damaged: %q", lines[0])
	}
	// The damage is visible, not hidden.
	if _, err := supervisor.VerifyAuditLog(path); err == nil {
		t.Error("a half-written line must make verification fail loudly")
	}
}

func TestAuditLogIsSafeForConcurrentWriters(t *testing.T) {
	l, path := openLog(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.Record(supervisor.Entry{Kind: supervisor.KindError, Message: "x"}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n, err := supervisor.VerifyAuditLog(path); err != nil || n != 20 {
		t.Errorf("verify: %d, %v", n, err)
	}
}

func TestAuditLogFailuresAreBR_E079(t *testing.T) {
	l, _ := openLog(t)
	l.Close()
	if err := l.Record(supervisor.Entry{Kind: supervisor.KindError}); diag.CodeOf(err) != diag.CodeAuditFailed {
		t.Errorf("writing to a closed log: %v", err)
	}
	// A directory that cannot be created (a file is in the way).
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.OpenAuditLog(filepath.Join(blocker, "audit", "a.jsonl"), nil); diag.CodeOf(err) != diag.CodeAuditFailed {
		t.Errorf("opening under a file: %v", err)
	}
}
