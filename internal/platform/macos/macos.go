// Package macos runs the runner as a per-user LaunchAgent. A LaunchAgent (not a
// LaunchDaemon) lives in the user's GUI session, so jobs that need it (code signing,
// simulators) work; the price is that the user must be logged in, which for an unattended
// Mac means enabling auto-login (assumption A6, verified in BR-0 on a real Mac).
//
// Everything here drives `launchctl` through execx. It is unit-tested against a fake
// launchctl and has NOT yet run on a real Mac.
package macos

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/execx"
	"github.com/modullar/blade-runner/internal/platform"
)

// Platform is the macOS implementation.
type Platform struct {
	Exec execx.Runner
	UID  int    // the invoking user's uid: launchd domain gui/<uid>
	Home string // the invoking user's home directory

	// UnloadWait bounds how long Stop waits for launchd to let go of a service (default
	// 30s); Sleep and Now exist so tests need not really wait.
	UnloadWait time.Duration
	Sleep      func(time.Duration)
	Now        func() time.Time
}

const defaultUnloadWait = 30 * time.Second

const labelPrefix = "dev.bladerunner.runner." // placeholder reverse-DNS: rename with the product

var _ platform.Platform = (*Platform)(nil)

func (*Platform) OS() string { return "darwin" }

// Label is the launchd label for a runner name.
func Label(runnerName string) string { return labelPrefix + runnerName }

func (p *Platform) domain() string { return fmt.Sprintf("gui/%d", p.UID) }

func (p *Platform) target(s platform.Spec) string { return p.domain() + "/" + Label(s.RunnerName) }

func (p *Platform) DefinitionPath(s platform.Spec) string {
	return filepath.Join(p.Home, "Library", "LaunchAgents", Label(s.RunnerName)+".plist")
}

type plistData struct {
	Label, RunScript, RunnerDir, LogFile, Path, Home string
}

func (p *Platform) Render(s platform.Spec) ([]byte, error) {
	return platform.RenderTemplate("launchd.plist.tmpl", plistData{
		Label:     Label(s.RunnerName),
		RunScript: s.RunScript(),
		RunnerDir: s.RunnerDir,
		LogFile:   filepath.Join(s.LogDir, "runner.log"),
		Path:      "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		Home:      s.Home,
	})
}

func (p *Platform) CheckPrereqs(ctx context.Context) []platform.Prereq {
	var out []platform.Prereq
	add := func(name string, ok bool, detail, fix string) {
		out = append(out, platform.Prereq{Name: name, OK: ok, Detail: detail, Fix: fix})
	}
	_, err := p.Exec.LookPath("git")
	add("git", err == nil, found(err), "install Xcode Command Line Tools: xcode-select --install")
	_, err = p.Exec.LookPath("launchctl")
	add("launchctl", err == nil, found(err), "launchctl ships with macOS; this does not look like a Mac")
	_, err = p.Exec.Run(ctx, execx.Cmd{Name: "xcode-select", Args: []string{"-p"}})
	add("Xcode Command Line Tools", err == nil, found(err), "xcode-select --install")
	_, err = p.Exec.Run(ctx, execx.Cmd{Name: "launchctl", Args: []string{"print", p.domain()}})
	add("GUI login session", err == nil, found(err),
		"log in to the Mac's desktop as this user; for an unattended Mac also enable automatic login (System Settings > Users & Groups)")
	return out
}

func found(err error) string {
	if err == nil {
		return "found"
	}
	return "missing"
}

func (p *Platform) loaded(ctx context.Context, s platform.Spec) (bool, string, error) {
	res, err := p.Exec.Run(ctx, execx.Cmd{Name: "launchctl", Args: []string{"print", p.target(s)}})
	if err != nil {
		var ee *execx.ExitError
		if errors.As(err, &ee) { // launchctl ran and says it is not loaded
			return false, "", nil
		}
		return false, "", err
	}
	return true, res.Stdout, nil
}

func (p *Platform) Status(ctx context.Context, s platform.Spec) (platform.Status, error) {
	want, err := p.Render(s)
	if err != nil {
		return platform.Status{}, err
	}
	st := platform.Status{}
	st.Installed, st.Current = platform.DefinitionStatus(p.DefinitionPath(s), want)
	loaded, out, err := p.loaded(ctx, s)
	if err != nil {
		return st, err
	}
	switch {
	case !loaded:
		st.Detail = "not loaded in launchd"
	default:
		state := launchdState(out)
		st.Running = state == "running"
		st.Detail = "loaded, state = " + state
	}
	return st, nil
}

