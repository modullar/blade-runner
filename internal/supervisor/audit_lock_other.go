//go:build !unix

package supervisor

import "os"

// lockFile is a no-op where flock does not exist; Blade Runner supports macOS and Linux, which
// have it, so this only keeps the package building elsewhere.
func lockFile(*os.File) (busy bool, err error) { return false, nil }
