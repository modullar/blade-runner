//go:build unix

package execx

import (
	"os/exec"
	"syscall"
	"time"
)

// confine makes cancelling a command stop the whole process tree, not just the direct child.
// Without it, killing `sh` leaves its children holding the output pipes, and Run would block
// until they exit on their own.
func confine(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// A negative pid signals the whole process group.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = 3 * time.Second // and never wait longer than this for pipes to drain
}
