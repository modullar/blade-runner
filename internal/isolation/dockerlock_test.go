package isolation

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// lockDocker makes a real-Docker test exclusive across test PROCESSES until it finishes.
// `go test ./...` runs packages in parallel, and RemoveStale (and its test) acts on every
// container carrying the job label, including those internal/egress's tests create; without a
// lock they kill and trip each other. internal/egress takes the same lock (same file).
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
