package secrets

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/modullar/blade-runner/internal/config"
	"github.com/modullar/blade-runner/internal/execx"
)

// Env reads the token from BLADERUNNER_TOKEN. It cannot store or delete: the environment
// is the user's to manage.
type Env struct{ Getenv func(string) string }

func (*Env) Source() string { return config.TokenEnv }

func (e *Env) Get(_ context.Context, key string) (string, error) {
	if v := cleanToken(e.Getenv(EnvVar)); v != "" {
		return v, nil
	}
	return "", notFound(config.TokenEnv, key, "export "+EnvVar+"=<token> in the shell (and the service's environment) that runs bladerunner")
}

func (*Env) Set(context.Context, string, string) error {
	return storeFailed(nil, config.TokenEnv, "cannot store a token in the environment",
		"export "+EnvVar+"=<token> yourself, or choose another runner.token.source")
}

func (*Env) Delete(context.Context, string) error { return nil }

// File keeps the token in a 0600 file inside a 0700 directory.
type File struct{ Dir string }

func (*File) Source() string { return config.TokenFile }

func (f *File) path(key string) (string, error) {
	if key == "" || key != filepath.Base(key) || strings.HasPrefix(key, ".") {
		return "", fmt.Errorf("invalid secret key %q", key)
	}
	return filepath.Join(f.Dir, key+".token"), nil
}

func (f *File) Get(_ context.Context, key string) (string, error) {
	p, err := f.path(key)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return "", notFound(config.TokenFile, key, "run `bladerunner init` again and supply the token, or `bladerunner init --token-stdin < token.txt`")
	}
	if err != nil {
		return "", storeFailed(err, config.TokenFile, "cannot read the token file", "check "+p)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", storeFailed(nil, config.TokenFile,
			fmt.Sprintf("token file %s is readable by other users (mode %o)", p, info.Mode().Perm()),
			fmt.Sprintf("chmod 600 %q", p))
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", storeFailed(err, config.TokenFile, "cannot read the token file", "check "+p)
	}
	return cleanToken(string(data)), nil
}

func (f *File) Set(_ context.Context, key, value string) error {
	if err := ValidateToken(value); err != nil {
		return storeFailed(err, config.TokenFile, "refusing to store the token", "paste the whole token again")
	}
	p, err := f.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(f.Dir, 0o700); err != nil {
		return storeFailed(err, config.TokenFile, "cannot create the secrets directory", "check "+f.Dir)
	}
	tmp, err := os.CreateTemp(f.Dir, ".token-*.tmp")
	if err != nil {
		return storeFailed(err, config.TokenFile, "cannot write the token file", "check "+f.Dir)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return storeFailed(err, config.TokenFile, "cannot protect the token file", "check "+f.Dir)
	}
	if _, err := tmp.WriteString(value + "\n"); err != nil {
		tmp.Close()
		return storeFailed(err, config.TokenFile, "cannot write the token file", "check the disk space in "+f.Dir)
	}
	if err := tmp.Close(); err != nil {
		return storeFailed(err, config.TokenFile, "cannot write the token file", "check the disk space in "+f.Dir)
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return storeFailed(err, config.TokenFile, "cannot write the token file", "check "+f.Dir)
	}
	return nil
}

func (f *File) Delete(_ context.Context, key string) error {
	p, err := f.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return storeFailed(err, config.TokenFile, "cannot delete the token file", "delete "+p+" by hand")
	}
	return nil
}

// Keychain uses the macOS login keychain through the `security` tool.
//
// Reads use `find-generic-password -w`, which prints only the value. Writes go through
// `security -i` (commands on stdin) so the token never appears in a process argument list.
// Both behaviors are assumptions until BR-0/BR-2 run on a real Mac (spec A6).
type Keychain struct{ Exec execx.Runner }

const (
	keychainService = "bladerunner"
	secItemNotFound = 44 // errSecItemNotFound as `security` exits
)

func (*Keychain) Source() string { return config.TokenKeychain }

func (k *Keychain) Get(ctx context.Context, key string) (string, error) {
	res, err := k.Exec.Run(ctx, execx.Cmd{Name: "security", Args: []string{"find-generic-password", "-s", keychainService, "-a", key, "-w"}})
	if err != nil {
		if res.ExitCode == secItemNotFound {
			return "", notFound(config.TokenKeychain, key, "run `bladerunner init` again and supply the token")
		}
		return "", storeFailed(err, config.TokenKeychain, "cannot read the keychain", "unlock the login keychain, or allow access when macOS asks")
	}
	return cleanToken(res.Stdout), nil
}

func (k *Keychain) Set(ctx context.Context, key, value string) error {
	if err := ValidateToken(value); err != nil {
		return storeFailed(err, config.TokenKeychain, "refusing to store the token", "paste the whole token again")
	}
	if strings.ContainsAny(key, " \"'\\\n") {
		return fmt.Errorf("invalid secret key %q", key)
	}
	cmd := fmt.Sprintf("add-generic-password -U -s %s -a %s -w %s\n", keychainService, key, value)
	if _, err := k.Exec.Run(ctx, execx.Cmd{Name: "security", Args: []string{"-i"}, Stdin: cmd}); err != nil {
		return storeFailed(err, config.TokenKeychain, "cannot write the keychain", "unlock the login keychain, or use runner.token.source: file")
	}
	return nil
}

func (k *Keychain) Delete(ctx context.Context, key string) error {
	res, err := k.Exec.Run(ctx, execx.Cmd{Name: "security", Args: []string{"delete-generic-password", "-s", keychainService, "-a", key}})
	if err != nil && res.ExitCode != secItemNotFound {
		return storeFailed(err, config.TokenKeychain, "cannot delete the keychain item", fmt.Sprintf("security delete-generic-password -s %s -a %s", keychainService, key))
	}
	return nil
}
