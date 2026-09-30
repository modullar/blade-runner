package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/modullar/blade-runner/internal/config"
	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/install"
	"github.com/modullar/blade-runner/internal/provider"
	"github.com/modullar/blade-runner/internal/secrets"
	"github.com/modullar/blade-runner/internal/version"
)

// cmdInit writes bladerunner.yaml and stores the token. It never writes a secret into the
// file, and it refuses a public repository unless the user opts in explicitly.
func cmdInit(ctx context.Context, args []string, d *Deps) error {
	fs := newFlagSet("init", d)
	out := configFlag(fs)
	scope := fs.String("scope", "", "repo or org")
	repo := fs.String("repository", "", "OWNER/REPO (scope repo)")
	org := fs.String("organization", "", "organization (scope org)")
	name := fs.String("name", "", "runner name (default: this machine's hostname)")
	labels := fs.String("labels", "", "extra runner labels, comma separated")
	tokenSource := fs.String("token-source", "", "where the token lives: keychain, file or env (default: keychain on macOS, file on Linux)")
	placement := fs.String("placement-default", "", "default job placement: local, github or auto (default: auto)")
	workDir := fs.String("work-dir", "", "job work directory (default: ~/.bladerunner/work/<name>)")
	allowPublic := fs.Bool("allow-public-runner", false, "allow a runner for a PUBLIC repository (fork pull requests can then run code on this machine)")
	nonInteractive := fs.Bool("non-interactive", false, "never prompt: take everything from flags")
	tokenStdin := fs.Bool("token-stdin", false, "read the GitHub token from standard input")
	force := fs.Bool("force", false, "overwrite an existing config file")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	if _, err := os.Stat(*out); err == nil && !*force {
		return diag.New(diag.CodeConfigExists, *out+" already exists",
			"this project is already set up, and init will not overwrite your edits",
			"edit the file, or re-run with --force to replace it")
	}
	if _, err := d.NewPlatform(d.GOOS, d.Exec, d.user()); err != nil {
		return err
	}
	fmt.Fprintf(d.Stdout, "detected %s/%s\n", d.GOOS, d.GOARCH)

	in := &asker{r: bufio.NewReader(d.Stdin), w: d.Stderr, interactive: d.IsTerminal && !*nonInteractive && !*tokenStdin}
	var missing []string
	scopeV := in.value(*scope, "Runner scope (repo or org)", "repo", true, "--scope", &missing)
	var repoV, orgV string
	switch scopeV {
	case config.ScopeOrg:
		orgV = in.value(*org, "GitHub organization", "", true, "--organization", &missing)
	default:
		repoV = in.value(*repo, "Repository (OWNER/REPO)", "", true, "--repository", &missing)
	}
	if len(missing) > 0 {
		return diag.New(diag.CodeConfigInvalid, "init needs more information",
			"running without a terminal, these were not given: "+strings.Join(missing, ", "),
			"pass them as flags, or run `bladerunner init` in a terminal")
	}
	nameV := in.value(*name, "Runner name", d.configDefaults().Hostname, false, "", nil)
	placementV := in.value(*placement, "Default placement (local, github or auto)", "auto", false, "", nil)

	cfg := &config.Config{
		Version: 1,
		Runner: config.Runner{
			Scope: scopeV, Repository: repoV, Organization: orgV, Name: nameV,
			Labels: splitList(*labels), WorkDir: *workDir,
			Token: config.Token{Source: *tokenSource},
		},
		Placement: config.PlacementConfig{Default: config.Placement(placementV)},
	}
	if v, err := version.Parse(d.Version); err == nil {
		cfg.BladeRunner.MinVersion = fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2])
	}
	if err := config.Finalize(cfg, d.configDefaults()); err != nil {
		return err
	}
	env, err := newEnv(d, cfg)
	if err != nil {
		return err
	}

	token, err := gatherToken(ctx, d, env, *tokenStdin, in)
	if err != nil {
		return err
	}
	if token != "" {
		// Check the token before storing or trusting it: a rejected token is reported now.
		tok := token
		probe := d.githubClient(func(context.Context) (string, error) { return tok, nil })
		if err := probe.CheckAuth(ctx, env.Scope()); err != nil {
			return err
		}
		fmt.Fprintf(d.Stdout, "GitHub accepted the token for %s\n", cfg.Runner.Target())
		if stored, err := env.Secrets.Get(ctx, cfg.Runner.Name); err != nil || stored != token {
			if err := env.Secrets.Set(ctx, cfg.Runner.Name, token); err != nil {
				return err
			}
			fmt.Fprintf(d.Stdout, "token stored (%s); it is not written to %s\n", env.Secrets.Source(), *out)
		}
	}

	if cfg.Runner.Scope == config.ScopeRepo {
		if err := checkVisibility(ctx, d, cfg, token, *allowPublic); err != nil {
			return err
		}
	}

	rendered := config.Render(cfg)
	if _, err := config.Parse(rendered, d.configDefaults()); err != nil {
		return fmt.Errorf("internal error: the generated config does not parse: %w", err)
	}
	if err := os.WriteFile(*out, rendered, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(d.Stdout, "wrote %s\n\nNext:\n  bladerunner apply --dry-run   # see what would change\n  bladerunner apply             # install the runner\n  bladerunner doctor            # check it is healthy\n", *out)
	return nil
}

