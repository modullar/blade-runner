package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modullar/blade-runner/internal/cli"
	"github.com/modullar/blade-runner/internal/core"
	"github.com/modullar/blade-runner/internal/download"
	"github.com/modullar/blade-runner/internal/execx"
	"github.com/modullar/blade-runner/internal/platform"
	"github.com/modullar/blade-runner/internal/platform/host"
	"github.com/modullar/blade-runner/internal/provider/github/githubtest"
	"github.com/modullar/blade-runner/internal/testrig"
)

// cliRig runs the real command line against the fake GitHub, with real files and a real
// config.sh process; only the service manager is faked.
type cliRig struct {
	t        *testing.T
	srv      *githubtest.Server
	plat     *testrig.FakePlatform
	userHome string
	cfgPath  string
	out, err bytes.Buffer

	// per-run inputs
	terminal  bool
	secret    string
	deps      cli.Deps
	freeBytes uint64
}

func newCLIRig(t *testing.T) *cliRig {
	t.Helper()
	srv := githubtest.New()
	t.Cleanup(srv.Close)
	c := &cliRig{
		t: t, srv: srv,
		plat:      &testrig.FakePlatform{Prereqs: []platform.Prereq{{Name: "git", OK: true}}},
		userHome:  t.TempDir(),
		cfgPath:   filepath.Join(t.TempDir(), "bladerunner.yaml"),
		freeBytes: 100 << 30,
	}
	return c
}

func (c *cliRig) run(stdin string, args ...string) int {
	c.t.Helper()
	c.out.Reset()
	c.err.Reset()
	d := cli.Deps{
		GOOS: "linux", GOARCH: "amd64",
		UserHome: c.userHome, UserName: "dev", UID: 1000,
		Getenv:       func(string) string { return "" },
		Hostname:     func() (string, error) { return "e2e-host", nil },
		Exec:         execx.OS{},
		Euid:         func() int { return 1000 },
		GitHubAPIURL: c.srv.URL, GitHubWebURL: c.srv.URL,
		Fetcher:     &download.Fetcher{AllowHTTP: true},
		NewPlatform: func(string, execx.Runner, host.User) (platform.Platform, error) { return c.plat, nil },
		FreeBytes:   func(string) (uint64, error) { return c.freeBytes, nil },
		Stdin:       strings.NewReader(stdin), Stdout: &c.out, Stderr: &c.err,
		IsTerminal: c.terminal,
		ReadSecret: func() (string, error) { return c.secret, nil },
		Version:    "0.1.0",
		BinaryPath: testrig.Binary(c.t),
	}
	if c.deps.GOOS != "" {
		d.GOOS = c.deps.GOOS
		d.NewPlatform = nil // exercise the real platform selection
	}
	return cli.Run(context.Background(), args, d)
}

func (c *cliRig) mustRun(stdin string, args ...string) {
	c.t.Helper()
	if code := c.run(stdin, args...); code != 0 {
		c.t.Fatalf("bladerunner %v exited %d\nstdout:\n%s\nstderr:\n%s", args, code, c.out.String(), c.err.String())
	}
}

func (c *cliRig) initArgs(extra ...string) []string {
	return append([]string{"init", "-c", c.cfgPath, "--scope", "repo", "--repository", "acme/widgets",
		"--name", "e2e-runner", "--labels", "gpu", "--token-stdin"}, extra...)
}

// stepLines counts the output lines that report a step with the given outcome.
func (c *cliRig) stepLines(outcome string) int {
	n := 0
	for _, l := range strings.Split(c.out.String(), "\n") {
		if strings.HasPrefix(l, "  "+outcome+" ") {
			n++
		}
	}
	return n
}

func (c *cliRig) home() string { return filepath.Join(c.userHome, ".bladerunner") }

