// Package dockerlock lets real-Docker tests exclude each other across test PROCESSES.
// `go test ./...` runs packages in parallel against one daemon, and isolation's RemoveStale (and
// its test) acts on every container carrying the job label, including those other packages'
// tests create; without a lock they kill and trip each other.
//
// The lock file lives in a directory only the current user can use: a fixed, world-writable
// path such as /tmp/<name>.lock can be pre-created, symlinked or held forever by another user.
// Both candidate directories (the user cache directory and the temp directory) are checked for
// ownership and permissions before use. The consequence is that the lock serialises one user's
// test processes, not two users sharing one daemon.
//
// Within a process the lock is re-entrant for a test and its subtests (calling Lock again from
// either returns at once rather than deadlocking against itself); an unrelated test of the same
// process waits for it. On platforms without flock (Windows) Lock skips the calling test.
package dockerlock

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Dir returns the private directory holding the lock file, creating it if needed.
func Dir() (string, error) {
	var firstErr error
	if base, err := os.UserCacheDir(); err == nil {
		dir := filepath.Join(base, "bladerunner")
		if err := os.MkdirAll(dir, 0o700); err == nil {
			if err := checkPrivate(dir); err == nil {
				return dir, nil
			} else {
				firstErr = err
			}
		}
	}
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("bladerunner-docker-tests-%d", os.Getuid()))
	if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
		return "", err
	}
	// It may have been created by someone else first: use it only if it is ours and private.
	if err := checkPrivate(dir); err != nil {
		if firstErr != nil {
			return "", fmt.Errorf("%v; and %v", firstErr, err)
		}
		return "", err
	}
	return dir, nil
}

var (
	procMu     sync.Mutex    // guards holder
	holder     string        // name of the test that holds the lock in this process
	procToken  = make(chan struct{}, 1)
	errSkipped = "docker tests are serialised with flock, which this platform lacks"
)

// Lock makes the calling test exclusive across test processes until it finishes. It is
// re-entrant: a test that already holds it (or whose parent test does) returns at once.
func Lock(t testing.TB) {
	t.Helper()
	if !lockSupported {
		t.Skip(errSkipped)
	}
	name := t.Name()
	procMu.Lock()
	reentrant := holder != "" && (name == holder || strings.HasPrefix(name, holder+"/"))
	procMu.Unlock()
	if reentrant {
		return
	}
	procToken <- struct{}{} // an unrelated test of this process holds it: wait
	release := func() { <-procToken }
	dir, err := Dir()
	if err != nil {
		release()
		t.Fatalf("cannot prepare the docker test lock directory: %v", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "docker-tests.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		release()
		t.Fatalf("cannot open the docker test lock: %v", err)
	}
	if err := flock(f); err != nil {
		f.Close()
		release()
		t.Fatalf("cannot take the docker test lock: %v", err)
	}
	procMu.Lock()
	holder = name
	procMu.Unlock()
	t.Cleanup(func() {
		procMu.Lock()
		holder = ""
		procMu.Unlock()
		unflock(f)
		f.Close()
		release()
	})
}
