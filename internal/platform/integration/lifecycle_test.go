//go:build integration

// Package integration runs the platform code against the REAL service manager of the machine:
// launchd on macOS, the systemd user manager on Linux. Unit tests elsewhere use fakes for
// these tools; this is the test that shows the commands actually work.
//
//	go test -tags integration -v -count=1 ./internal/platform/integration
//
// It installs a harmless service (a shell script that sleeps) under a unique name, exercises
// the whole lifecycle, and always uninstalls it. It needs a logged-in GUI session on macOS,
// or a reachable systemd user manager on Linux, and skips, saying why, when it has neither.
package integration

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modullar/blade-runner/internal/execx"
	"github.com/modullar/blade-runner/internal/platform"
	"github.com/modullar/blade-runner/internal/platform/host"
)

var ctx = context.Background()

func newPlatform(t *testing.T) (platform.Platform, platform.Spec) {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	p, err := host.New(runtime.GOOS, execx.OS{}, host.User{Name: u.Username, UID: uid, Home: u.HomeDir})
	if err != nil {
		t.Skipf("unsupported platform: %v", err)
	}
	for _, pr := range p.CheckPrereqs(ctx) {
		if !pr.OK {
			t.Skipf("prerequisite %q not met on this machine (%s): %s", pr.Name, pr.Detail, pr.Fix)
		}
	}
	runnerDir := t.TempDir()
	script := "#!/bin/sh\necho hello-from-the-integration-runner\nexec sleep 600\n"
	if err := os.WriteFile(filepath.Join(runnerDir, "run.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	spec := platform.Spec{
		RunnerName: fmt.Sprintf("br-it-%d", os.Getpid()),
		RunnerDir:  runnerDir,
		LogDir:     filepath.Join(runnerDir, "logs"),
		Home:       u.HomeDir,
	}
	t.Cleanup(func() { _ = p.Uninstall(ctx, spec) }) // never leave a service behind
	return p, spec
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func running(t *testing.T, p platform.Platform, s platform.Spec) bool {
	t.Helper()
	st, err := p.Status(ctx, s)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	return st.Running
}

func TestRealServiceLifecycle(t *testing.T) {
	p, spec := newPlatform(t)

	st, err := p.Status(ctx, spec)
	if err != nil || st.Installed || st.Running {
		t.Fatalf("before install: %+v, %v", st, err)
	}

	if err := p.Install(ctx, spec); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if st, _ = p.Status(ctx, spec); !st.Installed || !st.Current || st.Running {
		t.Fatalf("after install: %+v (installed and current, but not started)", st)
	}
	if err := p.Install(ctx, spec); err != nil { // idempotent
		t.Fatalf("second Install: %v", err)
	}

	if err := p.Start(ctx, spec); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "the service to run", func() bool { return running(t, p, spec) })
	if err := p.Start(ctx, spec); err != nil { // starting a running service is fine
		t.Fatalf("second Start: %v", err)
	}

	if runtime.GOOS == "darwin" { // macOS logs are a file we control
		waitFor(t, "the service's output in the log", func() bool {
			var buf bytes.Buffer
			return p.Logs(ctx, spec, false, &buf) == nil && strings.Contains(buf.String(), "hello-from-the-integration-runner")
		})
	}

	if err := p.Stop(ctx, spec); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitFor(t, "the service to stop", func() bool { return !running(t, p, spec) })

	// A changed definition must reload and start again: on macOS this is the window where a
	// too-early bootstrap fails with "Input/output error".
	if err := p.Start(ctx, spec); err != nil {
		t.Fatalf("restart: %v", err)
	}
	waitFor(t, "the restarted service to run", func() bool { return running(t, p, spec) })
	moved := t.TempDir()
	if err := os.WriteFile(filepath.Join(moved, "run.sh"), []byte("#!/bin/sh\nexec sleep 600\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	changed := spec
	changed.RunnerDir = moved
	if st, _ = p.Status(ctx, changed); st.Current {
		t.Fatal("a moved runner directory must make the installed definition stale")
	}
	if err := p.Install(ctx, changed); err != nil {
		t.Fatalf("Install of a changed definition: %v", err)
	}
	if err := p.Start(ctx, changed); err != nil {
		t.Fatalf("Start right after a changed Install: %v", err)
	}
	waitFor(t, "the reloaded service to run", func() bool { return running(t, p, changed) })

	if err := p.Uninstall(ctx, changed); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(p.DefinitionPath(changed)); !os.IsNotExist(err) {
		t.Errorf("the definition file is still there: %v", err)
	}
	if st, _ = p.Status(ctx, changed); st.Installed || st.Running {
		t.Errorf("after uninstall: %+v", st)
	}
	if err := p.Uninstall(ctx, changed); err != nil {
		t.Errorf("Uninstall twice must succeed: %v", err)
	}
}
