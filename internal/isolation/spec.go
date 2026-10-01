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
// only blocked by firewall rules this package does not install (see docs/decisions/0006). The
// "allowlist" mode closes that gap for jobs that need the network (docs/decisions/0008).
package isolation

import (
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"github.com/modullar/blade-runner/internal/diag"
)

// Network modes. There is deliberately no "host" mode.
const (
	NetworkNone   = "none"   // no network at all
	NetworkBridge = "bridge" // an isolated bridge with outbound access
	// NetworkAllowlist puts the job on a Docker network with no route out and no address for the
	// host on it, whose only neighbour is an allowlisting proxy (internal/egress). The job can
	// reach the hostnames the proxy allows, through the proxy, and nothing else.
	NetworkAllowlist = "allowlist"
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

	Network string
	// The three fields below are set for NetworkAllowlist (by egress.Session.Apply) and must be
	// empty otherwise.
	EgressNetwork        string // the internal Docker network the job joins
	EgressProxy          string // the proxy's address on that network, "ip:port"
	EgressProxyContainer string // the proxy's container name, the only neighbour allowed

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

// proxyEnvNames are the variables a client uses to find a proxy. In allowlist mode they belong
// to Blade Runner: a caller must not be able to point the job at another proxy, or exempt a host
// from this one with NO_PROXY.
var proxyEnvNames = []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY"}

func isProxyEnv(k string) bool {
	for _, n := range proxyEnvNames {
		if strings.EqualFold(k, n) {
			return true
		}
	}
	return false
}

// reservedNetworks are names Docker gives meaning to. "bridge" is the default bridge (on which
// every other container lives), "host" and "none" are modes, "container:<id>" joins another
// container's stack, and the rest are Docker's built-ins. None of them can be the one network
// an allowlist job lives on: it must be a network egress.Open made for this job.
var reservedNetworks = []string{"bridge", "none", "host", "default", "ingress", "docker_gwbridge"}

func reservedNetwork(name string) bool {
	if strings.HasPrefix(strings.ToLower(name), "container:") {
		return true
	}
	for _, r := range reservedNetworks {
		if strings.EqualFold(name, r) {
			return true
		}
	}
	return false
}

func (s Spec) validateEgress() error {
	if !nameRe.MatchString(s.EgressNetwork) || reservedNetwork(s.EgressNetwork) {
		return invalid(fmt.Sprintf("allowlist mode needs the name of the job's own egress network (not a built-in such as bridge, host or none), got %q", s.EgressNetwork))
	}
	if !nameRe.MatchString(s.EgressProxyContainer) {
		return invalid(fmt.Sprintf("allowlist mode needs the proxy's container name, got %q", s.EgressProxyContainer))
	}
	ap, err := netip.ParseAddrPort(s.EgressProxy)
	if err != nil || !ap.Addr().Is4() || ap.Addr().IsLoopback() || ap.Addr().IsUnspecified() || ap.Port() == 0 {
		// An IP literal, so the job needs no DNS to find the proxy. IPv6 is not used: the egress
		// network is IPv4-only and the audit requires that.
		return invalid(fmt.Sprintf("allowlist mode needs the proxy as an IPv4 ip:port on the egress network, got %q", s.EgressProxy))
	}
	for k := range s.Env {
		if isProxyEnv(k) {
			return invalid(fmt.Sprintf("environment variable %s is reserved in allowlist mode: the proxy settings are not the caller's to change", k))
		}
	}
	return nil
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
	case NetworkAllowlist:
		if err := s.validateEgress(); err != nil {
			return s, err
		}
	case "":
		s.Network = NetworkNone // the safe default
	default:
		return s, invalid(fmt.Sprintf("network mode %q is not allowed (only %q, %q and %q)", s.Network, NetworkNone, NetworkBridge, NetworkAllowlist))
	}
	if s.Network != NetworkAllowlist && (s.EgressNetwork != "" || s.EgressProxy != "" || s.EgressProxyContainer != "") {
		return s, invalid("egress settings are only meaningful with network mode " + NetworkAllowlist)
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
