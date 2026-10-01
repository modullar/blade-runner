package egress

import (
	"fmt"
	"net/netip"
)

// AddressPolicy decides whether the proxy may connect to an address. The proxy asks it twice for
// every connection: about each address a name resolved to, and again about the address the
// socket is really being connected to.
type AddressPolicy interface {
	// Check returns nil to permit addr, or an error saying why not.
	Check(addr netip.Addr) error
}

// PublicOnly is the policy production uses: global unicast addresses outside a list of ranges it
// knows are not the public internet. The list (forbiddenPrefixes, plus Go's own loopback,
// private, link-local and multicast classes) follows the IANA special-purpose registries as of
// this writing; it is NOT a proof that every address it allows is globally routable, and a range
// IANA assigns later is allowed until it is added here. It refuses loopback, private (RFC 1918 and IPv6 unique-local), link-local (which holds the cloud
// metadata address 169.254.169.254), carrier-grade NAT, multicast, unspecified, documentation,
// benchmarking, reserved, and IPv6 forms that can wrap an IPv4 address (IPv4-mapped, NAT64,
// 6to4, Teredo). It has no configuration on purpose: an allowed name must never be a way to the
// host or the LAN.
type PublicOnly struct{}

var forbiddenPrefixes = mustPrefixes(
	"0.0.0.0/8",       // "this network"
	"100.64.0.0/10",   // carrier-grade NAT; also what some VPNs and cloud metadata use
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // documentation
	"198.18.0.0/15",   // benchmarking
	"198.51.100.0/24", // documentation
	"203.0.113.0/24",  // documentation
	"240.0.0.0/4",     // reserved (includes the broadcast address)
	"::/96",           // deprecated IPv4-compatible
	"64:ff9b::/96",    // NAT64: embeds an IPv4 address the policy would not see
	"64:ff9b:1::/48",  // local-use NAT64
	"100::/64",        // discard-only
	"2001::/32",       // Teredo
	"2001:db8::/32",   // documentation
	"2002::/16",       // 6to4: embeds an IPv4 address
	"::ffff:0:0/96",   // IPv4-mapped (a mapped address is unmapped first; one left is odd)
	"::ffff:0:0:0/96", // SIIT (RFC 2765): IPv4-translated, wraps an IPv4 address Check cannot unmap
	"fec0::/10",       // deprecated site-local
	"192.88.99.0/24",  // deprecated 6to4 relay anycast
	"2001:2::/48",     // benchmarking
	"2001:10::/28",    // ORCHID (deprecated)
	"3fff::/20",       // documentation (RFC 9637)
	"5f00::/16",       // SRv6 segment identifiers
)

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// Check implements AddressPolicy.
func (PublicOnly) Check(addr netip.Addr) error {
	if !addr.IsValid() {
		return fmt.Errorf("not an address")
	}
	a := addr.Unmap() // ::ffff:127.0.0.1 is 127.0.0.1
	if a.Zone() != "" {
		return fmt.Errorf("%s has a zone", a)
	}
	switch {
	case a.IsUnspecified():
		return fmt.Errorf("%s is the unspecified address", a)
	case a.IsLoopback():
		return fmt.Errorf("%s is a loopback address", a)
	case a.IsPrivate():
		return fmt.Errorf("%s is a private address", a)
	case a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast():
		return fmt.Errorf("%s is a link-local address (cloud metadata lives here)", a)
	case a.IsMulticast():
		return fmt.Errorf("%s is a multicast address", a)
	case !a.IsGlobalUnicast():
		return fmt.Errorf("%s is not a global unicast address", a)
	}
	for _, p := range forbiddenPrefixes {
		if p.Contains(a) {
			return fmt.Errorf("%s is in the reserved range %s", a, p)
		}
	}
	return nil
}
