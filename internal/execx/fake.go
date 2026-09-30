package execx

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
)

// Fake records commands and answers them from a handler. It is the last-resort stand-in
// for tools that cannot run on the test machine (launchctl, security): assert on what a
// unit did with the answers, but prefer real commands wherever the machine has them.
type Fake struct {
	mu    sync.Mutex
	Calls []Cmd
	// Handler answers a command. A nil Handler, or a nil return, means success with no
	// output. Return (Result, err) to simulate failure.
	Handler func(Cmd) (Result, error)
	// Tools are the names LookPath finds; others are "not found".
	Tools map[string]bool
}

// Run records c and returns the handler's answer.
func (f *Fake) Run(_ context.Context, c Cmd) (Result, error) {
	f.mu.Lock()
	f.Calls = append(f.Calls, c)
	h := f.Handler
	f.mu.Unlock()
	if h == nil {
		return Result{}, nil
	}
	res, err := h(c)
	if c.Stdout != nil {
		_, _ = c.Stdout.Write([]byte(res.Stdout))
		res.Stdout = ""
	}
	return res, err
}

// LookPath finds only the tools listed in Tools.
func (f *Fake) LookPath(name string) (string, error) {
	if f.Tools[name] {
		return "/fake/bin/" + name, nil
	}
	return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
}

// Line returns call i as one "name arg arg" string, for readable assertions.
func (f *Fake) Line(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.Calls) {
		return ""
	}
	c := f.Calls[i]
	return fmt.Sprint(append([]string{c.Name}, c.Args...))
}

// Lines returns every recorded call in order.
func (f *Fake) Lines() []string {
	f.mu.Lock()
	n := len(f.Calls)
	f.mu.Unlock()
	out := make([]string, n)
	for i := range out {
		out[i] = f.Line(i)
	}
	return out
}

// Fail is a Handler result for a command that exited with code.
func Fail(name string, code int, stderr string) (Result, error) {
	r := Result{ExitCode: code, Stderr: stderr}
	return r, &ExitError{Cmd: name, Result: r}
}
