package cli

import (
	"bufio"
	"context"
	"fmt"
	"strings"

	"github.com/modullar/blade-runner/internal/core"
	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/install"
)

// cmdRemove deregisters the runner and deletes what apply installed. It never deletes the
// project's workflow files or bladerunner.yaml.
func cmdRemove(ctx context.Context, args []string, d *Deps) error {
	fs := newFlagSet("remove", d)
	cfgPath := configFlag(fs)
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	skip := fs.Bool("skip-deregister", false, "leave the runner registered on GitHub (for a machine that cannot reach it); delete it in GitHub's settings afterwards")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	env, err := loadEnv(d, *cfgPath)
	if err != nil {
		return err
	}
	env.SkipDeregister = *skip
	name := env.Cfg.Runner.Name

	if !*yes {
		if !d.IsTerminal {
			return diag.New(diag.CodeConfirmRequired, "remove needs confirmation",
				"it deletes the runner, its service and its local state, and no terminal is attached to ask",
				"re-run with --yes to confirm")
		}
		fmt.Fprintf(d.Stderr, "This deregisters runner %q from %s and deletes its service, files, work directory and stored token.\n"+
			"bladerunner.yaml and your workflow files are kept.\nType the runner name to confirm: ", name, env.Cfg.Runner.Target())
		line, _ := bufio.NewReader(d.Stdin).ReadString('\n')
		if strings.TrimSpace(line) != name {
			return diag.New(diag.CodeConfirmRequired, "remove was not confirmed", "the name typed did not match "+name, "run `bladerunner remove` again and type "+name)
		}
	}

	unlock, err := core.LockDir(env.Layout.Home)
	if err != nil {
		return err
	}
	defer unlock()
	fmt.Fprintf(d.Stdout, "removing runner %s:\n", name)
	changed, err := install.RemoveEngine(env, printer{d.Stdout}).Apply(ctx)
	if err != nil {
		return err
	}
	if len(changed) == 0 {
		fmt.Fprintln(d.Stdout, "nothing to remove")
	} else {
		fmt.Fprintf(d.Stdout, "done: %d step(s). Your workflow files were not touched; edit them if they still target this runner.\n", len(changed))
	}
	return nil
}
