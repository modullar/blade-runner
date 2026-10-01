//go:build unix

package core

import (
	"fmt"
	"os"
	"syscall"

	"github.com/modullar/blade-runner/internal/diag"
)

// LockDir takes an exclusive, non-blocking lock on a directory and returns the function that
// releases it. It uses flock, so the operating system drops the lock if the process dies: a
// crashed run can never leave a stale lock behind, and the lock leaves no file to clean up.
func LockDir(dir string) (unlock func(), err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, diag.New(diag.CodeBusy, "another bladerunner command is already changing this machine",
				fmt.Sprintf("a second `apply` or `remove` was started while one holds the lock on %s", dir),
				"wait for the other command to finish, then run this one again")
		}
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
