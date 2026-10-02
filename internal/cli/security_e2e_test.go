package cli_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modullar/blade-runner/internal/cli"
	"github.com/modullar/blade-runner/internal/provider"
	"github.com/modullar/blade-runner/internal/testrig"
)

// projectDir is where the config (and so the project's workflows and checkout) lives.
func (c *cliRig) projectDir() string { return filepath.Dir(c.cfgPath) }

func (c *cliRig) writeWorkflow(name, body string) {
	c.t.Helper()
	dir := filepath.Join(c.projectDir(), ".github", "workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		c.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		c.t.Fatal(err)
	}
}

func (c *cliRig) gitInit(remote string) {
	c.t.Helper()
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", remote}} {
		if out, err := exec.Command("git", append([]string{"-C", c.projectDir()}, args...)...).CombinedOutput(); err != nil {
			c.t.Skipf("git unusable: %s", out)
		}
	}
}

func (c *cliRig) assertUntouched() {
	c.t.Helper()
	if len(c.srv.Runners("acme/widgets")) != 0 || c.plat.Installed {
		c.t.Error("the machine or GitHub was changed although the run was refused")
	}
	if _, err := os.Stat(filepath.Join(c.home(), "runners")); err == nil {
		c.t.Error("runner files were created although the run was refused")
	}
}

// The scenario the security requirement is about: a stranger's pull request must not be able
// to run on this machine, however the repository is set up.
func TestStrangersCodeCannotReachTheRunner(t *testing.T) {
	c := newCLIRig(t)
	c.mustRun(testrig.Token+"\n", c.initArgs()...) // the repository owner is "acme": the only trusted actor

	// An open workflow (any pull request would run on the runner): refused, with the fix.
	c.writeWorkflow("ci.yml", testrig.OpenWorkflow)
	for _, args := range [][]string{{"apply", "-c", c.cfgPath, "--dry-run"}, {"apply", "-c", c.cfgPath}} {
		if code := c.run("", args...); code != cli.ExitFailure || !strings.Contains(c.err.String(), "BR-E062") {
			t.Fatalf("%v: exit %d, stderr:\n%s", args, code, c.err.String())
		}
		for _, want := range []string{"ci.yml", "job test", "if: github.actor == 'acme'", "fork"} {
			if !strings.Contains(c.err.String(), want) {
				t.Errorf("%v: the refusal should contain %q:\n%s", args, want, c.err.String())
			}
		}
		c.assertUntouched()
	}

	// The same workflow on a trigger that strangers can fire is refused even with the guard.
	c.writeWorkflow("ci.yml", strings.Replace(testrig.GuardedWorkflow, "on: [push, pull_request]", "on: [push, issue_comment]", 1))
	if code := c.run("", "apply", "-c", c.cfgPath); code != cli.ExitFailure || !strings.Contains(c.err.String(), "issue_comment") {
		t.Fatalf("exit %d, stderr:\n%s", code, c.err.String())
	}
	c.assertUntouched()

	// Locked to the owner: apply proceeds and says what it verified.
	c.writeWorkflow("ci.yml", testrig.GuardedWorkflow)
	c.mustRun("", "apply", "-c", c.cfgPath)
	if !strings.Contains(c.out.String(), "all are locked to acme") {
		t.Errorf("apply should report what it verified:\n%s", c.out.String())
	}
	if len(c.srv.Runners("acme/widgets")) != 1 {
		t.Error("the runner was not registered")
	}

	// A new, unguarded workflow appears later: doctor notices.
	c.writeWorkflow("deploy.yml", strings.Replace(testrig.OpenWorkflow, "name: ci", "name: deploy", 1))
	if code := c.run("", "doctor", "-c", c.cfgPath); code != cli.ExitFailure {
		t.Errorf("doctor exit %d, want 1", code)
	}
	if out := c.out.String(); !strings.Contains(out, "workflow-policy") || !strings.Contains(out, "BR-E062") || !strings.Contains(out, "deploy.yml") {
		t.Errorf("doctor should flag the new workflow:\n%s", out)
	}
}

