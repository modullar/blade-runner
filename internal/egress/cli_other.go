//go:build !unix

package egress

import "os"

func flushSignals() []os.Signal { return nil }
