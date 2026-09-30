// Package host picks the platform implementation for the machine Blade Runner runs on.
package host

import (
	"fmt"

	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/execx"
	"github.com/modullar/blade-runner/internal/platform"
	"github.com/modullar/blade-runner/internal/platform/linux"
	"github.com/modullar/blade-runner/internal/platform/macos"
)

// User is the invoking user, as the service managers need it.
type User struct {
	Name string
	UID  int
	Home string
}

// New returns the platform for goos, or BR-E011 for anything v1 does not support.
func New(goos string, run execx.Runner, u User) (platform.Platform, error) {
	switch goos {
	case "darwin":
		return &macos.Platform{Exec: run, UID: u.UID, Home: u.Home}, nil
	case "linux":
		return &linux.Platform{Exec: run, User: u.Name, Home: u.Home}, nil
	}
	return nil, diag.New(diag.CodeUnsupportedPlatform, fmt.Sprintf("%s is not supported", goos),
		"v1 supports macOS and Linux (systemd) only", "run Blade Runner on a Mac or a Linux machine with systemd")
}
