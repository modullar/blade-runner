// Package isolation runs a job in a fresh, unprivileged, resource-limited container that is
// destroyed afterwards. Its job is containment: whatever the code inside does, it stays inside.
//
// Two rules make the containment checkable rather than hoped for:
//
//  1. The container is created first, inspected, and AUDITED against the invariants in
//     audit.go before it is started. A flag that silently did not take effect, or a runtime
//     that ignores one, stops the job instead of running it unconfined.
//  2. The image is identified by content (a digest or image id), never by a tag that can move.
//
// Limits, stated plainly: a container shares its host's kernel, so a kernel flaw could let code
// out; a virtual machine is a stronger boundary (on macOS Docker already runs inside one). The
// "bridge" network mode reaches whatever the host network reaches: host services and the LAN are
// only blocked by firewall rules this package does not install (see docs/decisions/0006).
package isolation

import (
	"fmt"
	"regexp"
	"time"

	"github.com/modullar/blade-runner/internal/diag"
)

// Network modes. There is deliberately no "host" mode.
const (
	NetworkNone   = "none"   // no network at all
	NetworkBridge = "bridge" // an isolated bridge with outbound access
)

// Spec describes one job's container.
type Spec struct {
	Name  string // a unique container name
	Image string // an immutable reference: name@sha256:<hex> or sha256:<hex>
	Args  []string
	// Stdin is how secrets reach the job. Never argv (visible in the process table) and never
	// environment variables (visible to `docker inspect` and to anything in the container).
	Stdin string
	// Env is for NON-secret settings only.
	Env    map[string]string
	Labels map[string]string

	Network      string
	CPUs         float64       // e.g. 2
	MemoryMiB    int           // a hard cap; the job is killed above it
	PidsLimit    int           // a cap on processes, so a fork bomb stops
	WorkTmpfsMiB int           // size of the only writable area besides /tmp
	Timeout      time.Duration // the job is killed after this
}

// Defaults for the limits, chosen to be small but workable; callers override them.
const (
	DefaultCPUs      = 2.0
	DefaultMemoryMiB = 4096
	DefaultPids      = 512
	DefaultWorkMiB   = 4096
	DefaultTimeout   = 60 * time.Minute
)

var (
	digestRe = regexp.MustCompile(`^(?:[a-zA-Z0-9][a-zA-Z0-9./_:-]*@)?sha256:[0-9a-f]{64}$`)
	nameRe   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
)

func invalid(what string) error {
	return diag.New(diag.CodeIsolation, "the job's container was refused: "+what,
		"the job description is unsafe or incomplete", "fix the caller: this is a bug in Blade Runner, not something to override")
}

// Validate refuses a Spec that could not be made safe, and fills defaults into a copy.
func (s Spec) Validate() (Spec, error) {
	if !nameRe.MatchString(s.Name) {
		return s, invalid(fmt.Sprintf("%q is not a valid container name", s.Name))
	}
	if !digestRe.MatchString(s.Image) {
		return s, invalid(fmt.Sprintf("image %q is not pinned by content (name@sha256:... or sha256:...): a tag can be moved to different code", s.Image))
	}
	switch s.Network {
	case NetworkNone, NetworkBridge:
	case "":
		s.Network = NetworkNone // the safe default
	default:
		return s, invalid(fmt.Sprintf("network mode %q is not allowed (only %q and %q)", s.Network, NetworkNone, NetworkBridge))
	}
	for k := range s.Env {
		if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(k) {
			return s, invalid(fmt.Sprintf("%q is not a valid environment variable name", k))
		}
	}
	if s.CPUs <= 0 {
		s.CPUs = DefaultCPUs
	}
	if s.MemoryMiB <= 0 {
		s.MemoryMiB = DefaultMemoryMiB
	}
	if s.PidsLimit <= 0 {
		s.PidsLimit = DefaultPids
	}
	if s.WorkTmpfsMiB <= 0 {
		s.WorkTmpfsMiB = DefaultWorkMiB
	}
	if s.Timeout <= 0 {
		s.Timeout = DefaultTimeout
	}
	return s, nil
}
