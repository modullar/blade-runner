// Package execx is the one seam between Blade Runner and the operating system's commands
// (launchctl, systemctl, security, config.sh ...). Production uses OS; tests that cannot
// run the real tool (launchd on a Linux box) use Fake.
package execx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Cmd describes one command. Env holds extra KEY=VALUE pairs added to the current
// environment: secrets go here or in Stdin, never in Args, which any local user can read
// from the process table.
type Cmd struct {
	Name  string
	Args  []string
	Env   []string
	Dir   string
	Stdin string
	// Stdout, when set, receives the command's output as it is produced (for `logs -f`)
	// instead of it being collected in Result.Stdout.
	Stdout io.Writer
}

// Result is what a command printed and how it exited.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// ExitError is returned when a command ran and exited non-zero. It never includes Env or
// Stdin, so it is safe to print.
type ExitError struct {
	Cmd    string
	Result Result
}

func (e *ExitError) Error() string {
	msg := strings.TrimSpace(e.Result.Stderr)
	if msg == "" {
		msg = strings.TrimSpace(e.Result.Stdout)
	}
	if msg == "" {
		return fmt.Sprintf("%s exited with status %d", e.Cmd, e.Result.ExitCode)
	}
	return fmt.Sprintf("%s exited with status %d: %s", e.Cmd, e.Result.ExitCode, firstLines(msg, 5))
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = append(lines[:n], "...")
	}
	return strings.Join(lines, "\n")
}

// Runner runs commands.
type Runner interface {
	Run(ctx context.Context, c Cmd) (Result, error)
	LookPath(name string) (string, error)
}

// OS runs real commands.
type OS struct{}

// Run executes c. A non-zero exit returns the Result and an *ExitError.
func (OS) Run(ctx context.Context, c Cmd) (Result, error) {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = c.Dir
	if len(c.Env) > 0 {
		cmd.Env = append(os.Environ(), c.Env...)
	}
	if c.Stdin != "" {
		cmd.Stdin = strings.NewReader(c.Stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if c.Stdout != nil {
		cmd.Stdout = c.Stdout
	}
	err := cmd.Run()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if err == nil {
		return res, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.ExitCode = ee.ExitCode()
		return res, &ExitError{Cmd: c.Name, Result: res}
	}
	return res, fmt.Errorf("run %s: %w", c.Name, err)
}

// LookPath is exec.LookPath.
func (OS) LookPath(name string) (string, error) { return exec.LookPath(name) }