func TestPrimaryFlowInitApplyDoctorRemove(t *testing.T) {
	c := newCLIRig(t)

	// init: writes the config, stores the token outside it, verifies the token with GitHub.
	c.mustRun(testrig.Token+"\n", c.initArgs()...)
	cfgText, err := os.ReadFile(c.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"scope: repo", "repository: acme/widgets", "name: e2e-runner", "labels: [gpu]", "min_version: \"0.1.0\""} {
		if !strings.Contains(string(cfgText), want) {
			t.Errorf("config is missing %q:\n%s", want, cfgText)
		}
	}
	if strings.Contains(string(cfgText), testrig.Token) {
		t.Fatal("the token was written into bladerunner.yaml")
	}
	tokenFile := filepath.Join(c.home(), "secrets", "e2e-runner.token")
	if info, err := os.Stat(tokenFile); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("token file: %v %v, want a 0600 file", info, err)
	}
	if !strings.Contains(c.out.String(), "GitHub accepted the token") {
		t.Errorf("init output:\n%s", c.out.String())
	}

	// apply --dry-run: lists every step, changes nothing.
	c.mustRun("", "apply", "-c", c.cfgPath, "--dry-run")
	if n := c.stepLines("would change"); n != 6 {
		t.Errorf("dry-run lists %d pending steps, want 6:\n%s", n, c.out.String())
	}
	if _, err := os.Stat(filepath.Join(c.home(), "runners")); err == nil {
		t.Error("dry-run created the runner directory")
	}
	if len(c.srv.Runners("acme/widgets")) != 0 {
		t.Error("dry-run registered a runner")
	}

	// apply: the runner comes online, and the real run matches the dry-run's prediction.
	c.mustRun("", "apply", "-c", c.cfgPath)
	if n := c.stepLines("changed"); n != 6 {
		t.Errorf("apply reports fewer than 5 changes:\n%s", c.out.String())
	}
	runners := c.srv.Runners("acme/widgets")
	if len(runners) != 1 || runners[0].Name != "e2e-runner" || !runners[0].Online {
		t.Fatalf("GitHub runners = %+v", runners)
	}

	// apply again: nothing changes.
	c.mustRun("", "apply", "-c", c.cfgPath)
	if !strings.Contains(c.out.String(), "already up to date") {
		t.Errorf("second apply should be a no-op:\n%s", c.out.String())
	}
	c.mustRun("", "apply", "-c", c.cfgPath, "--dry-run")
	if !strings.Contains(c.out.String(), "nothing would change") {
		t.Errorf("dry-run after apply:\n%s", c.out.String())
	}

	// doctor: all green.
	c.mustRun("", "doctor", "-c", c.cfgPath)
	if !strings.Contains(c.out.String(), "all checks passed") || strings.Contains(c.out.String(), "FAIL") {
		t.Errorf("doctor output:\n%s", c.out.String())
	}

	// remove: confirmation is required without a terminal...
	if code := c.run("", "remove", "-c", c.cfgPath); code != cli.ExitFailure || !strings.Contains(c.err.String(), "BR-E090") {
		t.Errorf("remove without --yes: exit %d, stderr:\n%s", code, c.err.String())
	}
	if len(c.srv.Runners("acme/widgets")) != 1 {
		t.Fatal("an unconfirmed remove deregistered the runner")
	}
	// ...and with it, nothing is left behind except the project's own config file.
	c.mustRun("", "remove", "-c", c.cfgPath, "--yes")
	if left, _ := os.ReadDir(c.userHome); len(left) != 0 {
		t.Errorf("remove left %d entries in the home directory: %v", len(left), left)
	}
	if len(c.srv.Runners("acme/widgets")) != 0 {
		t.Error("the runner is still registered")
	}
	if _, err := os.Stat(c.cfgPath); err != nil {
		t.Error("remove must never delete the project's bladerunner.yaml")
	}
	c.mustRun("", "remove", "-c", c.cfgPath, "--yes") // idempotent
	if !strings.Contains(c.out.String(), "nothing to remove") {
		t.Errorf("second remove:\n%s", c.out.String())
	}
}

