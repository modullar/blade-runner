// Command bladerunner turns a Mac or Linux machine into a self-hosted GitHub Actions runner.
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/modullar/blade-runner/internal/cli"
	"github.com/modullar/blade-runner/internal/doctor"
	"github.com/modullar/blade-runner/internal/version"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(cli.Run(ctx, os.Args[1:], systemDeps()))
}

// systemDeps wires the commands to the real machine.
func systemDeps() cli.Deps {
	u, err := user.Current()
	if err != nil {
		fmt.Fprintln(os.Stderr, "bladerunner: cannot determine the current user:", err)
		os.Exit(cli.ExitFailure)
	}
	uid, _ := strconv.Atoi(u.Uid)
	self, _ := os.Executable()
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	return cli.Deps{
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		UserHome: u.HomeDir, UserName: u.Username, UID: uid,
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		IsTerminal: isTerminal(os.Stdin),
		ReadSecret: readSecret,
		Version:    version.Version,
		BinaryPath: self,
		FreeBytes:  doctor.FreeBytes,
	}
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// readSecret reads one line with terminal echo off. If echo cannot be turned off it
// refuses rather than show a token on screen.
func readSecret() (string, error) {
	off := exec.Command("stty", "-echo")
	off.Stdin = os.Stdin
	if err := off.Run(); err != nil {
		return "", fmt.Errorf("cannot turn off terminal echo: %w", err)
	}
	defer func() {
		on := exec.Command("stty", "echo")
		on.Stdin = os.Stdin
		_ = on.Run()
		fmt.Fprintln(os.Stderr)
	}()
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}
