package cli

import (
	"fmt"
	"io"

	"github.com/modullar/blade-runner/internal/core"
)

// printer is a core.Reporter that writes one aligned line per event.
type printer struct{ w io.Writer }

func (p printer) Note(msg string) { fmt.Fprintf(p.w, "note: %s\n", msg) }

func (p printer) GatePassed(id string) { fmt.Fprintf(p.w, "  %-13s %s\n", "ok", "check "+id) }

func (p printer) StepResult(id string, o core.Outcome, msg string) {
	fmt.Fprintf(p.w, "  %-13s %-20s %s\n", o, id, msg)
}
