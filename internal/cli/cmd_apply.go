package cli

import (
	"context"
	"fmt"

	"github.com/modullar/blade-runner/internal/core"
	"github.com/modullar/blade-runner/internal/install"
)

// cmdApply converges the machine to bladerunner.yaml. --dry-run shows what would change.
func cmdApply(ctx context.Context, args []string, d *Deps) error {
	fs := newFlagSet("apply", d)
	cfgPath := configFlag(fs)
	dry := fs.Bool("dry-run", false, "show what would change, and change nothing")
	allowPublic := fs.Bool("allow-public-runner", false, "register a runner to a PUBLIC repository (fork pull requests can then run code on this machine)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	env, err := loadEnv(d, *cfgPath)
	if err != nil {
		return err
	}
	env.AllowPublic = *allowPublic
	engine := install.ApplyEngine(env, printer{d.Stdout})

	if *dry {
		fmt.Fprintf(d.Stdout, "dry run for runner %s (%s):\n", env.Cfg.Runner.Name, env.Cfg.Runner.Target())
		plan, err := engine.DryRun(ctx)
		if err != nil {
			return err
		}
		if n := len(plan.Pending()); n == 0 {
			fmt.Fprintln(d.Stdout, "nothing would change")
		} else {
			fmt.Fprintf(d.Stdout, "%d step(s) would change. Run without --dry-run to apply.\n", n)
		}
		return nil
	}

	unlock, err := core.LockDir(env.Layout.Home)
	if err != nil {
		return err
	}
	defer unlock()
	fmt.Fprintf(d.Stdout, "applying for runner %s (%s):\n", env.Cfg.Runner.Name, env.Cfg.Runner.Target())
	changed, err := engine.Apply(ctx)
	if err != nil {
		return err
	}
	if len(changed) == 0 {
		fmt.Fprintln(d.Stdout, "already up to date: nothing changed")
	} else {
		fmt.Fprintf(d.Stdout, "done: %d step(s) changed. Check health with `bladerunner doctor`.\n", len(changed))
	}
	return nil
}
