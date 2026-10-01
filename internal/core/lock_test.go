//go:build unix

package core

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/modullar/blade-runner/internal/diag"
)

func TestLockDirIsExclusiveAndReleasable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "home")
	unlock, err := LockDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LockDir(dir); diag.CodeOf(err) != diag.CodeBusy {
		t.Fatalf("second lock: %v, want BR-E004", err)
	}
	unlock()
	unlock2, err := LockDir(dir)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	unlock2()
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("the lock left files behind: %v", entries)
	}
}

func TestLockSurvivesTheDirectoryBeingRemoved(t *testing.T) {
	// remove deletes the home directory while holding the lock on it.
	dir := filepath.Join(t.TempDir(), "home")
	unlock, err := LockDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := os.Remove(dir); err != nil {
		t.Fatalf("removing a locked empty directory: %v", err)
	}
}
