package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/modullar/blade-runner/internal/admit"
	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/trust"
)

const trustUsage = `usage: bladerunner trust <command>

  add --name NAME --key FILE|- [--expires YYYY-MM-DD]
        give someone permission to run code here, by trusting their PUBLIC ssh-ed25519 key
  list  show who has permission, and whether each is active, expired or revoked
  revoke NAME|FINGERPRINT
        take the permission back; the record is kept and the key is never trusted again
  verify --sha SHA [--repository OWNER/REPO]
        ask GitHub for a commit and show whether this machine would admit it, and why
`

// cmdTrust manages the keys whose signed commits may run on this machine. Permission is
// exactly a public key in this store: the owner adds one, and revokes it to withdraw it.
func cmdTrust(ctx context.Context, args []string, d *Deps) error {
	if len(args) == 0 {
		fmt.Fprint(d.Stderr, trustUsage)
		return usageError{fmt.Errorf("trust: expected add, list, revoke or verify")}
	}
	sub, rest := args[0], args[1:]
	fs := newFlagSet("trust "+sub, d)
	cfgPath := configFlag(fs)
	switch sub {
	case "add":
		name := fs.String("name", "", "a short name for this signer, such as their GitHub login")
		keyArg := fs.String("key", "", "their public key file (an .pub file), or - to read it from standard input")
		expires := fs.String("expires", "", "optional: the permission ends on this date (YYYY-MM-DD)")
		if err := parseFlags(fs, rest); err != nil {
			return err
		}
		return trustAdd(d, *cfgPath, *name, *keyArg, *expires)
	case "list":
		if err := parseFlags(fs, rest); err != nil {
			return err
		}
		return trustList(d, *cfgPath)
	case "revoke":
		positional, err := parseInterspersed(fs, rest)
		if err != nil {
			return err
		}
		if len(positional) != 1 {
			return usageError{fmt.Errorf("trust revoke: expected one name or fingerprint")}
		}
		return trustRevoke(d, *cfgPath, positional[0])
	case "verify":
		sha := fs.String("sha", "", "the full commit id to check")
		repo := fs.String("repository", "", "OWNER/REPO (default: the configured repository)")
		if err := parseFlags(fs, rest); err != nil {
			return err
		}
		return trustVerify(ctx, d, *cfgPath, *sha, *repo)
	}
	fmt.Fprint(d.Stderr, trustUsage)
	return usageError{fmt.Errorf("trust: unknown command %q", sub)}
}

// parseInterspersed parses flags that may come before or after positional arguments
// (`revoke alice -c file` as well as `revoke -c file alice`); Go's flag package alone stops at
// the first positional argument.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, usageError{err}
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func trustAdd(d *Deps, cfgPath, name, keyArg, expires string) error {
	if name == "" || keyArg == "" {
		return usageError{fmt.Errorf("trust add: --name and --key are required")}
	}
	env, err := loadEnv(d, cfgPath)
	if err != nil {
		return err
	}
	var data []byte
	if keyArg == "-" {
		data, err = io.ReadAll(io.LimitReader(d.Stdin, 8192))
	} else {
		data, err = os.ReadFile(keyArg)
	}
	if err != nil {
		return diag.Wrap(err, diag.CodeTrustStore, "cannot read the key", "the file is missing or unreadable", "pass the path of their .pub file, or - to pipe it in")
	}
	key, err := trust.ParsePublicKey(string(data))
	if err != nil {
		return diag.Wrap(err, diag.CodeTrustStore, "that is not a usable public key",
			"only SSH ed25519 PUBLIC keys are supported (the file ends in .pub and starts with ssh-ed25519); never send a private key",
			"ask them for the output of: cat ~/.ssh/id_ed25519.pub")
	}
	var until time.Time
	if expires != "" {
		until, err = time.Parse("2006-01-02", expires)
		if err != nil {
			return usageError{fmt.Errorf("--expires %q is not a YYYY-MM-DD date", expires)}
		}
	}
	signer, err := env.Trust.Add(name, key, until)
	if err != nil {
		return err
	}
	fmt.Fprintf(d.Stdout, "%s may now run code here: commits signed with key %s are admitted", signer.Name, signer.Fingerprint)
	if signer.ExpiresAt != nil {
		fmt.Fprintf(d.Stdout, " until %s", signer.ExpiresAt.Format("2006-01-02"))
	}
	fmt.Fprintln(d.Stdout, ".\nThey sign with:  git config gpg.format ssh && git config user.signingkey <their private key> && git commit -S")
	return nil
}

func trustList(d *Deps, cfgPath string) error {
	env, err := loadEnv(d, cfgPath)
	if err != nil {
		return err
	}
	signers, err := env.Trust.Load()
	if err != nil {
		return err
	}
	if len(signers) == 0 {
		fmt.Fprintln(d.Stdout, "nobody has permission to run code here yet: add a key with `bladerunner trust add`")
		return nil
	}
	now := env.Trust.Clock()
	w := tabwriter.NewWriter(d.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATUS\tKEY\tEXPIRES")
	for _, s := range signers {
		exp := "never"
		if s.ExpiresAt != nil {
			exp = s.ExpiresAt.Format("2006-01-02")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", s.Name, s.Status(now), s.Fingerprint, exp)
	}
	return w.Flush()
}

func trustRevoke(d *Deps, cfgPath, who string) error {
	env, err := loadEnv(d, cfgPath)
	if err != nil {
		return err
	}
	s, err := env.Trust.Revoke(who)
	if err != nil {
		return err
	}
	fmt.Fprintf(d.Stdout, "%s (%s) no longer has permission: commits signed with that key are refused, and it can never be trusted again.\n", s.Name, s.Fingerprint)
	return nil
}

func trustVerify(ctx context.Context, d *Deps, cfgPath, sha, repo string) error {
	if sha == "" {
		return usageError{fmt.Errorf("trust verify: --sha is required")}
	}
	env, err := loadEnv(d, cfgPath)
	if err != nil {
		return err
	}
	if repo == "" {
		repo = env.Cfg.Runner.Repository
	}
	if repo == "" {
		return usageError{fmt.Errorf("trust verify: --repository is required for an organization-scope runner")}
	}
	a := &admit.Admitter{Provider: env.Provider, Verifier: &trust.Verifier{Store: env.Trust}}
	v, err := a.Admit(ctx, admit.Subject{Repository: repo, SHA: strings.TrimSpace(sha)})
	if err != nil {
		return err
	}
	fmt.Fprintf(d.Stdout, "admitted: %s in %s is signed by %s (%s)\n", sha[:min(12, len(sha))], repo, v.Signer.Name, v.Signer.Fingerprint)
	return nil
}
