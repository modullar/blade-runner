package egress

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// lockDocker makes a real-Docker test exclusive across test PROCESSES until it finishes.
// `go test ./...` runs packages in parallel, and internal/isolation's RemoveStale test removes
// every container carrying the job label (which these tests' jobs carry too) and asserts none
// remain; without a lock the two packages kill and trip each other. internal/isolation takes the
// same lock (same file) in its own needDocker.
func lockDocker(t *testing.T) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(os.TempDir(), "bladerunner-docker-tests.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("cannot open the docker test lock: %v", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		t.Fatalf("cannot take the docker test lock: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	})
}
