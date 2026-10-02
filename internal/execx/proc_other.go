//go:build !unix

package execx

import (
	"os/exec"
	"time"
)

// confine on platforms without process groups: cancelling kills the direct child only, and Run
// never waits longer than WaitDelay for the pipes to drain.
func confine(cmd *exec.Cmd) {
	cmd.WaitDelay = 3 * time.Second
}
