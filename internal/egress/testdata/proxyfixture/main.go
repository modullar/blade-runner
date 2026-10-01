// Command proxyfixture is a TEST-ONLY build of the egress proxy. It differs from
// cmd/egress-proxy in exactly two ways, both needed because a test has no public internet:
//
//   - -resolve name=ip answers lookups from a fixed table instead of DNS;
//   - -trust-cidr lets the proxy connect inside one CIDR (the test's stand-in "internet", a
//     listener on the Docker host) that the production policy would refuse.
//
// Everything else (allowlist, ports, CONNECT-only, resolve-once-then-dial-the-address, the
// network topology around it) is the production code. It is never shipped.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/netip"
	"os"
	"strings"

	"github.com/modullar/blade-runner/internal/egress"
)

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(s string) error { *m = append(*m, s); return nil }

type table map[string][]netip.Addr

func (t table) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if a, ok := t[strings.TrimSuffix(host, ".")]; ok { // the proxy asks for absolute names
		return a, nil
	}
	return nil, fmt.Errorf("no such host %s", host)
}

type trusting struct {
	cidrs []netip.Prefix
	base  egress.AddressPolicy
}

func (t trusting) Check(a netip.Addr) error {
	for _, c := range t.cidrs {
		if c.Contains(a.Unmap()) {
			return nil
		}
	}
	return t.base.Check(a)
}

func main() {
	os.Exit(egress.RunProxy(os.Args[1:], os.Stdout, os.Stderr, func(fs *flag.FlagSet) func(*egress.Proxy) {
		var resolve, trust multi
		fs.Var(&resolve, "resolve", "name=ip (repeatable)")
		fs.Var(&trust, "trust-cidr", "a CIDR the proxy may connect to (repeatable)")
		return func(p *egress.Proxy) {
			t := table{}
			for _, r := range resolve {
				name, ip, _ := strings.Cut(r, "=")
				t[name] = append(t[name], netip.MustParseAddr(ip))
			}
			p.Resolver = t
			pol := trusting{base: egress.PublicOnly{}}
			for _, c := range trust {
				pol.cidrs = append(pol.cidrs, netip.MustParsePrefix(c))
			}
			p.Policy = pol
		}
	}))
}
