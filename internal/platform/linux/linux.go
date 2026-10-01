// Package linux runs the runner as a systemd *user* unit, so nothing needs root. A user unit
// stops at logout unless lingering is enabled for the user, so Install enables it.
//
// Everything here drives systemctl and loginctl through execx. It is unit-tested against
// fakes and has NOT yet run against a real systemd user manager (BR-2 gate, Linux CI job).
package linux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/execx"
	"github.com/modullar/blade-runner/internal/platform"
)

// Platform is the Linux implementation.
type Platform struct {
	Exec execx.Runner
	User string // login name, for loginctl enable-linger
	Home string
}

var _ platform.Platform = (*Platform)(nil)

func (*Platform) OS() string { return "linux" }

// UnitName is the systemd unit for a runner name.
func UnitName(runnerName string) string { return "bladerunner-runner-" + runnerName + ".service" }

func (p *Platform) DefinitionPath(s platform.Spec) string {
	return filepath.Join(p.Home, ".config", "systemd", "user", UnitName(s.RunnerName))
}

type unitData struct {
	RunnerName, RunScript, RunnerDir, Home string
}

// Render refuses paths systemd would split or expand (spaces, %, $, quotes): a unit that
// silently runs the wrong thing is worse than an error.
func (p *Platform) Render(s platform.Spec) ([]byte, error) {
	for _, path := range []string{s.RunScript(), s.RunnerDir, s.Home} {
		if strings.ContainsAny(path, " \t\n%$\"'\\") {
			return nil, diag.New(diag.CodeServiceInstall,
				fmt.Sprintf("path %q cannot be used in a systemd unit", path),
				"it contains a space or one of % $ quotes backslash, which systemd would split or expand",
				"set BLADERUNNER_HOME (or use a home directory) without such characters, then re-run")
		}
	}
	return platform.RenderTemplate("systemd.service.tmpl", unitData{
		RunnerName: s.RunnerName, RunScript: s.RunScript(), RunnerDir: s.RunnerDir, Home: s.Home,
	})
}

func (p *Platform) ctl(ctx context.Context, args ...string) (execx.Result, error) {
	return p.Exec.Run(ctx, execx.Cmd{Name: "systemctl", Args: append([]string{"--user"}, args...)})
}

func (p *Platform) CheckPrereqs(ctx context.Context) []platform.Prereq {
	var out []platform.Prereq
	add := func(name string, ok bool, detail, fix string) {
		out = append(out, platform.Prereq{Name: name, OK: ok, Detail: detail, Fix: fix})
	}
	for _, tool := range []string{"git", "systemctl", "loginctl"} {
		_, err := p.Exec.LookPath(tool)
		fix := "install " + tool + " with your package manager"
		if tool == "systemctl" || tool == "loginctl" {
			fix = "Blade Runner needs systemd on Linux in v1; " + tool + " was not found"
		}
		add(tool, err == nil, found(err), fix)
	}
	_, err := p.ctl(ctx, "show-environment")
	add("systemd user manager", err == nil, found(err),
		"log in through a normal session (ssh counts) so /run/user/$UID exists, or run `sudo loginctl enable-linger $USER` and re-login")
	return out
}

func found(err error) string {
	if err == nil {
		return "found"
	}
	return "missing"
}

func (p *Platform) Status(ctx context.Context, s platform.Spec) (platform.Status, error) {
	want, err := p.Render(s)
	if err != nil {
		return platform.Status{}, err
	}
	st := platform.Status{}
	st.Installed, st.Current = platform.DefinitionStatus(p.DefinitionPath(s), want)
	res, err := p.ctl(ctx, "is-active", UnitName(s.RunnerName))
	state := strings.TrimSpace(res.Stdout)
	if err != nil {
		var ee *execx.ExitError
		if !errors.As(err, &ee) { // could not even run systemctl
			return st, err
		}
		if state == "" {
			state = "inactive"
		}
	}
	st.Running = state == "active"
	st.Detail = "systemd state: " + state
	return st, nil
}

