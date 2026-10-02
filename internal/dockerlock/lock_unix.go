//go:build unix

package dockerlock

import (
	"fmt"
	"os"
	"syscall"
)

const lockSupported = true

func flock(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_EX) }

func unflock(f *os.File) { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }

// checkPrivate accepts dir only if it is a real directory (not a symlink) owned by this user
// and closed to group and others.
func checkPrivate(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || !fi.IsDir() || int(st.Uid) != os.Getuid() || fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s is not a private directory owned by this user", dir)
	}
	return nil
}
