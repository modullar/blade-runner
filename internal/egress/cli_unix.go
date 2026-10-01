//go:build unix

package egress

import (
	"os"
	"syscall"
)

// flushSignals are the signals that ask the proxy to write its pending log lines now.
func flushSignals() []os.Signal { return []os.Signal{syscall.SIGUSR1} }
