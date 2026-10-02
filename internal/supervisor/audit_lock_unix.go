//go:build unix

package supervisor

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive, non-blocking flock on f. The operating system drops it when the
// process dies, so a crash never leaves a stale lock. It reports whether the lock is held by
// someone else.
func lockFile(f *os.File) (busy bool, err error) {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if err == syscall.EWOULDBLOCK {
			return true, nil
		}
		return false, err
	}
	return false, nil
}