func TestAForkMustSetUpItsOwnRunner(t *testing.T) {
	c := newCLIRig(t)
	c.mustRun(testrig.Token+"\n", c.initArgs()...) // a config for acme/widgets, as committed by its owner
	c.gitInit(c.srv.URL + "/mallory/widgets.git")  // ...now in mallory's fork

	for _, args := range [][]string{{"apply", "-c", c.cfgPath, "--dry-run"}, {"apply", "-c", c.cfgPath}} {
		if code := c.run("", args...); code != cli.ExitFailure || !strings.Contains(c.err.String(), "BR-E063") {
			t.Fatalf("%v: exit %d, stderr:\n%s", args, code, c.err.String())
		}
		if !strings.Contains(c.err.String(), "bladerunner init --force --repository mallory/widgets") {
			t.Errorf("the fork's owner needs to be told what to do:\n%s", c.err.String())
		}
		c.assertUntouched()
	}

	// What the fork owner does: their own init for their own repository, with their token.
	c.srv.Repos["mallory/widgets"] = true
	c.mustRun(testrig.Token+"\n", "init", "-c", c.cfgPath, "--force", "--scope", "repo", "--repository", "mallory/widgets",
		"--name", "mallory-runner", "--token-stdin")
	cfg, _ := os.ReadFile(c.cfgPath)
	if !strings.Contains(string(cfg), "trusted_actors: [mallory]") {
		t.Errorf("the fork's config must trust only the fork's owner, not acme:\n%s", cfg)
	}
	c.mustRun("", "apply", "-c", c.cfgPath)
	if len(c.srv.Runners("mallory/widgets")) != 1 || len(c.srv.Runners("acme/widgets")) != 0 {
		t.Errorf("the fork registered a runner in the wrong place: mallory=%v acme=%v", c.srv.Runners("mallory/widgets"), c.srv.Runners("acme/widgets"))
	}
}

func TestAllowRepoMismatchFlag(t *testing.T) {
	c := newCLIRig(t)
	c.mustRun(testrig.Token+"\n", c.initArgs()...)
	c.gitInit(c.srv.URL + "/mallory/widgets.git")
	c.mustRun("", "apply", "-c", c.cfgPath, "--allow-repo-mismatch")
	if !strings.Contains(c.out.String(), "--allow-repo-mismatch") {
		t.Errorf("the override must be announced:\n%s", c.out.String())
	}
}

func TestPublicRepositoryNeedsEveryLayer(t *testing.T) {
	c := newCLIRig(t)
	c.srv.Repos["acme/widgets"] = false // public
	c.mustRun(testrig.Token+"\n", c.initArgs("--allow-public-runner")...)
	c.writeWorkflow("ci.yml", testrig.GuardedWorkflow)

	// 1. no opt-in flag
	if code := c.run("", "apply", "-c", c.cfgPath); code != cli.ExitFailure || !strings.Contains(c.err.String(), "BR-E060") {
		t.Fatalf("without --allow-public-runner: exit %d, stderr:\n%s", code, c.err.String())
	}
	// 2. opt-in, but outside contributors' runs do not need approval
	c.srv.ForkApproval = map[string]string{"acme/widgets": "first_time_contributors"}
	if code := c.run("", "apply", "-c", c.cfgPath, "--allow-public-runner"); code != cli.ExitFailure || !strings.Contains(c.err.String(), "BR-E064") {
		t.Fatalf("weak approval policy: exit %d, stderr:\n%s", code, c.err.String())
	}
	c.assertUntouched()
	// 3. the API cannot tell: the user must confirm by hand
	c.srv.ForkApproval = nil
	c.srv.ForkApprovalUnsupported = true
	if code := c.run("", "apply", "-c", c.cfgPath, "--allow-public-runner"); code != cli.ExitFailure || !strings.Contains(c.err.String(), "--fork-approval-confirmed") {
		t.Fatalf("unverifiable: exit %d, stderr:\n%s", code, c.err.String())
	}
	c.assertUntouched()
	// 4. everything in place
	c.mustRun("", "apply", "-c", c.cfgPath, "--allow-public-runner", "--fork-approval-confirmed")
	if len(c.srv.Runners("acme/widgets")) != 1 {
		t.Error("with every layer satisfied the runner should be registered")
	}
}

