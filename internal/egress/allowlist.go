// Package egress gives a job that needs the network a way out that is limited to a list of
// hostnames, and gives it no way to reach the host, the host's network, cloud metadata or other
// containers.
//
// The design (docs/decisions/0008-egress.md) has two halves that only work together:
//
//   - The job's container sits on a Docker network that is internal (no route out) and on
//     which the host has no address. The only other thing on that network is a proxy.
//   - The proxy (this package: Proxy, and the command in cmd/egress-proxy) accepts HTTP CONNECT
//     for allowlisted hostnames only, resolves the name itself, refuses every internal address
//     it finds, and dials the address it checked.
//
// Manager builds and tears down that topology around isolation.Docker, and internal/isolation
// audits it (network mode "allowlist") before a job is allowed to start.
package egress

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"github.com/modullar/blade-runner/internal/diag"
)

// DefaultAllowlist is a starting point for a runner that must reach GitHub. It is NOT verified
// against a live runner (BR-0): treat it as a configurable default, not a fact.
var DefaultAllowlist = []string{
	"github.com",
	"api.github.com",
	"codeload.github.com",
	"objects.githubusercontent.com",
	"ghcr.io",
	"*.actions.githubusercontent.com",
}

// DefaultPorts are the ports a CONNECT may target.
var DefaultPorts = []int{443}

// Allowlist is a set of hostnames a job may reach: exact names, and wildcards. A wildcard
// `*.example.com` matches every name BELOW example.com at any depth (a.example.com and
// a.b.example.com) and never example.com itself; trust it as you would trust everyone who can
// create a name under that domain. Wildcards over a public or multi-tenant suffix are refused
// (see sharedSuffixes).
type Allowlist struct {
	exact    map[string]bool
	suffixes []string // ".example.com" for "*.example.com"
}

func policyInvalid(what string) error {
	return diag.New(diag.CodeEgressPolicyInvalid, "the egress allowlist is invalid: "+what,
		"an entry is not a plain hostname or a `*.domain` wildcard",
		"fix the allowlist in the configuration; entries are lower-case hostnames, never IP addresses or URLs")
}

// ParseAllowlist validates entries and builds the list. An empty list is refused: "allow
// nothing" is network mode none, and an accidental empty list should not look like a policy.
func ParseAllowlist(entries []string) (Allowlist, error) {
	if len(entries) == 0 {
		return Allowlist{}, policyInvalid("it is empty (use network mode none for no network)")
	}
	a := Allowlist{exact: map[string]bool{}}
	for _, e := range entries {
		wild := strings.HasPrefix(e, "*.")
		name := strings.TrimPrefix(e, "*.")
		if err := checkHostname(name); err != nil {
			return Allowlist{}, policyInvalid(fmt.Sprintf("%q: %v", e, err))
		}
		if wild {
			if !strings.Contains(name, ".") {
				return Allowlist{}, policyInvalid(fmt.Sprintf("%q would allow every name under a top-level domain", e))
			}
			if isSharedSuffix(name) {
				return Allowlist{}, policyInvalid(fmt.Sprintf("%q would allow every registrant under the shared suffix %s (name one registrant's domain instead, or list exact hosts)", e, name))
			}
			a.suffixes = append(a.suffixes, "."+name)
		} else {
			a.exact[name] = true
		}
	}
	return a, nil
}

// Allowed reports whether a normalised hostname (see NormalizeHost) is on the list.
func (a Allowlist) Allowed(host string) bool {
	if a.exact[host] {
		return true
	}
	for _, s := range a.suffixes {
		if strings.HasSuffix(host, s) && len(host) > len(s) {
			return true
		}
	}
	return false
}

// Entries returns the list in the form ParseAllowlist accepts, for passing to the proxy.
func (a Allowlist) Entries() []string {
	var out []string
	for e := range a.exact {
		out = append(out, e)
	}
	for _, s := range a.suffixes {
		out = append(out, "*"+s)
	}
	sort.Strings(out)
	return out
}

// checkHostname accepts only lower-case ASCII letters, digits and hyphens in dot-separated
// labels. Anything else (upper case, IDN, underscores, an IP address, a port) is refused, so
// the allowlist and the host a client names are compared as plain bytes.
func checkHostname(h string) error {
	if h == "" {
		return fmt.Errorf("empty name")
	}
	if len(h) > 253 {
		return fmt.Errorf("longer than 253 bytes")
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return fmt.Errorf("an IP address (the allowlist is by name)")
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 {
			return fmt.Errorf("empty or over-long label")
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("label starts or ends with a hyphen")
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return fmt.Errorf("character %q is not a lower-case ASCII letter, digit or hyphen", c)
			}
		}
	}
	// inet_aton-style resolvers read a name whose every label is a number as an IPv4 address:
	// decimal ("1.2.3"), octal ("010.1") or hex ("0x7f.1"). Refuse all three forms, so such a
	// name is never on the list or accepted from a client. (This is defence in depth: whatever a
	// name resolves to is still checked by the AddressPolicy.) A name with any ordinary label,
	// like "0x7f.example", is not one of these and is allowed.
	numeric := true
	for _, label := range strings.Split(h, ".") {
		if !numericLabel(label) {
			numeric = false
			break
		}
	}
	if numeric {
		return fmt.Errorf("looks like a numeric address")
	}
	return nil
}

// numericLabel reports whether label is a decimal or octal number, or 0x followed by hex digits.
func numericLabel(label string) bool {
	if strings.HasPrefix(label, "0x") {
		label = label[2:]
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return false
			}
		}
		return true
	}
	for i := 0; i < len(label); i++ {
		if label[i] < '0' || label[i] > '9' {
			return false
		}
	}
	return label != ""
}

// NormalizeHost lower-cases a client-supplied host and drops one trailing dot. It returns false
// when the result is not a plain hostname, so the proxy never compares or resolves odd input.
func NormalizeHost(h string) (string, bool) {
	h = strings.TrimSuffix(strings.ToLower(h), ".")
	if checkHostname(h) != nil {
		return "", false
	}
	return h, true
}
