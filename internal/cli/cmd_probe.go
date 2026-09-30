package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/probe"
	"github.com/modullar/blade-runner/internal/secrets"
)

// cmdProbe runs the BR-0 checks against the real GitHub. It writes nothing on this machine,
// needs no config file, and prints a report with no secrets in it.
func cmdProbe(ctx context.Context, args []string, d *Deps) error {
	if len(args) == 0 || args[0] != "github" {
		return usageError{fmt.Errorf("probe: expected `probe github --repository OWNER/REPO`")}
	}
	fs := newFlagSet("probe github", d)
	repo := fs.String("repository", "", "OWNER/REPO to check")
	skipJIT := fs.Bool("skip-jit", false, "do not register (and delete) a temporary just-in-time runner")
	stdin := fs.Bool("token-stdin", false, "read the GitHub token from standard input (default: the BLADERUNNER_TOKEN environment variable)")
	if err := parseFlags(fs, args[1:]); err != nil {
		return err
	}
	if !strings.Contains(*repo, "/") {
		return usageError{fmt.Errorf("probe: --repository OWNER/REPO is required")}
	}
	token := strings.TrimSpace(d.Getenv(secrets.EnvVar))
	if *stdin {
		data, err := io.ReadAll(io.LimitReader(d.Stdin, 4096))
		if err != nil {
			return err
		}
		token = strings.TrimSpace(string(data))
	}
	if err := secrets.ValidateToken(token); err != nil {
		return diag.Wrap(err, diag.CodeTokenMissing, "no usable GitHub token for the probe",
			"none was given, or it is malformed", "export "+secrets.EnvVar+"=<token>, or pipe it with --token-stdin")
	}
	client := d.githubClient(func(context.Context) (string, error) { return token, nil })

	fmt.Fprintf(d.Stdout, "BR-0 probe of %s\nThis machine is not changed and no runner is installed. The report below contains no secrets.\n\n", *repo)
	findings := probe.Run(ctx, client, probe.Options{Repository: *repo, SkipJIT: *skipJIT})
	fmt.Fprint(d.Stdout, probe.Render(findings))
	if probe.Failed(findings) {
		fmt.Fprintln(d.Stdout, "\nAt least one assumption failed: share this report, it says what to change.")
		return errFailed
	}
	fmt.Fprintln(d.Stdout, "\nNo assumption failed. SKIP and NOTE lines say what is still unknown.")
	return nil
}
