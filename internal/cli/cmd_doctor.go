package cli

import (
	"context"
	"fmt"

	"github.com/modullar/blade-runner/internal/doctor"
)

// cmdDoctor prints every finding and exits non-zero if any check failed.
func cmdDoctor(ctx context.Context, args []string, d *Deps) error {
	fs := newFlagSet("doctor", d)
	cfgPath := configFlag(fs)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	env, err := loadEnv(d, *cfgPath)
	if err != nil {
		return err
	}
	results := doctor.Run(ctx, env, doctor.Options{CLIVersion: d.Version, FreeBytes: d.FreeBytes})
	for _, r := range results {
		fmt.Fprintf(d.Stdout, "%-5s %-18s %s", r.Level, r.ID, r.Message)
		if r.Code != "" {
			fmt.Fprintf(d.Stdout, " [%s]", r.Code)
		}
		fmt.Fprintln(d.Stdout)
		if r.Fix != "" {
			fmt.Fprintf(d.Stdout, "      fix:  %s\n", r.Fix)
		}
		if docs := r.Docs(); docs != "" {
			fmt.Fprintf(d.Stdout, "      docs: %s\n", docs)
		}
	}
	if doctor.Failed(results) {
		fmt.Fprintln(d.Stdout, "\ndoctor found problems.")
		return errFailed
	}
	fmt.Fprintln(d.Stdout, "\nall checks passed.")
	return nil
}
