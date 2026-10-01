package testrig

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/modullar/blade-runner/internal/dockerlock"
)

var (
	runnerImgOnce sync.Once
	runnerImgID   string
	runnerImgErr  error
)

// FakeRunnerImage builds, once per test process, a FROM scratch image holding the fakerunner
// fixture (internal/supervisor/testdata/fakerunner) and returns its image id, which is
// content-pinned (sha256:<hex>) and so accepted by the supervisor and isolation. The test is
// skipped, with the reason, when there is no usable Docker daemon. Nothing is pulled: this
// works where registry access is blocked.
func FakeRunnerImage(t testing.TB) string {
	t.Helper()
	if out, err := exec.Command("docker", "info", "--format", "{{.OSType}}").Output(); err != nil || strings.TrimSpace(string(out)) != "linux" {
		t.Skipf("no usable Docker daemon with Linux containers: %v", err)
	}
	dockerlock.Lock(t) // real-Docker tests in other packages share one daemon
	runnerImgOnce.Do(func() {
		dir, err := os.MkdirTemp("", "fakerunner-img-")
		if err != nil {
			runnerImgErr = err
			return
		}
		build := exec.Command("go", "build", "-o", filepath.Join(dir, "fakerunner"), "github.com/modullar/blade-runner/internal/supervisor/testdata/fakerunner")
		build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
		if out, err := build.CombinedOutput(); err != nil {
			runnerImgErr = fmt.Errorf("build fakerunner: %v\n%s", err, out)
			return
		}
		_ = os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\nCOPY fakerunner /fakerunner\nENTRYPOINT [\"/fakerunner\"]\n"), 0o644)
		if out, err := exec.Command("docker", "build", "-q", "-t", "br-supervisor-fakerunner", dir).CombinedOutput(); err != nil {
			runnerImgErr = fmt.Errorf("docker build: %v\n%s", err, out)
			return
		}
		out, err := exec.Command("docker", "image", "inspect", "--format", "{{.Id}}", "br-supervisor-fakerunner").Output()
		runnerImgID, runnerImgErr = strings.TrimSpace(string(out)), err
	})
	if runnerImgErr != nil {
		t.Fatalf("cannot prepare the runner image: %v", runnerImgErr)
	}
	return runnerImgID
}
