package dockerlock

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLockDirIsPrivateAndNotTheOldSharedPath(t *testing.T) {
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() || fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("lock dir %s: %v %v", dir, fi, err)
	}
	if dir == os.TempDir() {
		t.Errorf("the lock must not sit directly in the shared temp dir")
	}
}

func TestLockIsReleasedAtCleanup(t *testing.T) {
	t.Run("first", func(t *testing.T) { Lock(t) })
	t.Run("second", func(t *testing.T) { Lock(t) }) // would hang if the first never released
}

func TestDirRefusesALooseCacheDirectoryInsteadOfUsingIt(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("HOME", cache)
	loose := filepath.Join(cache, "bladerunner")
	if err := os.Mkdir(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(loose, 0o777); err != nil { // MkdirAll would leave this alone
		t.Fatal(err)
	}
	dir, err := Dir()
	if err != nil {
		t.Fatal(err) // the private temp-dir fallback is available
	}
	if dir == loose {
		t.Fatalf("Dir returned the group/world-writable %s", loose)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("fallback %s is not private: %v %v", dir, fi, err)
	}
}

func TestDirRefusesACacheDirectoryThatIsASymlink(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("HOME", cache)
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(cache, "bladerunner")); err != nil {
		t.Skip("symlinks unavailable")
	}
	dir, err := Dir()
	if err == nil && dir == filepath.Join(cache, "bladerunner") {
		t.Fatalf("Dir followed a symlink planted at %s", dir)
	}
}

// subTB is a testing.TB with its own name and cleanups, so Lock's re-entrancy can be exercised
// without needing a second test process.
type subTB struct {
	testing.TB
	name     string
	cleanups []func()
}

func (s *subTB) Name() string     { return s.name }
func (s *subTB) Cleanup(f func()) { s.cleanups = append(s.cleanups, f) }
func (s *subTB) Helper()          {}
func (s *subTB) release() {
	for i := len(s.cleanups) - 1; i >= 0; i-- {
		s.cleanups[i]()
	}
}

func TestLockIsReentrantForATestAndItsSubtests(t *testing.T) {
	parent := &subTB{TB: t, name: "TestX"}
	child := &subTB{TB: t, name: "TestX/sub"}
	done := make(chan struct{})
	go func() {
		defer close(done)
		Lock(parent)
		Lock(parent) // the same test again
		Lock(child)  // a subtest of the holder
		child.release()
		parent.release()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Lock deadlocked against itself")
	}
	// Released for real: another test can take it.
	other := &subTB{TB: t, name: "TestY"}
	Lock(other)
	other.release()
}