func TestInitRecordsWhoIsTrusted(t *testing.T) {
	t.Run("the repository owner by default", func(t *testing.T) {
		c := newCLIRig(t)
		c.mustRun(testrig.Token+"\n", c.initArgs()...)
		cfg, _ := os.ReadFile(c.cfgPath)
		if !strings.Contains(string(cfg), "trusted_actors: [acme]") {
			t.Errorf("config:\n%s", cfg)
		}
		if !strings.Contains(c.out.String(), "only code from acme is allowed to run on this machine") {
			t.Errorf("init should say who is trusted:\n%s", c.out.String())
		}
	})
	t.Run("an explicit list", func(t *testing.T) {
		c := newCLIRig(t)
		c.mustRun(testrig.Token+"\n", c.initArgs("--trusted-actors", "acme, alice")...)
		cfg, _ := os.ReadFile(c.cfgPath)
		if !strings.Contains(string(cfg), "trusted_actors: [acme, alice]") {
			t.Errorf("config:\n%s", cfg)
		}
	})
	t.Run("organization scope must name them", func(t *testing.T) {
		c := newCLIRig(t)
		code := c.run(testrig.Token+"\n", "init", "-c", c.cfgPath, "--scope", "org", "--organization", "acme", "--token-stdin")
		if code != cli.ExitFailure || !strings.Contains(c.err.String(), "runner.trusted_actors") {
			t.Errorf("exit %d, stderr:\n%s", code, c.err.String())
		}
	})
}

