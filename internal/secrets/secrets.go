// Package secrets stores and retrieves the GitHub token without it ever touching
// bladerunner.yaml, logs, generated files or a command line (spec sections 4.1 and 10).
package secrets

import (
	"context"
	"fmt"
	"os"
	"strings"
	"unicode"

	"github.com/modullar/blade-runner/internal/config"
	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/execx"
)

// EnvVar is the variable the env backend reads.
const EnvVar = "BLADERUNNER_TOKEN"

// Store reads and writes one secret per key. Keys are runner names, so two runners on one
// machine (a side-by-side migration, say) keep separate tokens.
type Store interface {
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, value string) error
	Delete(ctx context.Context, key string) error
	// Source names the backend as it appears in config (keychain, env, file).
	Source() string
}

// Options are the collaborators the backends need.
type Options struct {
	Exec   execx.Runner
	Dir    string              // file backend: directory holding the token files
	Getenv func(string) string // env backend; defaults to os.Getenv
}

// New returns the backend for a config token source.
func New(source string, o Options) (Store, error) {
	switch source {
	case config.TokenKeychain:
		if o.Exec == nil {
			return nil, fmt.Errorf("keychain backend needs a command runner")
		}
		return &Keychain{Exec: o.Exec}, nil
	case config.TokenFile:
		if o.Dir == "" {
			return nil, fmt.Errorf("file backend needs a directory")
		}
		return &File{Dir: o.Dir}, nil
	case config.TokenEnv:
		g := o.Getenv
		if g == nil {
			g = os.Getenv
		}
		return &Env{Getenv: g}, nil
	}
	return nil, fmt.Errorf("unknown token source %q", source)
}

// ValidateToken rejects values that cannot be a token: empty, or containing whitespace,
// quotes or control characters (which would also break the keychain's command syntax).
func ValidateToken(t string) error {
	if t == "" {
		return fmt.Errorf("the token is empty")
	}
	for _, r := range t {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == '"' || r == '\'' || r == '\\' {
			return fmt.Errorf("the token contains whitespace, quotes or control characters: check it was pasted whole")
		}
	}
	return nil
}

func notFound(source, key, fix string) *diag.Error {
	return diag.New(diag.CodeTokenMissing,
		fmt.Sprintf("no GitHub token found for runner %q (token source: %s)", key, source),
		"the token was never stored on this machine, or was stored for a different runner name", fix)
}

func storeFailed(err error, source, what, fix string) *diag.Error {
	return diag.Wrap(err, diag.CodeTokenStoreFailed, what,
		fmt.Sprintf("the %s token backend refused the operation", source), fix)
}

func cleanToken(s string) string { return strings.TrimSpace(s) }
