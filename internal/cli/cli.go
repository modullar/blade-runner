// Package cli is the command layer: parse flags, build the environment, delegate to the
// engine, shape the output. Handlers stay thin; the work is in config, install, doctor and
// core.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/modullar/blade-runner/internal/config"
	"github.com/modullar/blade-runner/internal/core"
	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/download"
	"github.com/modullar/blade-runner/internal/execx"
	"github.com/modullar/blade-runner/internal/install"
	"github.com/modullar/blade-runner/internal/platform"
	"github.com/modullar/blade-runner/internal/platform/host"
	"github.com/modullar/blade-runner/internal/provider/github"
	"github.com/modullar/blade-runner/internal/secrets"
)

// Exit codes.
const (
	ExitOK      = 0
	ExitFailure = 1
	ExitUsage   = 2
)

// Deps are the machine and network facts the commands depend on. main fills them from the
// real system; tests substitute a fake GitHub, a fake service manager and scripted input.
type Deps struct {
	GOOS, GOARCH string
	UserHome     string
	UserName     string
	UID          int
	Getenv       func(string) string
	Hostname     func() (string, error)
	Exec         execx.Runner
	Euid         func() int

	GitHubAPIURL string // empty: https://api.github.com
	GitHubWebURL string // empty: https://github.com
	Fetcher      *download.Fetcher
	NewPlatform  func(goos string, run execx.Runner, u host.User) (platform.Platform, error)
	FreeBytes    func(path string) (uint64, error) // nil: the real filesystem

	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
	IsTerminal bool                   // stdin is an interactive terminal
	ReadSecret func() (string, error) // reads a line without echo; needed for interactive token entry
	Version    string
}

func (d *Deps) fill() {
	if d.Getenv == nil {
		d.Getenv = os.Getenv
	}
	if d.Hostname == nil {
		d.Hostname = os.Hostname
	}
	if d.Exec == nil {
		d.Exec = execx.OS{}
	}
	if d.Euid == nil {
		d.Euid = os.Geteuid
	}
	if d.Fetcher == nil {
		d.Fetcher = &download.Fetcher{}
	}
	if d.NewPlatform == nil {
		d.NewPlatform = host.New
	}
	if d.Stdin == nil {
		d.Stdin = strings.NewReader("")
	}
	if d.Stdout == nil {
		d.Stdout = io.Discard
	}
	if d.Stderr == nil {
		d.Stderr = io.Discard
	}
}

func (d *Deps) configDefaults() config.Defaults {
	name, _ := d.Hostname()
	return config.Defaults{Hostname: sanitizeHostname(name), GOOS: d.GOOS}
}

// sanitizeHostname makes a hostname usable as a runner name ("Macs-Mini.local" stays; a
// name with spaces or other characters is cleaned).
func sanitizeHostname(h string) string {
	var b strings.Builder
	for _, r := range h {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('-')
		}
	}
	return strings.TrimLeft(b.String(), "-_.")
}

const usage = `bladerunner - turn this machine into a self-hosted GitHub Actions runner

Usage: bladerunner <command> [flags]

Commands:
  init      write bladerunner.yaml and store the GitHub token
  apply     install or update the runner and its service to match the config (--dry-run first)
  doctor    check prerequisites, token, registration and service health
  remove    deregister the runner and delete everything it installed
  version   print the version

Run "bladerunner <command> -h" for a command's flags.
`

// Run executes one command line and returns the process exit code.
func Run(ctx context.Context, args []string, d Deps) int {
	d.fill()
	if len(args) == 0 {
		fmt.Fprint(d.Stderr, usage)
		return ExitUsage
	}
	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "init":
		err = cmdInit(ctx, rest, &d)
	case "apply":
		err = cmdApply(ctx, rest, &d)
	case "doctor":
		err = cmdDoctor(ctx, rest, &d)
	case "remove":
		err = cmdRemove(ctx, rest, &d)
	case "version", "--version":
		fmt.Fprintln(d.Stdout, "bladerunner", d.Version)
		return ExitOK
	case "help", "-h", "--help":
		fmt.Fprint(d.Stdout, usage)
		return ExitOK
	default:
		fmt.Fprintf(d.Stderr, "bladerunner: unknown command %q\n\n%s", cmd, usage)
		return ExitUsage
	}
	return exitFor(err, d.Stderr)
}

