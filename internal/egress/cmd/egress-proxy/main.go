// Command egress-proxy is the allowlisting forward proxy that runs in its own container next to
// a job's internal network (see docs/decisions/0008-egress.md). It has no option to allow an
// internal address: that is the point. Build it static (CGO_ENABLED=0) into a FROM scratch image.
package main

import (
	"os"

	"github.com/modullar/blade-runner/internal/egress"
)

func main() {
	os.Exit(egress.RunProxy(os.Args[1:], os.Stdout, os.Stderr, nil))
}
