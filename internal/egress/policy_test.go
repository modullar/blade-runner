package egress

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/modullar/blade-runner/internal/diag"
)

func TestAllowlistMatching(t *testing.T) {
	a, err := ParseAllowlist([]string{"github.com", "*.example.com", "localhost"})
	if err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string]bool{
		"github.com":                 true,
		"api.github.com":             false, // an exact entry does not cover subdomains
		"a.example.com":              true,
		"a.b.example.com":            true,
		"example.com":                false, // a wildcard does not cover the apex
		"evilexample.com":            false, // suffix match must be on a label boundary
		"example.com.evil.com":       false,
		"github.com.evil.com":        false,
		"notgithub.com":              false,
		"localhost":                  true,
		".example.com":               false,
		"":                           false,
		"example.com.":               false, // callers normalise first; the raw form is not on the list
		"b.example.com.attacker.org": false,
	} {
		if got := a.Allowed(host); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestParseAllowlistRefusesWhatItCannotTrust(t *testing.T) {
	for name, entries := range map[string][]string{
		"empty":                 nil,
		"empty entry":           {""},
		"an IPv4 address":       {"1.2.3.4"},
		"an IPv6 address":       {"::1"},
		"loopback":              {"127.0.0.1"},
		"a bare star":           {"*"},
		"a star over a TLD":     {"*.com"},
		"a star in the middle":  {"a.*.com"},
		"a star without a dot":  {"*example.com"},
		"a port":                {"github.com:443"},
		"a URL":                 {"https://github.com"},
		"a path":                {"github.com/x"},
		"upper case":            {"GitHub.com"},
		"non-ASCII":             {"gîthub.com"},
		"an underscore":         {"a_b.example.com"},
		"a leading hyphen":      {"-a.example.com"},
		"an empty label":        {"a..example.com"},
		"a trailing dot":        {"example.com."},
		"a space":               {"a b.example.com"},
		"all digits":            {"10.1.2"},
		"over-long label":       {strings.Repeat("a", 64) + ".com"},
		"one bad among good":    {"github.com", "http://x"},
		"a wildcard wildcard":   {"*.*.example.com"},
		"a leading dot wildcad": {".example.com"},
	} {
		_, err := ParseAllowlist(entries)
		if diag.CodeOf(err) != diag.CodeEgressPolicyInvalid {
			t.Errorf("%s: err = %v, want BR-E081", name, err)
		}
	}
	if _, err := ParseAllowlist(DefaultAllowlist); err != nil {
		t.Errorf("the default allowlist must itself be valid: %v", err)
	}
}

func TestNormalizeHost(t *testing.T) {
	for in, want := range map[string]string{"GitHub.COM": "github.com", "github.com.": "github.com", "a.b-c.d": "a.b-c.d"} {
		if got, ok := NormalizeHost(in); !ok || got != want {
			t.Errorf("NormalizeHost(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", ".", "a b", "a@b.com", "a.com/", "evil.com@github.com", "github.com..", "1.2.3.4", "[::1]", "gîthub.com", "a.com\x00.evil.com", "github.com%00"} {
		if got, ok := NormalizeHost(in); ok {
			t.Errorf("NormalizeHost(%q) = %q: must be refused", in, got)
		}
	}
}

func TestEntriesRoundTrip(t *testing.T) {
	a, _ := ParseAllowlist([]string{"b.com", "*.a.com", "c.org"})
	got := strings.Join(a.Entries(), " ")
	if got != "*.a.com b.com c.org" {
		t.Errorf("Entries = %q", got)
	}
	if _, err := ParseAllowlist(a.Entries()); err != nil {
		t.Error(err)
	}
}

func TestPublicOnlyRefusesEveryInternalAddress(t *testing.T) {
	refused := []string{
		"127.0.0.1", "127.255.255.254", "::1", "0.0.0.0", "::", "0.1.2.3",
		"10.0.0.1", "10.255.255.255", "172.16.0.1", "172.31.255.255", "192.168.0.1", "192.168.255.255",
		"169.254.169.254", "169.254.0.1", "fe80::1", "fe80::a00:27ff:fe00:1",
		"100.64.0.1", "100.100.100.200", "100.127.255.255", // carrier-grade NAT, incl. a cloud metadata address
		"fd00:ec2::254", "fc00::1", "fd12:3456::1", // IPv6 unique-local, incl. a cloud metadata address
		"224.0.0.1", "239.255.255.250", "ff02::1", "ff05::2",
		"192.0.0.1", "192.0.2.1", "198.18.0.1", "198.19.255.255", "198.51.100.1", "203.0.113.1", "240.0.0.1", "255.255.255.255",
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:169.254.169.254", "::ffff:192.168.1.1", // IPv4-mapped forms
		"64:ff9b::7f00:1", "64:ff9b::a00:1", "2002:7f00:1::1", "2002:a00:1::1", "2001:0:4136:e378:8000:63bf:3fff:fdd2", // forms that wrap an IPv4 address
		"2001:db8::1", "100::1", "::7f00:1",
	}
	for _, s := range refused {
		if err := (PublicOnly{}).Check(netip.MustParseAddr(s)); err == nil {
			t.Errorf("%s must be refused", s)
		}
	}
	if err := (PublicOnly{}).Check(netip.Addr{}); err == nil {
		t.Error("the zero address must be refused")
	}
	if err := (PublicOnly{}).Check(netip.MustParseAddr("fe80::1%eth0")); err == nil {
		t.Error("a zoned address must be refused")
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "140.82.112.3", "93.184.216.34", "172.32.0.1", "172.15.255.255", "100.63.255.255", "100.128.0.1", "2606:4700:4700::1111", "2a00:1450:4001::1"} {
		if err := (PublicOnly{}).Check(netip.MustParseAddr(s)); err != nil {
			t.Errorf("%s is a public address and must be allowed: %v", s, err)
		}
	}
}
