package secrets

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/execx"
)

var ctx = context.Background()

func TestFileBackendRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")
	s, err := New("file", Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "mini"); diag.CodeOf(err) != diag.CodeTokenMissing {
		t.Fatalf("Get before Set: %v, want BR-E020", err)
	}
	if err := s.Set(ctx, "mini", "ghp_abc123"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Get(ctx, "mini"); err != nil || got != "ghp_abc123" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	di, _ := os.Stat(dir)
	fi, _ := os.Stat(filepath.Join(dir, "mini.token"))
	if di.Mode().Perm() != 0o700 || fi.Mode().Perm() != 0o600 {
		t.Errorf("dir mode %v, file mode %v, want 0700 and 0600", di.Mode().Perm(), fi.Mode().Perm())
	}
	if err := s.Set(ctx, "mini", "ghp_new"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(ctx, "mini"); got != "ghp_new" {
		t.Errorf("overwrite gave %q", got)
	}
	if err := s.Delete(ctx, "mini"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "mini"); err != nil {
		t.Errorf("deleting twice must be fine: %v", err)
	}
	if _, err := s.Get(ctx, "mini"); diag.CodeOf(err) != diag.CodeTokenMissing {
		t.Errorf("Get after Delete: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".token-*")); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
}

func TestFileBackendRefusesLooseModeAndBadKeys(t *testing.T) {
	dir := t.TempDir()
	s := &File{Dir: dir}
	p := filepath.Join(dir, "mini.token")
	if err := os.WriteFile(p, []byte("ghp_x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := s.Get(ctx, "mini")
	if diag.CodeOf(err) != diag.CodeTokenStoreFailed || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("world-readable token file: %v, want BR-E023 with a chmod fix", err)
	}
	for _, key := range []string{"", "../escape", "a/b", ".hidden"} {
		if _, err := s.Get(ctx, key); err == nil {
			t.Errorf("Get(%q) should be refused", key)
		}
		if err := s.Set(ctx, key, "ghp_x"); err == nil {
			t.Errorf("Set(%q) should be refused", key)
		}
	}
}

func TestValidateTokenRefusesUnstorableValues(t *testing.T) {
	for _, bad := range []string{"", "has space", "tab\there", "new\nline", `qu"ote`, "qu'ote", `back\slash`} {
		if ValidateToken(bad) == nil {
			t.Errorf("ValidateToken(%q) should fail", bad)
		}
		f := &File{Dir: t.TempDir()}
		if err := f.Set(ctx, "k", bad); diag.CodeOf(err) != diag.CodeTokenStoreFailed {
			t.Errorf("Set(%q) = %v, want BR-E023", bad, err)
		}
	}
	if err := ValidateToken("github_pat_11ABC_def"); err != nil {
		t.Errorf("a real-looking token must pass: %v", err)
	}
}

func TestEnvBackend(t *testing.T) {
	env := map[string]string{}
	s, _ := New("env", Options{Getenv: func(k string) string { return env[k] }})
	if _, err := s.Get(ctx, "mini"); diag.CodeOf(err) != diag.CodeTokenMissing || !strings.Contains(err.Error(), EnvVar) {
		t.Errorf("unset env: %v", err)
	}
	env[EnvVar] = "  ghp_env \n"
	if got, err := s.Get(ctx, "mini"); err != nil || got != "ghp_env" {
		t.Errorf("Get = %q, %v", got, err)
	}
	if err := s.Set(ctx, "mini", "x"); diag.CodeOf(err) != diag.CodeTokenStoreFailed {
		t.Errorf("Set on env must explain it cannot store: %v", err)
	}
	if err := s.Delete(ctx, "mini"); err != nil {
		t.Errorf("Delete on env is a no-op: %v", err)
	}
}

func TestKeychainSetKeepsTokenOutOfArguments(t *testing.T) {
	const token = "ghp_supersecret"
	f := &execx.Fake{}
	k := &Keychain{Exec: f}
	if err := k.Set(ctx, "mini", token); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("calls = %v", f.Lines())
	}
	c := f.Calls[0]
	if c.Name != "security" || len(c.Args) != 1 || c.Args[0] != "-i" {
		t.Errorf("command = %v, want `security -i`", f.Line(0))
	}
	for _, a := range append([]string{c.Name}, c.Args...) {
		if strings.Contains(a, token) {
			t.Errorf("token appears in an argument: %q", a)
		}
	}
	if want := "add-generic-password -U -s bladerunner -a mini -w " + token + "\n"; c.Stdin != want {
		t.Errorf("stdin = %q, want %q", c.Stdin, want)
	}
}

func TestKeychainGetAndDelete(t *testing.T) {
	f := &execx.Fake{Handler: func(c execx.Cmd) (execx.Result, error) {
		switch c.Args[0] {
		case "find-generic-password":
			if c.Args[4] == "missing" {
				return execx.Fail("security", 44, "could not be found")
			}
			if c.Args[4] == "locked" {
				return execx.Fail("security", 36, "User interaction is not allowed")
			}
			return execx.Result{Stdout: "ghp_kc\n"}, nil
		case "delete-generic-password":
			if c.Args[4] == "missing" {
				return execx.Fail("security", 44, "could not be found")
			}
			if c.Args[4] == "locked" {
				return execx.Fail("security", 36, "denied")
			}
		}
		return execx.Result{}, nil
	}}
	k := &Keychain{Exec: f}
	if got, err := k.Get(ctx, "mini"); err != nil || got != "ghp_kc" {
		t.Errorf("Get = %q, %v", got, err)
	}
	if _, err := k.Get(ctx, "missing"); diag.CodeOf(err) != diag.CodeTokenMissing {
		t.Errorf("exit 44 must map to BR-E020: %v", err)
	}
	if _, err := k.Get(ctx, "locked"); diag.CodeOf(err) != diag.CodeTokenStoreFailed {
		t.Errorf("other failures must map to BR-E023: %v", err)
	}
	if err := k.Delete(ctx, "missing"); err != nil {
		t.Errorf("deleting an absent item is fine: %v", err)
	}
	if err := k.Delete(ctx, "locked"); diag.CodeOf(err) != diag.CodeTokenStoreFailed {
		t.Errorf("Delete failure: %v", err)
	}
	if err := k.Set(ctx, `bad key`, "ghp_x"); err == nil {
		t.Error("a key with spaces must be refused (it would break the command line)")
	}
}

func TestNewRejectsUnknownSourceAndMissingCollaborators(t *testing.T) {
	if _, err := New("vault", Options{}); err == nil {
		t.Error("unknown source must error")
	}
	if _, err := New("keychain", Options{}); err == nil {
		t.Error("keychain without an exec runner must error")
	}
	if _, err := New("file", Options{}); err == nil {
		t.Error("file without a dir must error")
	}
}