func TestInitFailurePaths(t *testing.T) {
	t.Run("a rejected token writes nothing", func(t *testing.T) {
		c := newCLIRig(t)
		if code := c.run("ghp_wrong\n", c.initArgs()...); code != cli.ExitFailure || !strings.Contains(c.err.String(), "BR-E021") {
			t.Fatalf("exit %d, stderr:\n%s", code, c.err.String())
		}
		if _, err := os.Stat(c.cfgPath); err == nil {
			t.Error("a config was written although the token was rejected")
		}
		if _, err := os.Stat(filepath.Join(c.home(), "secrets")); err == nil {
			t.Error("a rejected token was stored")
		}
		if strings.Contains(c.err.String(), "ghp_wrong") {
			t.Error("the token leaked into the error output")
		}
	})

	t.Run("a public repository is refused without the opt-in flag", func(t *testing.T) {
		c := newCLIRig(t)
		c.srv.Repos["acme/widgets"] = false
		if code := c.run(testrig.Token+"\n", c.initArgs()...); code != cli.ExitFailure || !strings.Contains(c.err.String(), "BR-E060") {
			t.Fatalf("exit %d, stderr:\n%s", code, c.err.String())
		}
		if _, err := os.Stat(c.cfgPath); err == nil {
			t.Error("a config was written for a refused public repository")
		}
	})

	t.Run("the opt-in flag allows a public repository, loudly", func(t *testing.T) {
		c := newCLIRig(t)
		c.srv.Repos["acme/widgets"] = false
		c.mustRun(testrig.Token+"\n", c.initArgs("--allow-public-runner")...)
		if !strings.Contains(c.out.String(), "PUBLIC") {
			t.Errorf("the opt-in must print a warning:\n%s", c.out.String())
		}
	})

	t.Run("an existing config is not overwritten", func(t *testing.T) {
		c := newCLIRig(t)
		c.mustRun(testrig.Token+"\n", c.initArgs()...)
		before, _ := os.ReadFile(c.cfgPath)
		if code := c.run(testrig.Token+"\n", c.initArgs()...); code != cli.ExitFailure || !strings.Contains(c.err.String(), "BR-E003") {
			t.Fatalf("exit %d, stderr:\n%s", code, c.err.String())
		}
		if after, _ := os.ReadFile(c.cfgPath); !bytes.Equal(before, after) {
			t.Error("the existing config was modified")
		}
		c.mustRun(testrig.Token+"\n", c.initArgs("--force", "--labels", "gpu,fast")...)
	})

	t.Run("non-interactive init names the missing flags", func(t *testing.T) {
		c := newCLIRig(t)
		code := c.run("", "init", "-c", c.cfgPath, "--non-interactive")
		if code != cli.ExitFailure || !strings.Contains(c.err.String(), "--repository") || !strings.Contains(c.err.String(), "BR-E001") {
			t.Errorf("exit %d, stderr:\n%s", code, c.err.String())
		}
	})

	t.Run("invalid values are reported with their key path", func(t *testing.T) {
		c := newCLIRig(t)
		code := c.run(testrig.Token+"\n", c.initArgs("--placement-default", "sometimes")...)
		if code != cli.ExitFailure || !strings.Contains(c.err.String(), "placement.default") {
			t.Errorf("exit %d, stderr:\n%s", code, c.err.String())
		}
	})

	t.Run("an unsupported OS is refused", func(t *testing.T) {
		c := newCLIRig(t)
		c.deps.GOOS = "windows"
		if code := c.run(testrig.Token+"\n", c.initArgs()...); code != cli.ExitFailure || !strings.Contains(c.err.String(), "BR-E011") {
			t.Errorf("exit %d, stderr:\n%s", code, c.err.String())
		}
	})

	t.Run("env token source cannot be stored via stdin", func(t *testing.T) {
		c := newCLIRig(t)
		code := c.run(testrig.Token+"\n", c.initArgs("--token-source", "env")...)
		if code != cli.ExitFailure || !strings.Contains(c.err.String(), "BR-E023") {
			t.Errorf("exit %d, stderr:\n%s", code, c.err.String())
		}
	})

	t.Run("a malformed token on stdin is refused", func(t *testing.T) {
		c := newCLIRig(t)
		code := c.run("has spaces in it\n", c.initArgs()...)
		if code != cli.ExitFailure || !strings.Contains(c.err.String(), "BR-E023") {
			t.Errorf("exit %d, stderr:\n%s", code, c.err.String())
		}
	})
}

func TestInteractiveInitPromptsAndHidesTheToken(t *testing.T) {
	c := newCLIRig(t)
	c.terminal = true
	c.secret = testrig.Token
	c.mustRun("repo\nacme/widgets\nmy-runner\nauto\n", "init", "-c", c.cfgPath)

	cfg, _ := os.ReadFile(c.cfgPath)
	if !strings.Contains(string(cfg), "name: my-runner") || !strings.Contains(string(cfg), "repository: acme/widgets") {
		t.Errorf("config from prompts:\n%s", cfg)
	}
	if _, err := os.Stat(filepath.Join(c.home(), "secrets", "my-runner.token")); err != nil {
		t.Errorf("token not stored: %v", err)
	}
	if strings.Contains(c.out.String()+c.err.String(), testrig.Token) {
		t.Error("the token was echoed")
	}
	if !strings.Contains(c.err.String(), "input hidden") {
		t.Errorf("the token prompt should say the input is hidden:\n%s", c.err.String())
	}
}