// errFailed marks a command that already printed its own findings (doctor) and only needs
// a non-zero exit.
var errFailed = errors.New("failed")

// usageError marks bad flags or missing arguments.
type usageError struct{ error }

func exitFor(err error, stderr io.Writer) int {
	var ue usageError
	switch {
	case err == nil:
		return ExitOK
	case errors.Is(err, flag.ErrHelp):
		return ExitOK
	case errors.Is(err, errFailed):
		return ExitFailure
	case errors.As(err, &ue):
		fmt.Fprintln(stderr, "bladerunner:", ue.error)
		return ExitUsage
	}
	var de *diag.Error
	if errors.As(err, &de) {
		fmt.Fprintln(stderr, err)
	} else {
		fmt.Fprintln(stderr, "error:", err)
	}
	return ExitFailure
}

// newFlagSet builds a flag set that reports errors through Run rather than exiting.
func newFlagSet(name string, d *Deps) *flag.FlagSet {
	fs := flag.NewFlagSet("bladerunner "+name, flag.ContinueOnError)
	fs.SetOutput(d.Stderr)
	return fs
}

func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return usageError{err}
	}
	if fs.NArg() > 0 {
		return usageError{fmt.Errorf("unexpected argument %q", fs.Arg(0))}
	}
	return nil
}

func configFlag(fs *flag.FlagSet) *string {
	p := fs.String("config", "bladerunner.yaml", "path to the config file")
	fs.StringVar(p, "c", "bladerunner.yaml", "shorthand for -config")
	return p
}

func (d *Deps) bladeHome() string {
	if h := d.Getenv("BLADERUNNER_HOME"); h != "" {
		return h
	}
	return filepath.Join(d.UserHome, ".bladerunner")
}

func (d *Deps) user() host.User { return host.User{Name: d.UserName, UID: d.UID, Home: d.UserHome} }

// newEnv wires the collaborators for a loaded config.
func newEnv(d *Deps, cfg *config.Config) (*install.Env, error) {
	layout := install.Layout{Home: d.bladeHome(), RunnerName: cfg.Runner.Name}
	store, err := secrets.New(cfg.Runner.Token.Source, secrets.Options{Exec: d.Exec, Dir: layout.SecretsDir(), Getenv: d.Getenv})
	if err != nil {
		return nil, err
	}
	plat, err := d.NewPlatform(d.GOOS, d.Exec, d.user())
	if err != nil {
		return nil, err
	}
	name := cfg.Runner.Name
	return &install.Env{
		Cfg: cfg, Layout: layout, UserHome: d.UserHome, GOOS: d.GOOS, GOARCH: d.GOARCH,
		Provider: d.githubClient(func(ctx context.Context) (string, error) { return store.Get(ctx, name) }),
		Platform: plat,
		Secrets:  store,
		Exec:     d.Exec,
		Fetcher:  d.Fetcher,
		State:    &core.StateStore{Path: layout.StateFile()},
		Euid:     d.Euid,
	}, nil
}

func (d *Deps) githubClient(token func(context.Context) (string, error)) *github.Client {
	return &github.Client{APIURL: d.GitHubAPIURL, WebURL: d.GitHubWebURL, Token: token}
}

// loadEnv loads the config and builds the environment: the start of apply, doctor, remove.
func loadEnv(d *Deps, path string) (*install.Env, error) {
	cfg, err := config.Load(path, d.configDefaults())
	if err != nil {
		return nil, err
	}
	return newEnv(d, cfg)
}
