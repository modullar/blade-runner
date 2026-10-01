// Package version carries the CLI version and compares X.Y.Z versions for
// bladerunner.min_version.
package version

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is overridden at release time: -ldflags "-X .../internal/version.Version=0.1.0".
var Version = "0.1.0-dev"

// Parse reads "X.Y.Z", ignoring a leading "v" and any "-prerelease" or "+build" suffix.
func Parse(s string) ([3]int, error) {
	var out [3]int
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return out, fmt.Errorf("%q is not X.Y.Z", s)
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, fmt.Errorf("%q is not X.Y.Z", s)
		}
		out[i] = n
	}
	return out, nil
}

// Less reports whether a is older than b. A prerelease suffix is ignored, so
// 0.1.0-dev satisfies min_version 0.1.0.
func Less(a, b string) (bool, error) {
	pa, err := Parse(a)
	if err != nil {
		return false, err
	}
	pb, err := Parse(b)
	if err != nil {
		return false, err
	}
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] < pb[i], nil
		}
	}
	return false, nil
}