func TestInitReusesAnAlreadyStoredToken(t *testing.T) {
	c := newCLIRig(t)
	c.mustRun(testrig.Token+"\n", c.initArgs()...)
	c.terminal = true
	c.secret = "must-not-be-asked"
	c.mustRun("", "init", "-c", c.cfgPath, "--force", "--scope", "repo", "--repository", "acme/widgets", "--name", "e2e-runner", "--non-interactive")
	if !strings.Contains(c.out.String(), "using the token already stored") {
		t.Errorf("output:\n%s", c.out.String())
	}
}

func TestApplyFailurePaths(t *testing.T) {
	t.Run("no config", func(t *testing.T) {
		c := newCLIRig(t)
		if code := c.run("", "apply", "-c", c.cfgPath); code != cli.ExitFailure || !strings.Contains(c.err.String(), "BR-E002") || !strings.Contains(c.err.String(), "bladerunner init") {
			t.Errorf("exit %d, stderr:\n%s", code, c.err.String())
		}
	})

	t.Run("a typo in the config is named by key path", func(t *testing.T) {
		c := newCLIRig(t)
		c.mustRun(testrig.Token+"\n", c.initArgs()...)
		cfg, _ := os.ReadFile(c.cfgPath)
		bad := strings.Replace(string(cfg), "  work_dir:", "  wrk_dir:", 1)
		if err := os.WriteFile(c.cfgPath, []byte(bad), 0o644); err != nil {
			t.Fatal(err)
		}
		if code := c.run("", "apply", "-c", c.cfgPath); code != cli.ExitFailure || !strings.Contains(c.err.String(), `runner.wrk_dir: unknown key (did you mean "work_dir"?)`) {
			t.Errorf("exit %d, stderr:\n%s", code, c.err.String())
		}
	})

	t.Run("a missing token is explained", func(t *testing.T) {
		c := newCLIRig(t)
		c.mustRun(testrig.Token+"\n", c.initArgs()...)
		_ = os.Remove(filepath.Join(c.home(), "secrets", "e2e-runner.token"))
		if code := c.run("", "apply", "-c", c.cfgPath); code != cli.ExitFailure || !strings.Contains(c.err.String(), "BR-E020") {
			t.Errorf("exit %d, stderr:\n%s", code, c.err.String())
		}
		if !strings.Contains(c.err.String(), "docs:") || !strings.Contains(c.err.String(), "fix:") || !strings.Contains(c.err.String(), "likely cause:") {
			t.Errorf("an error must say what, why, how to fix and link docs:\n%s", c.err.String())
		}
	})

	t.Run("a public repository is refused even for a dry run", func(t *testing.T) {
		c := newCLIRig(t)
		c.mustRun(testrig.Token+"\n", c.initArgs()...)
		c.srv.Repos["acme/widgets"] = false // the repository was made public after init
		if code := c.run("", "apply", "-c", c.cfgPath, "--dry-run"); code != cli.ExitFailure || !strings.Contains(c.err.String(), "BR-E060") {
			t.Errorf("exit %d, stderr:\n%s", code, c.err.String())
		}
		c.mustRun("", "apply", "-c", c.cfgPath, "--dry-run", "--allow-public-runner")
	})

	t.Run("a tampered download installs nothing", func(t *testing.T) {
		c := newCLIRig(t)
		c.mustRun(testrig.Token+"\n", c.initArgs()...)
		c.srv.ChecksumOverride = strings.Repeat("0", 64)
		if code := c.run("", "apply", "-c", c.cfgPath); code != cli.ExitFailure || !strings.Contains(c.err.String(), "BR-E031") {
			t.Errorf("exit %d, stderr:\n%s", code, c.err.String())
		}
		if len(c.srv.Runners("acme/widgets")) != 0 || c.plat.Installed {
			t.Error("something was installed or registered after a checksum mismatch")
		}
	})

	t.Run("a crash mid-apply is resumed by running apply again", func(t *testing.T) {
		c := newCLIRig(t)
		c.mustRun(testrig.Token+"\n", c.initArgs()...)
		c.plat.FailStartOnce = true
		if code := c.run("", "apply", "-c", c.cfgPath); code != cli.ExitFailure {
			t.Fatalf("expected the start to fail, exit %d", code)
		}
		c.mustRun("", "apply", "-c", c.cfgPath)
		if !c.plat.Running || len(c.srv.Runners("acme/widgets")) != 1 {
			t.Errorf("not converged: running=%v runners=%+v", c.plat.Running, c.srv.Runners("acme/widgets"))
		}
		if c.stepLines("changed") != 1 || !strings.Contains(c.out.String(), "service-running") {
			t.Errorf("the resumed run should change only the last step:\n%s", c.out.String())
		}
	})
}