func TestHookCommandDecidesLikeThePolicy(t *testing.T) {
	c := newCLIRig(t)
	policy := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(policy, []byte(`{"version":1,"scope":"repo","repository":"acme/widgets","trusted_actors":["acme"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	hook := func(env map[string]string, args ...string) int {
		c.out.Reset()
		c.err.Reset()
		var environ []string
		for k, v := range env {
			environ = append(environ, k+"="+v)
		}
		d := cli.Deps{Stdout: &c.out, Stderr: &c.err, Environ: func() []string { return environ }}
		return cli.Run(context.Background(), append([]string{"hook"}, args...), d)
	}
	owner := map[string]string{"GITHUB_REPOSITORY": "acme/widgets", "GITHUB_ACTOR": "acme", "GITHUB_EVENT_NAME": "push"}
	stranger := map[string]string{"GITHUB_REPOSITORY": "acme/widgets", "GITHUB_ACTOR": "mallory", "GITHUB_EVENT_NAME": "push"}

	if code := hook(owner, "job-started", "--policy", policy); code != cli.ExitOK || !strings.Contains(c.out.String(), "allowed") {
		t.Errorf("owner: exit %d out=%q err=%q", code, c.out.String(), c.err.String())
	}
	if code := hook(stranger, "job-started", "--policy", policy); code != cli.ExitFailure || !strings.Contains(c.err.String(), "REFUSED") {
		t.Errorf("stranger: exit %d err=%q", code, c.err.String())
	}
	if code := hook(owner, "job-started"); code != cli.ExitFailure || !strings.Contains(c.err.String(), "no policy file") {
		t.Errorf("no --policy must refuse: exit %d err=%q", code, c.err.String())
	}
	if code := hook(owner, "job-started", "--policy", filepath.Join(t.TempDir(), "absent.json")); code != cli.ExitFailure || !strings.Contains(c.err.String(), "cannot be read") {
		t.Errorf("a missing policy must refuse: exit %d err=%q", code, c.err.String())
	}
	if code := hook(owner); code != cli.ExitUsage {
		t.Errorf("`hook` with no subcommand: exit %d, want 2", code)
	}
	if code := hook(owner, "other"); code != cli.ExitUsage {
		t.Errorf("an unknown hook: exit %d, want 2", code)
	}
}

// ---- permission by cryptographic key -------------------------------------------------

func (c *cliRig) serveCommits() (map[string]testrig.RealCommit, map[string]string) {
	c.t.Helper()
	commits, pub := testrig.RealFixtures(c.t)
	for _, cm := range commits {
		c.srv.AddCommit("acme/widgets", cm.SHA, cm.Payload, cm.Signature)
	}
	return commits, pub
}

func (c *cliRig) writePub(name, line string) string {
	p := filepath.Join(c.t.TempDir(), name+".pub")
	if err := os.WriteFile(p, []byte(line+"\n"), 0o644); err != nil {
		c.t.Fatal(err)
	}
	return p
}

func TestPermissionIsGrantedAndWithdrawnWithKeys(t *testing.T) {
	c := newCLIRig(t)
	commits, pub := c.serveCommits()
	c.mustRun(testrig.Token+"\n", c.initArgs()...)
	trustArgs := func(rest ...string) []string { return append([]string{"trust"}, append(rest, "-c", c.cfgPath)...) }
	verify := func(who string) int {
		return c.run("", append([]string{"trust", "verify", "-c", c.cfgPath, "--sha"}, commits[who].SHA)...)
	}

	// Nobody has permission yet.
	c.mustRun("", trustArgs("list")...)
	if !strings.Contains(c.out.String(), "nobody has permission") {
		t.Errorf("list on an empty store:\n%s", c.out.String())
	}
	if code := verify("owner"); code != cli.ExitFailure || !strings.Contains(c.err.String(), "BR-E067") {
		t.Fatalf("verify with nobody trusted: exit %d\n%s", code, c.err.String())
	}

	// The owner trusts their own key; a contributor's commit is still refused.
	c.mustRun("", trustArgs("add", "--name", "owner", "--key", c.writePub("owner", pub["owner"]))...)
	if !strings.Contains(c.out.String(), "owner may now run code here") {
		t.Errorf("add output:\n%s", c.out.String())
	}
	c.mustRun("", "trust", "verify", "-c", c.cfgPath, "--sha", commits["owner"].SHA)
	if !strings.Contains(c.out.String(), "admitted") || !strings.Contains(c.out.String(), "signed by owner") {
		t.Errorf("verify output:\n%s", c.out.String())
	}
	if code := verify("mallory"); code != cli.ExitFailure || !strings.Contains(c.err.String(), "does not trust") || !strings.Contains(c.err.String(), "bladerunner trust add") {
		t.Fatalf("an untrusted contributor: exit %d\n%s", code, c.err.String())
	}

	// The contributor's key arrives on stdin (as if pasted or piped): permission is granted.
	c.mustRun(pub["mallory"]+"\n", trustArgs("add", "--name", "mallory", "--key", "-", "--expires", "2099-01-01")...)
	if code := verify("mallory"); code != cli.ExitOK {
		t.Fatalf("after granting: exit %d\n%s", code, c.err.String())
	}
	c.mustRun("", trustArgs("list")...)
	for _, want := range []string{"owner", "mallory", "active", "2099-01-01", "never", "SHA256:"} {
		if !strings.Contains(c.out.String(), want) {
			t.Errorf("list missing %q:\n%s", want, c.out.String())
		}
	}
	if strings.Contains(c.out.String(), "ssh-ed25519") {
		t.Errorf("list should show fingerprints, not whole keys:\n%s", c.out.String())
	}

	// Withdrawn: refused again, and the record says why.
	c.mustRun("", "trust", "revoke", "mallory", "-c", c.cfgPath)
	if code := verify("mallory"); code != cli.ExitFailure || !strings.Contains(c.err.String(), "revoked") {
		t.Fatalf("after revoking: exit %d\n%s", code, c.err.String())
	}
	c.mustRun("", trustArgs("list")...)
	if !strings.Contains(c.out.String(), "revoked") {
		t.Errorf("a revoked signer stays listed, marked revoked:\n%s", c.out.String())
	}
	// A revoked key cannot be quietly re-trusted under another name.
	if code := c.run(pub["mallory"]+"\n", trustArgs("add", "--name", "mallory2", "--key", "-")...); code != cli.ExitFailure || !strings.Contains(c.err.String(), "revoked") {
		t.Errorf("re-adding a revoked key: exit %d\n%s", code, c.err.String())
	}
	// The owner's own commit is unaffected.
	if code := verify("owner"); code != cli.ExitOK {
		t.Errorf("the owner's commit after revoking someone else: exit %d", code)
	}
}

func TestTrustCommandsRefuseMisuse(t *testing.T) {
	c := newCLIRig(t)
	_, pub := c.serveCommits()
	c.mustRun(testrig.Token+"\n", c.initArgs()...)
	cfg := []string{"-c", c.cfgPath}
	with := func(args ...string) []string { return append(append([]string{"trust"}, args...), cfg...) }

	// A private key must be refused, not accepted or stored.
	if code := c.run("-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXk=\n-----END OPENSSH PRIVATE KEY-----\n", with("add", "--name", "x", "--key", "-")...); code != cli.ExitFailure ||
		!strings.Contains(c.err.String(), "never send a private key") {
		t.Errorf("a private key: exit %d\n%s", code, c.err.String())
	}
	if strings.Contains(c.err.String(), "b3BlbnNzaC1rZXk") {
		t.Error("the key material was echoed back")
	}
	// Other key types and GPG keys are refused with the supported type named.
	for _, bad := range []string{"ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQC7 x", "-----BEGIN PGP PUBLIC KEY BLOCK-----"} {
		if code := c.run(bad+"\n", with("add", "--name", "x", "--key", "-")...); code != cli.ExitFailure || !strings.Contains(c.err.String(), "BR-E066") {
			t.Errorf("%q: exit %d\n%s", bad, code, c.err.String())
		}
	}
	for name, args := range map[string][]string{
		"add without a name":    with("add", "--key", "-"),
		"add without a key":     with("add", "--name", "x"),
		"add with a bad date":   with("add", "--name", "x", "--key", "-", "--expires", "next week"),
		"verify without a sha":  with("verify"),
		"revoke with no target": with("revoke"),
		"unknown subcommand":    with("frobnicate"),
		"no subcommand":         {"trust"},
	} {
		if code := c.run(pub["owner"]+"\n", args...); code != cli.ExitUsage {
			t.Errorf("%s: exit %d, want 2\n%s", name, code, c.err.String())
		}
	}
	if code := c.run("", with("add", "--name", "x", "--key", filepath.Join(t.TempDir(), "absent.pub"))...); code != cli.ExitFailure {
		t.Errorf("a missing key file: exit %d", code)
	}
	if code := c.run("", with("revoke", "nobody")...); code != cli.ExitFailure || !strings.Contains(c.err.String(), "BR-E066") {
		t.Errorf("revoking nobody: exit %d\n%s", code, c.err.String())
	}
}

func TestOnlyFullCommitIdsAreAccepted(t *testing.T) {
	c := newCLIRig(t)
	commits, pub := c.serveCommits()
	c.mustRun(testrig.Token+"\n", c.initArgs()...)
	c.mustRun("", "trust", "add", "--name", "owner", "--key", c.writePub("o", pub["owner"]), "-c", c.cfgPath)
	if code := c.run("", "trust", "verify", "--sha", commits["owner"].SHA[:10], "-c", c.cfgPath); code != cli.ExitFailure || !strings.Contains(c.err.String(), "full commit id") {
		t.Errorf("abbreviated id: exit %d\n%s", code, c.err.String())
	}
}

func TestProbeCommandReportsWithoutTouchingTheMachine(t *testing.T) {
	c := newCLIRig(t)
	c.serveCommits()
	c.srv.AddRun("acme/widgets", providerRun(7))
	d := func(args ...string) int {
		c.out.Reset()
		c.err.Reset()
		deps := cli.Deps{
			UserHome: c.userHome, Stdin: strings.NewReader(testrig.Token + "\n"), Stdout: &c.out, Stderr: &c.err,
			GitHubAPIURL: c.srv.URL, GitHubWebURL: c.srv.URL,
			Getenv: func(k string) string {
				if k == "BLADERUNNER_TOKEN" {
					return testrig.Token
				}
				return ""
			},
		}
		return cli.Run(context.Background(), append([]string{"probe", "github"}, args...), deps)
	}

	if code := d("--repository", "acme/widgets"); code != cli.ExitOK {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, c.out.String(), c.err.String())
	}
	out := c.out.String()
	for _, want := range []string{"[PASS] A2", "[PASS] C1", "[PASS] C3", "[PASS] C2", "no secrets", "not changed"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
	// No pull request exists to look at, so C10 to C12b were skipped: exit 0, but the summary must
	// not read as a clean bill of health, and the last line says so apart from the table.
	if !strings.Contains(out, "4 checks SKIPPED, BR-0 is NOT complete (C10, C11, C12, C12b)") {
		t.Errorf("the summary does not say what was skipped:\n%s", out)
	}
	if lines := strings.Split(strings.TrimSpace(out), "\n"); !strings.HasPrefix(lines[len(lines)-1], "BR-0 INCOMPLETE: C10, C11, C12, C12b skipped") {
		t.Errorf("the last line = %q", lines[len(lines)-1])
	}
	for _, secret := range []string{testrig.Token, "JIT-"} {
		if strings.Contains(out, secret) {
			t.Errorf("the report leaked %q", secret)
		}
	}
	if left, _ := os.ReadDir(c.userHome); len(left) != 0 {
		t.Errorf("the probe wrote to the machine: %v", left)
	}
	if n := len(c.srv.Runners("acme/widgets")); n != 0 {
		t.Errorf("the probe left %d runner(s) on GitHub", n)
	}

	// The token can also come on stdin; a failing assumption exits non-zero and says so.
	c.srv.JITUnsupported = true
	if code := d("--repository", "acme/widgets", "--token-stdin"); code != cli.ExitFailure || !strings.Contains(c.out.String(), "At least one assumption failed") {
		t.Errorf("a failing probe: exit %d\n%s", code, c.out.String())
	}
	if code := d("--repository", "acme/widgets", "--skip-jit", "--token-stdin"); code != cli.ExitOK {
		t.Errorf("--skip-jit: exit %d\n%s", code, c.out.String())
	}
}

func TestProbeCommandRefusesMisuse(t *testing.T) {
	c := newCLIRig(t)
	run := func(stdin string, getenv func(string) string, args ...string) int {
		c.err.Reset()
		return cli.Run(context.Background(), append([]string{"probe"}, args...),
			cli.Deps{Stdin: strings.NewReader(stdin), Stdout: &c.out, Stderr: &c.err, GitHubAPIURL: c.srv.URL, Getenv: getenv})
	}
	none := func(string) string { return "" }
	if code := run("", none, "github", "--repository", "acme/widgets"); code != cli.ExitFailure || !strings.Contains(c.err.String(), "BR-E020") {
		t.Errorf("no token: exit %d\n%s", code, c.err.String())
	}
	if code := run("has space\n", none, "github", "--repository", "acme/widgets", "--token-stdin"); code != cli.ExitFailure {
		t.Errorf("a malformed token: exit %d", code)
	}
	for name, args := range map[string][]string{
		"no subcommand":    {},
		"unknown target":   {"gitlab"},
		"no repository":    {"github"},
		"a bare repo name": {"github", "--repository", "widgets"},
	} {
		if code := run("x", none, args...); code != cli.ExitUsage {
			t.Errorf("%s: exit %d, want 2", name, code)
		}
	}
}

func providerRun(id int64) provider.Run {
	return provider.Run{ID: id, HeadSHA: strings.Repeat("a", 40), Event: "push", Status: "completed", HeadRepository: "acme/widgets", Actor: "acme"}
}
