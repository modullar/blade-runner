package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/modullar/blade-runner/internal/diag"
)

const stateSchema = 1

// StepRecord is the last outcome recorded for a step. It is a progress log for humans and
// for `status`; the truth about whether a step is done is always re-read from the machine.
type StepRecord struct {
	Status string    `json:"status"` // "done" or "failed"
	At     time.Time `json:"at"`
	Error  string    `json:"error,omitempty"`
}

// State is the local record of what Blade Runner installed. It holds no secrets and is
// safe to delete: apply re-derives everything from the machine.
type State struct {
	SchemaVersion int                   `json:"schema_version"`
	RunnerName    string                `json:"runner_name"`
	Target        string                `json:"target"`
	RunnerVersion string                `json:"runner_version,omitempty"` // the pinned runner release
	RunnerSHA256  string                `json:"runner_sha256,omitempty"`
	Steps         map[string]StepRecord `json:"steps,omitempty"`
	UpdatedAt     time.Time             `json:"updated_at"`
}

// StateStore persists State in one JSON file, written atomically.
type StateStore struct {
	Path string
	Now  func() time.Time
}

// Load returns the stored state, or a zero State if there is none yet.
func (s *StateStore) Load() (State, error) {
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return State{SchemaVersion: stateSchema}, nil
	}
	if err != nil {
		return State{}, diag.Wrap(err, diag.CodeStateCorrupt, fmt.Sprintf("cannot read state file %s", s.Path),
			"the file is unreadable", "check its permissions, or delete it: apply rebuilds it from the machine")
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return State{}, diag.Wrap(err, diag.CodeStateCorrupt, fmt.Sprintf("state file %s is not valid JSON", s.Path),
			"it was edited by hand or cut off by a crash",
			fmt.Sprintf("delete it (rm %q): apply rebuilds it from the machine", s.Path))
	}
	if st.SchemaVersion > stateSchema {
		return State{}, diag.New(diag.CodeStateCorrupt, fmt.Sprintf("state file %s is from a newer bladerunner (schema %d)", s.Path, st.SchemaVersion),
			"this CLI is older than the one that wrote it", "upgrade bladerunner")
	}
	return st, nil
}

// Update loads the state, applies fn and writes it back atomically (temp file + rename),
// so a crash leaves either the old file or the new one.
func (s *StateStore) Update(fn func(*State)) error {
	st, err := s.Load()
	if err != nil {
		return err
	}
	fn(&st)
	st.SchemaVersion = stateSchema
	if s.Now != nil {
		st.UpdatedAt = s.Now().UTC()
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.Path)
}
