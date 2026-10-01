// Package templates holds the files Blade Runner generates: the launchd plist and the
// systemd unit. Workflow snippets join them in BR-3. They are embedded so the binary is
// the only thing a machine needs.
package templates

import "embed"

// FS contains every *.tmpl file in this directory.
//
//go:embed *.tmpl
var FS embed.FS
