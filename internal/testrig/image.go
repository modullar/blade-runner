package testrig

import (
	"crypto/sha256"
	"encoding/hex"
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
	runnerImgMu  sync.Mutex
	runnerImgTag string // set once the image has been built by this process
	runnerImgBin string // the built fakerunner, kept so a vanished image can be rebuilt
	runnerImgErr error
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
	runnerImgMu.Lock()
	defer runnerImgMu.Unlock()
	// Other test processes share the daemon and it may garbage-collect images between tests, so
	// the image is checked on every call (under the lock) and rebuilt when it has gone.
	if runnerImgTag != "" {
		if out, err := exec.Command("docker", "image", "inspect", "--format", "{{.Id}}", runnerImgTag).Output(); err == nil {
			return strings.TrimSpace(string(out))
		}
	}
	if runnerImgErr == nil && runnerImgBin == "" {
		runnerImgErr = buildFakeRunnerBinary()
	}
	if runnerImgErr == nil {
		runnerImgErr = buildFakeRunnerImage()
	}
	if runnerImgErr != nil {
		t.Fatalf("cannot prepare the runner image: %v", runnerImgErr)
	}
	out, err := exec.Command("docker", "image", "inspect", "--format", "{{.Id}}", runnerImgTag).Output()
	if err != nil {
		t.Fatalf("cannot inspect the runner image: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func buildFakeRunnerBinary() error {
	dir, err := os.MkdirTemp("", "fakerunner-bin-")
	if err != nil {
		return err
	}
	bin := filepath.Join(dir, "fakerunner")
	build := exec.Command("go", "build", "-o", bin, "github.com/modullar/blade-runner/internal/supervisor/testdata/fakerunner")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("build fakerunner: %v\n%s", err, out)
	}
	runnerImgBin = bin
	return nil
}

// buildFakeRunnerImage builds the FROM scratch image. The tag carries the binary's content hash:
// each test process builds its own copy, and a shared tag would be moved by the second build,
// leaving the first process's image untagged while it is still in use.
func buildFakeRunnerImage() error {
	data, err := os.ReadFile(runnerImgBin)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	tag := "br-supervisor-fakerunner:" + hex.EncodeToString(sum[:8])
	dir, err := os.MkdirTemp("", "fakerunner-img-")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "fakerunner"), data, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\nCOPY fakerunner /fakerunner\nENTRYPOINT [\"/fakerunner\"]\n"), 0o644); err != nil {
		return err
	}
	if out, err := exec.Command("docker", "build", "-q", "-t", tag, dir).CombinedOutput(); err != nil {
		return fmt.Errorf("docker build: %v\n%s", err, out)
	}
	runnerImgTag = tag
	return nil
}
