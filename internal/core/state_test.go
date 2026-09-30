package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modullar/blade-runner/internal/diag"
)

func TestStateRoundTripAndPermissions(t *testing.T) {
	dir := t.TempDir()
	s := &StateStore{Path: filepath.Join(dir, "sub", "state.json"), Now: func() time.Time { return time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC) }}

	st, err := s.Load()
	if err != nil || st.RunnerName != "" {
		t.Fatalf("missing file should load as empty state, got %+v, %v", st, err)
	}
	if err := s.Update(func(st *State) { st.RunnerName = "mini"; st.RunnerVersion = "2.999.0" }); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(st *State) { st.Target = "o/r" }); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.RunnerName != "mini" || got.RunnerVersion != "2.999.0" || got.Target != "o/r" || got.SchemaVersion != 1 {
		t.Errorf("state = %+v (updates must merge)", got)
	}
	if !got.UpdatedAt.Equal(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("UpdatedAt = %v", got.UpdatedAt)
	}
	info, err := os.Stat(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("state file mode = %v, want 0600", info.Mode().Perm())
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(s.Path), ".state-*.tmp"))
	if len(leftovers) != 0 {
		t.Errorf("temp files left behind: %v", leftovers)
	}
}

func TestStateCorruptIsReportedWithAFix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &StateStore{Path: path}
	_, err := s.Load()
	if diag.CodeOf(err) != diag.CodeStateCorrupt || !strings.Contains(err.Error(), "rm ") {
		t.Errorf("err = %v, want BR-E080 with a delete hint", err)
	}
	if err := s.Update(func(*State) {}); diag.CodeOf(err) != diag.CodeStateCorrupt {
		t.Errorf("Update over a corrupt file must fail, not overwrite it: %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != "{not json" {
		t.Error("a corrupt state file must be left for the user to inspect")
	}
}

func TestStateFromNewerSchemaIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"schema_version": 99}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (&StateStore{Path: path}).Load(); diag.CodeOf(err) != diag.CodeStateCorrupt {
		t.Errorf("err = %v, want BR-E080", err)
	}
}
