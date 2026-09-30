// Package platform is the OS extension point (spec section 4.1): prerequisite checks and
// the service manager (launchd on macOS, a systemd user unit on Linux) that keeps the
// runner alive. The implementations live in platform/macos and platform/linux; platform/host
// picks one.
package platform

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/template"

	"github.com/modullar/blade-runner/templates"
)

// Spec is what a service definition is generated from.
type Spec struct {
	RunnerName string
	RunnerDir  string // holds run.sh
	LogDir     string
	Home       string
}

// RunScript is the executable the service manager keeps running.
func (s Spec) RunScript() string { return filepath.Join(s.RunnerDir, "run.sh") }

// Status is the service's state as found on the machine.
type Status struct {
	Installed bool // the definition file exists
	Current   bool // and matches what Render would write now
	Running   bool
	Detail    string
}

// Prereq is one prerequisite check. A failing one names the fix.
type Prereq struct {
	Name   string
	OK     bool
	Detail string
	Fix    string
}

// Platform manages the runner as a background service for the invoking user. It never
// needs root: a per-user LaunchAgent or systemd user unit.
type Platform interface {
	OS() string
	CheckPrereqs(ctx context.Context) []Prereq
	// DefinitionPath is where the service definition file lives.
	DefinitionPath(s Spec) string
	// Render produces the service definition without touching disk (golden-testable).
	Render(s Spec) ([]byte, error)
	// Install writes the definition and registers it with the service manager, without
	// starting it. Re-installing an unchanged definition does nothing.
	Install(ctx context.Context, s Spec) error
	Start(ctx context.Context, s Spec) error
	Stop(ctx context.Context, s Spec) error
	Status(ctx context.Context, s Spec) (Status, error)
	// Uninstall stops the service and removes its definition.
	Uninstall(ctx context.Context, s Spec) error
	Logs(ctx context.Context, s Spec, follow bool, out io.Writer) error
}

// RenderTemplate executes an embedded template. The "xml" function escapes text for plist
// values.
func RenderTemplate(name string, data any) ([]byte, error) {
	t, err := template.New(name).Funcs(template.FuncMap{"xml": xmlEscape}).ParseFS(templates.FS, name)
	if err != nil {
		return nil, fmt.Errorf("load template %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("render template %s: %w", name, err)
	}
	return buf.Bytes(), nil
}

func xmlEscape(s string) (string, error) {
	var b bytes.Buffer
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		return "", err
	}
	return b.String(), nil
}

// WriteDefinition writes a generated file atomically. It reports whether the content
// changed, so callers can reload the service only when needed.
func WriteDefinition(path string, content []byte) (changed bool, err error) {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, content) {
		return false, nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(dir, ".bladerunner-*.tmp")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return false, err
	}
	return true, os.Rename(tmp.Name(), path)
}

// DefinitionStatus reports whether the file exists and is current.
func DefinitionStatus(path string, want []byte) (installed, current bool) {
	got, err := os.ReadFile(path)
	if err != nil {
		return false, false
	}
	return true, bytes.Equal(got, want)
}