func (p *Platform) lingerEnabled(ctx context.Context) bool {
	res, err := p.Exec.Run(ctx, execx.Cmd{Name: "loginctl", Args: []string{"show-user", p.User, "--property=Linger"}})
	return err == nil && strings.TrimSpace(res.Stdout) == "Linger=yes"
}

func (p *Platform) Install(ctx context.Context, s platform.Spec) error {
	content, err := p.Render(s)
	if err != nil {
		return err
	}
	changed, err := platform.WriteDefinition(p.DefinitionPath(s), content)
	if err != nil {
		return installErr(err, "cannot write "+p.DefinitionPath(s), "check the permissions of ~/.config/systemd/user")
	}
	if changed {
		if _, err := p.ctl(ctx, "daemon-reload"); err != nil {
			return installErr(err, "systemd would not reload its units", "check `systemctl --user status`")
		}
	}
	unit := UnitName(s.RunnerName)
	if _, err := p.ctl(ctx, "is-enabled", unit); err != nil {
		if _, err := p.ctl(ctx, "enable", unit); err != nil {
			return installErr(err, "systemd would not enable "+unit, "check `systemctl --user status`")
		}
	}
	if !p.lingerEnabled(ctx) {
		if _, err := p.Exec.Run(ctx, execx.Cmd{Name: "loginctl", Args: []string{"enable-linger", p.User}}); err != nil {
			return installErr(err, "cannot enable lingering, so the runner would stop when you log out",
				fmt.Sprintf("run once with administrator rights: sudo loginctl enable-linger %s, then re-run `bladerunner apply`", p.User))
		}
	}
	return nil
}

func (p *Platform) Start(ctx context.Context, s platform.Spec) error {
	if _, err := p.ctl(ctx, "start", UnitName(s.RunnerName)); err != nil {
		return installErr(err, "systemd would not start the runner", "see `bladerunner logs` and `systemctl --user status "+UnitName(s.RunnerName)+"`")
	}
	return nil
}

func (p *Platform) Stop(ctx context.Context, s platform.Spec) error {
	if _, err := p.ctl(ctx, "stop", UnitName(s.RunnerName)); err != nil {
		// Stopping a unit that does not exist is fine: there is nothing to stop.
		if st, statusErr := p.Status(ctx, s); statusErr == nil && !st.Running && !st.Installed {
			return nil
		}
		return installErr(err, "systemd would not stop the runner", "see `systemctl --user status "+UnitName(s.RunnerName)+"`")
	}
	return nil
}

func (p *Platform) Uninstall(ctx context.Context, s platform.Spec) error {
	unit := UnitName(s.RunnerName)
	if err := p.Stop(ctx, s); err != nil {
		return err
	}
	installed := false
	if _, err := os.Stat(p.DefinitionPath(s)); err == nil {
		installed = true
	}
	if installed {
		_, _ = p.ctl(ctx, "disable", unit) // disabling a unit that is not enabled is harmless
	}
	if err := os.Remove(p.DefinitionPath(s)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return installErr(err, "cannot remove "+p.DefinitionPath(s), "delete it by hand")
	}
	if installed {
		if _, err := p.ctl(ctx, "daemon-reload"); err != nil {
			return installErr(err, "systemd would not reload its units", "run `systemctl --user daemon-reload`")
		}
	}
	return nil
}

func (p *Platform) Logs(ctx context.Context, s platform.Spec, follow bool, out io.Writer) error {
	args := []string{"--user-unit", UnitName(s.RunnerName), "-n", "200", "--no-pager"}
	if follow {
		args = append(args, "-f")
	}
	_, err := p.Exec.Run(ctx, execx.Cmd{Name: "journalctl", Args: args, Stdout: out})
	if err != nil && ctx.Err() == nil {
		return diag.Wrap(err, diag.CodeServiceNotRunning, "cannot read the runner log",
			"journalctl is missing or the unit has never run", "run `bladerunner apply`, then try again")
	}
	return nil
}

func installErr(err error, what, fix string) error {
	return diag.Wrap(err, diag.CodeServiceInstall, what, "the systemd user manager refused the operation", fix)
}
