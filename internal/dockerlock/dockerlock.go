// Package dockerlock lets real-Docker tests exclude each other across test PROCESSES.
// `go test ./...` runs packages in parallel against one daemon, and isolation's RemoveStale (and
// its test) acts on every container carrying the job label, including those other packages'
// tests create; without a lock they kill and trip each other.
//
// The lock file lives in a directory only the current user can use: a fixed, world-writable
// path such as /tmp/<name>.lock can be pre-created, symlinked or held forever by another user.
// The consequence is that the lock serialises one user's test processes, not two users sharing
// one daemon.
package dockerlock

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Dir returns the private directory holding the lock file, creating it if needed.
func Dir() (string, error) {
	if base, err := os.UserCacheDir(); err == nil {
		dir := filepath.Join(base, "bladerunner")
		if err := os.MkdirAll(dir, 0o700); err == nil {
			return dir, nil
		}
	}
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("bladerunner-docker-tests-%d", os.Getuid()))
	if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
		return "", err
	}
	// It may have been created by someone else first: use it only if it is ours and private.
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || !fi.IsDir() || int(st.Uid) != os.Getuid() || fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s is not a private directory owned by this user", dir)
	}
	return dir, nil
}

// Lock makes the calling test exclusive across test processes until it finishes.
func Lock(t testing.TB) {
	t.Helper()
	dir, err := Dir()
	if err != nil {
		t.Fatalf("cannot prepare the docker test lock directory: %v", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "docker-tests.lock"), os.O_CREATE|os.O_RDWR, 0o600)
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