// launchdState extracts "state = running" from `launchctl print` output.
func launchdState(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "state = "); ok {
			return v
		}
	}
	return "unknown"
}

func (p *Platform) Install(ctx context.Context, s platform.Spec) error {
	content, err := p.Render(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.LogDir, 0o755); err != nil {
		return installErr(err, "cannot create the log directory "+s.LogDir)
	}
	changed, err := platform.WriteDefinition(p.DefinitionPath(s), content)
	if err != nil {
		return installErr(err, "cannot write "+p.DefinitionPath(s))
	}
	if !changed {
		return nil
	}
	// launchd reads the plist at bootstrap, so a changed definition needs the old one
	// unloaded; Start bootstraps it again.
	if loaded, _, err := p.loaded(ctx, s); err == nil && loaded {
		if err := p.unload(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// unload boots the service out and waits until launchd has really let go of it.
// `launchctl bootout` can return while the service is still loaded, and bootstrapping the
// same label in that window fails with "Bootstrap failed: 5: Input/output error". The
// forge CI supervisor this code was modeled on hit exactly that, and waits the same way.
func (p *Platform) unload(ctx context.Context, s platform.Spec) error {
	if _, err := p.Exec.Run(ctx, execx.Cmd{Name: "launchctl", Args: []string{"bootout", p.target(s)}}); err != nil {
		// Already gone is fine; anything else is decided by whether it is still loaded.
		if loaded, _, qerr := p.loaded(ctx, s); qerr == nil && !loaded {
			return nil
		}
		return installErr(err, "launchd would not stop the runner")
	}
	wait := p.UnloadWait
	if wait == 0 {
		wait = defaultUnloadWait
	}
	now, sleep := p.Now, p.Sleep
	if now == nil {
		now = time.Now
	}
	if sleep == nil {
		sleep = time.Sleep
	}
	deadline := now().Add(wait)
	for {
		loaded, _, err := p.loaded(ctx, s)
		if err != nil {
			return installErr(err, "cannot query launchd")
		}
		if !loaded {
			return nil
		}
		if !now().Before(deadline) {
			return installErr(fmt.Errorf("still loaded %s after launchctl bootout", wait),
				"launchd did not let go of the runner in time")
		}
		sleep(500 * time.Millisecond)
	}
}

func (p *Platform) Start(ctx context.Context, s platform.Spec) error {
	loaded, out, err := p.loaded(ctx, s)
	if err != nil {
		return installErr(err, "cannot query launchd")
	}
	var cmd execx.Cmd
	switch {
	case loaded && launchdState(out) == "running":
		return nil
	case loaded:
		cmd = execx.Cmd{Name: "launchctl", Args: []string{"kickstart", p.target(s)}}
	default:
		cmd = execx.Cmd{Name: "launchctl", Args: []string{"bootstrap", p.domain(), p.DefinitionPath(s)}}
	}
	if _, err := p.Exec.Run(ctx, cmd); err != nil {
		return installErr(err, "launchd would not start the runner")
	}
	return nil
}

func (p *Platform) Stop(ctx context.Context, s platform.Spec) error {
	loaded, _, err := p.loaded(ctx, s)
	if err != nil {
		return installErr(err, "cannot query launchd")
	}
	if !loaded {
		return nil
	}
	return p.unload(ctx, s)
}

func (p *Platform) Uninstall(ctx context.Context, s platform.Spec) error {
	if err := p.Stop(ctx, s); err != nil {
		return err
	}
	if err := os.Remove(p.DefinitionPath(s)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return installErr(err, "cannot remove "+p.DefinitionPath(s))
	}
	return nil
}

func (p *Platform) Logs(ctx context.Context, s platform.Spec, follow bool, out io.Writer) error {
	args := []string{"-n", "200"}
	if follow {
		args = append(args, "-f")
	}
	args = append(args, filepath.Join(s.LogDir, "runner.log"))
	_, err := p.Exec.Run(ctx, execx.Cmd{Name: "tail", Args: args, Stdout: out})
	if err != nil && ctx.Err() == nil {
		return diag.Wrap(err, diag.CodeServiceNotRunning, "cannot read the runner log",
			"the runner has not written a log yet", "run `bladerunner apply`, then try again")
	}
	return nil
}

func installErr(err error, what string) error {
	return diag.Wrap(err, diag.CodeServiceInstall, what,
		"launchd refused the operation: the GUI login session is missing, or the plist is invalid",
		"log in to the Mac's desktop as this user, then re-run `bladerunner apply`")
}
