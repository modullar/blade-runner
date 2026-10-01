package dockerlock

import (
	"os"
	"testing"
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