// gatherToken finds the token to use: standard input, a token already stored for this
// runner, or a hidden prompt. It returns "" (with guidance printed) when none is available.
func gatherToken(ctx context.Context, d *Deps, env *install.Env, fromStdin bool, in *asker) (string, error) {
	source := env.Cfg.Runner.Token.Source
	switch {
	case fromStdin:
		if source == config.TokenEnv {
			return "", diag.New(diag.CodeTokenStoreFailed, "cannot store a token in the environment",
				"--token-stdin stores the token, and the env source has nowhere to store it",
				"export "+secrets.EnvVar+"=<token> instead, or use --token-source file|keychain")
		}
		data, err := io.ReadAll(io.LimitReader(d.Stdin, 4096))
		if err != nil {
			return "", err
		}
		tok := strings.TrimSpace(string(data))
		if err := secrets.ValidateToken(tok); err != nil {
			return "", diag.Wrap(err, diag.CodeTokenStoreFailed, "the token on standard input is not usable", "it is empty or malformed", "pipe the whole token: bladerunner init --token-stdin < token.txt")
		}
		return tok, nil
	}
	if existing, err := env.Secrets.Get(ctx, env.Cfg.Runner.Name); err == nil {
		fmt.Fprintf(d.Stdout, "using the token already stored (%s)\n", source)
		return existing, nil
	} else if source == config.TokenEnv {
		fmt.Fprintf(d.Stdout, "no token yet: export %s=<token> before `bladerunner apply`\n", secrets.EnvVar)
		return "", nil
	} else if diag.CodeOf(err) != diag.CodeTokenMissing {
		return "", err
	}
	if in.interactive && d.ReadSecret != nil {
		fmt.Fprintf(d.Stderr, "GitHub token with permission to manage runners (input hidden): ")
		tok, err := d.ReadSecret()
		if err != nil {
			return "", diag.Wrap(err, diag.CodeTokenStoreFailed, "cannot read the token without echoing it",
				"this terminal does not support hidden input", "use: bladerunner init --token-stdin < token.txt")
		}
		if err := secrets.ValidateToken(tok); err != nil {
			return "", diag.Wrap(err, diag.CodeTokenStoreFailed, "the token is not usable", "it is empty or malformed", "paste the whole token")
		}
		return tok, nil
	}
	fmt.Fprintf(d.Stdout, "no token stored yet: run `bladerunner init --force --token-stdin < token.txt`, or re-run init in a terminal\n")
	return "", nil
}

// checkVisibility enforces the public-repository guard at init time. When visibility cannot
// be determined it warns and continues: apply checks again and fails closed.
func checkVisibility(ctx context.Context, d *Deps, cfg *config.Config, token string, allow bool) error {
	var tf func(context.Context) (string, error)
	if token != "" {
		tf = func(context.Context) (string, error) { return token, nil }
	}
	vis, err := d.githubClient(tf).Visibility(ctx, cfg.Runner.Repository)
	if err != nil {
		reason := err.Error()
		var de *diag.Error
		if errors.As(err, &de) {
			reason = de.Code + ": " + de.What
		}
		fmt.Fprintf(d.Stdout, "warning: could not tell whether %s is public (%s); `bladerunner apply` will check again and refuses to continue if it cannot\n",
			cfg.Runner.Repository, reason)
		return nil
	}
	if err := install.PublicRepoDecision(cfg.Runner.Repository, vis, allow); err != nil {
		return err
	}
	if vis == provider.Public {
		fmt.Fprintf(d.Stdout, "warning: %s is PUBLIC and you opted in: every contributor's pull request can run code on this machine\n", cfg.Runner.Repository)
	}
	return nil
}

// asker reads values from flags or, when interactive, from prompts.
type asker struct {
	r           *bufio.Reader
	w           io.Writer
	interactive bool
}

// value returns flagVal if set; else prompts (interactive) or records the flag as missing
// (non-interactive, required); else returns the default.
func (a *asker) value(flagVal, prompt, def string, required bool, flagName string, missing *[]string) string {
	if flagVal != "" {
		return flagVal
	}
	if a.interactive {
		if def != "" {
			fmt.Fprintf(a.w, "%s [%s]: ", prompt, def)
		} else {
			fmt.Fprintf(a.w, "%s: ", prompt)
		}
		line, _ := a.r.ReadString('\n')
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
		return def
	}
	if required && def == "" && missing != nil {
		*missing = append(*missing, flagName)
	}
	return def
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
