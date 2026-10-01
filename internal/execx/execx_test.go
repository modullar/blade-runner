package execx

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These run real processes: OS is the one place the program touches the machine, and a fake
// cannot show that arguments, environment, stdin and exit codes really arrive.

var ctx = context.Background()

func sh(script string, extra ...Cmd) Cmd {
	c := Cmd{Name: "sh", Args: []string{"-c", script}}
	if len(extra) > 0 {
		c.Env, c.Stdin, c.Stdout, c.Dir = extra[0].Env, extra[0].Stdin, extra[0].Stdout, extra[0].Dir
	}
	return c
}

func TestRunCapturesOutputAndSucceeds(t *testing.T) {
	res, err := OS{}.Run(ctx, sh(`printf out; printf err >&2`))
	if err != nil || res.Stdout != "out" || res.Stderr != "err" || res.ExitCode != 0 {
		t.Errorf("res=%+v err=%v", res, err)
	}
}

func TestEnvIsPassedAndAdded(t *testing.T) {
	t.Setenv("BR_EXISTING", "kept")
	res, err := OS{}.Run(ctx, sh(`printf '%s/%s' "$BR_EXISTING" "$BR_EXTRA"`, Cmd{Env: []string{"BR_EXTRA=added"}}))
	if err != nil || res.Stdout != "kept/added" {
		t.Errorf("env must extend the current environment, not replace it: %q %v", res.Stdout, err)
	}
}

func TestStdinReachesTheProcess(t *testing.T) {
	res, err := OS{}.Run(ctx, sh(`cat`, Cmd{Stdin: "secret-on-stdin"}))
	if err != nil || res.Stdout != "secret-on-stdin" {
		t.Errorf("stdout = %q, %v", res.Stdout, err)
	}
}

func TestSecretsPassedByEnvAreNotInTheProcessArguments(t *testing.T) {
	// The spec's rule: secrets travel in env or stdin, never argv. Show a child cannot see it
	// in its own argument list.
	res, err := OS{}.Run(ctx, sh(`ps -o args= -p $$`, Cmd{Env: []string{"TOKEN=ghp_supersecret"}}))
	if err != nil {
		t.Skipf("ps unavailable: %v", err)
	}
	if strings.Contains(res.Stdout, "ghp_supersecret") {
		t.Errorf("the env value showed up in the process arguments: %q", res.Stdout)
	}
}

func TestNonZeroExitIsAnExitErrorWithoutLeakingEnv(t *testing.T) {
	res, err := OS{}.Run(ctx, sh(`echo "boom" >&2; exit 7`, Cmd{Env: []string{"TOKEN=ghp_supersecret"}}))
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %T %v, want *ExitError", err, err)
	}
	if res.ExitCode != 7 || ee.Result.ExitCode != 7 {
		t.Errorf("exit code = %d / %d", res.ExitCode, ee.Result.ExitCode)
	}
	if !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "status 7") {
		t.Errorf("message should carry stderr and the status: %q", err)
	}
	if strings.Contains(err.Error(), "ghp_supersecret") {
		t.Errorf("the error leaked the environment: %q", err)
	}
}

func TestExitErrorFallsBackToStdoutThenToTheStatus(t *testing.T) {
	_, err := OS{}.Run(ctx, sh(`echo "only stdout"; exit 2`))
	if !strings.Contains(err.Error(), "only stdout") {
		t.Errorf("%q", err)
	}
	_, err = OS{}.Run(ctx, sh(`exit 3`))
	if !strings.Contains(err.Error(), "status 3") || strings.Contains(err.Error(), ": ") && strings.Count(err.Error(), ":") > 1 {
		t.Errorf("%q", err)
	}
}

func TestLongStderrIsTrimmed(t *testing.T) {
	_, err := OS{}.Run(ctx, sh(`for i in 1 2 3 4 5 6 7 8 9 10; do echo line$i >&2; done; exit 1`))
	if strings.Contains(err.Error(), "line10") || !strings.Contains(err.Error(), "...") {
		t.Errorf("an error message must not dump unbounded output: %q", err)
	}
}

func TestStartFailureIsNotAnExitError(t *testing.T) {
	_, err := OS{}.Run(ctx, Cmd{Name: "definitely-not-a-real-command-xyz"})
	var ee *ExitError
	if err == nil || errors.As(err, &ee) {
		t.Errorf("a command that cannot start is not an exit: %v", err)
	}
}

func TestStdoutCanBeStreamed(t *testing.T) {
	var buf bytes.Buffer
	res, err := OS{}.Run(ctx, sh(`printf streamed`, Cmd{Stdout: &buf}))
	if err != nil || buf.String() != "streamed" || res.Stdout != "" {
		t.Errorf("streamed=%q collected=%q err=%v", buf.String(), res.Stdout, err)
	}
}

func TestDirIsHonored(t *testing.T) {
	dir := t.TempDir()
	res, err := OS{}.Run(ctx, sh(`pwd -P`, Cmd{Dir: dir}))
	if err != nil || !strings.HasSuffix(strings.TrimSpace(res.Stdout), strings.TrimPrefix(dir, "/private")) {
		t.Errorf("pwd = %q want %q (%v)", res.Stdout, dir, err)
	}
}

func TestContextCancelStopsTheProcess(t *testing.T) {
	c, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := OS{}.Run(c, sh(`sleep 30`))
	if err == nil || time.Since(start) > 10*time.Second {
		t.Errorf("a cancelled context must stop the command promptly: err=%v after %v", err, time.Since(start))
	}
}

func TestCancelStopsTheWholeProcessTree(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "child-survived")
	c, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	// A child that would create the marker after 1s if it were left running.
	_, _ = OS{}.Run(c, sh(`(sleep 1; touch `+marker+`) & wait`))
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Error("the child process outlived the cancelled command: only the direct child was killed")
	}
}

func TestLookPath(t *testing.T) {
	if p, err := (OS{}).LookPath("sh"); err != nil || p == "" {
		t.Errorf("sh: %q %v", p, err)
	}
	if _, err := (OS{}).LookPath("definitely-not-a-real-command-xyz"); err == nil {
		t.Error("a missing tool must be an error")
	}
}

func TestFakeRecordsAndAnswers(t *testing.T) {
	f := &Fake{Tools: map[string]bool{"git": true}, Handler: func(c Cmd) (Result, error) {
		if c.Args[0] == "bad" {
			return Fail(c.Name, 4, "nope")
		}
		return Result{Stdout: "ok"}, nil
	}}
	if res, err := f.Run(ctx, Cmd{Name: "x", Args: []string{"good"}}); err != nil || res.Stdout != "ok" {
		t.Errorf("%+v %v", res, err)
	}
	if _, err := f.Run(ctx, Cmd{Name: "x", Args: []string{"bad"}}); err == nil {
		t.Error("the handler's failure must come back")
	}
	if got := f.Lines(); len(got) != 2 || got[0] != "[x good]" {
		t.Errorf("lines = %v", got)
	}
	if _, err := f.LookPath("git"); err != nil {
		t.Error("git is listed")
	}
	if _, err := f.LookPath("svn"); err == nil {
		t.Error("svn is not")
	}
	if f.Line(99) != "" {
		t.Error("an out-of-range line is empty")
	}
}
