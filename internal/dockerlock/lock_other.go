//go:build !unix

package dockerlock

import "os"

const lockSupported = false

func flock(*os.File) error { return nil }

func unflock(*os.File) {}

// checkPrivate: there is no uid/mode to check here; Lock skips on this platform anyway.
func checkPrivate(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return os.ErrInvalid
	}
	return nil
}
