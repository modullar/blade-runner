package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/modullar/blade-runner/internal/hook"
)

// cmdHook is what the runner calls before every job (`bladerunner hook job-started`). It exits
// non-zero, and so fails the job before any step runs, unless the policy allows it. It is not
// a command for people: the generated hook script calls it.
func cmdHook(_ context.Context, args []string, d *Deps) error {
	if len(args) == 0 || args[0] != "job-started" {
		return usageError{fmt.Errorf("hook: expected `job-started`")}
	}
	fs := newFlagSet("hook job-started", d)
	policyPath := fs.String("policy", "", "path to the job policy file")
	if err := parseFlags(fs, args[1:]); err != nil {
		return err
	}
	refuse := func(reason string) error {
		fmt.Fprintf(d.Stderr, "bladerunner: REFUSED this job: %s\n", reason)
		return errFailed
	}
	if *policyPath == "" {
		return refuse("no policy file was given to the hook")
	}
	policy, err := hook.LoadPolicy(*policyPath)
	if err != nil {
		return refuse("the job policy cannot be read (" + err.Error() + ")")
	}
	env := hook.EnvMap(d.environ())
	decision := hook.Evaluate(policy, env, os.ReadFile)
	if !decision.Allow {
		return refuse(decision.Reason)
	}
	fmt.Fprintf(d.Stdout, "bladerunner: allowed: %s\n", decision.Reason)
	return nil
}