func TestConcurrentApplyAndRemoveAreRefusedWhileAnotherRuns(t *testing.T) {
	c := newCLIRig(t)
	c.mustRun(testrig.Token+"\n", c.initArgs()...)
	unlock, err := core.LockDir(c.home()) // as if another bladerunner were mid-apply
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"apply", "-c", c.cfgPath}, {"remove", "-c", c.cfgPath, "--yes"}} {
		if code := c.run("", args...); code != cli.ExitFailure || !strings.Contains(c.err.String(), "BR-E004") {
			t.Errorf("%v: exit %d, stderr:\n%s", args, code, c.err.String())
		}
	}
	if len(c.srv.Runners("acme/widgets")) != 0 || c.plat.Installed {
		t.Error("a locked-out command still changed the machine")
	}
	// A dry run only reads, so it is allowed while another command changes things.
	c.mustRun("", "apply", "-c", c.cfgPath, "--dry-run")
	unlock()
	c.mustRun("", "apply", "-c", c.cfgPath)
}

func TestDoctorReportsProblemsWithFixesAndExitsNonZero(t *testing.T) {
	c := newCLIRig(t)
	c.mustRun(testrig.Token+"\n", c.initArgs()...) // initialised but never applied
	code := c.run("", "doctor", "-c", c.cfgPath)
	if code != cli.ExitFailure {
		t.Errorf("exit = %d, want 1", code)
	}
	out := c.out.String()
	for _, want := range []string{"FAIL", "BR-E041", "bladerunner apply", "docs: https://", "doctor found problems"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output missing %q:\n%s", want, out)
		}
	}
}

func TestRemoveConfirmation(t *testing.T) {
	c := newCLIRig(t)
	c.mustRun(testrig.Token+"\n", c.initArgs()...)
	c.mustRun("", "apply", "-c", c.cfgPath)
	c.terminal = true

	if code := c.run("wrong-name\n", "remove", "-c", c.cfgPath); code != cli.ExitFailure || !strings.Contains(c.err.String(), "BR-E090") {
		t.Errorf("a wrong confirmation: exit %d, stderr:\n%s", code, c.err.String())
	}
	if len(c.srv.Runners("acme/widgets")) != 1 {
		t.Fatal("a wrong confirmation deregistered the runner")
	}
	c.mustRun("e2e-runner\n", "remove", "-c", c.cfgPath)
	if len(c.srv.Runners("acme/widgets")) != 0 {
		t.Error("the confirmed remove did not deregister")
	}
}

func TestRemoveSkipDeregisterWorksOffline(t *testing.T) {
	c := newCLIRig(t)
	c.mustRun(testrig.Token+"\n", c.initArgs()...)
	c.mustRun("", "apply", "-c", c.cfgPath)
	c.srv.Down = true
	if code := c.run("", "remove", "-c", c.cfgPath, "--yes"); code != cli.ExitFailure || !strings.Contains(c.err.String(), "BR-E022") {
		t.Fatalf("exit %d, stderr:\n%s", code, c.err.String())
	}
	c.mustRun("", "remove", "-c", c.cfgPath, "--yes", "--skip-deregister")
	if left, _ := os.ReadDir(c.userHome); len(left) != 0 {
		t.Errorf("leftovers: %v", left)
	}
}

func TestUsageErrorsExitTwo(t *testing.T) {
	c := newCLIRig(t)
	for name, args := range map[string][]string{
		"no command":      nil,
		"unknown command": {"frobnicate"},
		"unknown flag":    {"apply", "--nope"},
		"extra argument":  {"apply", "stray"},
	} {
		if code := c.run("", args...); code != cli.ExitUsage {
			t.Errorf("%s: exit %d, want 2 (stderr: %s)", name, code, c.err.String())
		}
	}
	c.mustRun("", "version")
	if strings.TrimSpace(c.out.String()) != "bladerunner 0.1.0" {
		t.Errorf("version output %q", c.out.String())
	}
	c.mustRun("", "help")
	for _, cmd := range []string{"init", "apply", "doctor", "remove"} {
		if !strings.Contains(c.out.String(), cmd) {
			t.Errorf("help does not mention %s", cmd)
		}
	}
	if code := c.run("", "apply", "-h"); code != cli.ExitOK {
		t.Errorf("apply -h exit %d, want 0", code)
	}
}
