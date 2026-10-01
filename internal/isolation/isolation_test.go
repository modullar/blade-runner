package isolation

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/dockerlock"
	"github.com/modullar/blade-runner/internal/execx"
)

var ctx = context.Background()

// ---- pure: the audit and the spec ----------------------------------------------------

// compliant is what `docker inspect` reports for a container that meets every invariant.
const compliant = `[{
  "Config": {"User": "65534:65534"},
  "HostConfig": {
    "Privileged": false, "CapAdd": null, "CapDrop": ["ALL"], "ReadonlyRootfs": true,
    "SecurityOpt": ["no-new-privileges"], "NetworkMode": "none",
    "PidMode": "", "IpcMode": "private", "UTSMode": "", "UsernsMode": "",
    "Devices": [], "Binds": null, "VolumesFrom": null, "PidsLimit": 512,
    "Memory": 4294967296, "NanoCpus": 2000000000, "PublishAllPorts": false, "PortBindings": {}
  },
  "Mounts": []
}]`

func mutate(t *testing.T, edit func(m map[string]any)) []byte {
	t.Helper()
	var list []map[string]any
	if err := json.Unmarshal([]byte(compliant), &list); err != nil {
		t.Fatal(err)
	}
	edit(list[0])
	b, _ := json.Marshal(list)
	return b
}

func host(m map[string]any) map[string]any { return m["HostConfig"].(map[string]any) }

func TestAuditAcceptsACompliantContainer(t *testing.T) {
	v, err := Audit([]byte(compliant), Spec{Network: NetworkNone})
	if err != nil || len(v) != 0 {
		t.Fatalf("violations = %v, err = %v", v, err)
	}
}

func TestAuditCatchesEveryWayAContainerCanBeLessConfined(t *testing.T) {
	tests := []struct {
		name string
		edit func(m map[string]any)
		want string
	}{
		{"root user", func(m map[string]any) { m["Config"].(map[string]any)["User"] = "" }, "runs as root"},
		{"explicit root", func(m map[string]any) { m["Config"].(map[string]any)["User"] = "0:0" }, "runs as root"},
		{"named root", func(m map[string]any) { m["Config"].(map[string]any)["User"] = "root" }, "runs as root"},
		{"privileged", func(m map[string]any) { host(m)["Privileged"] = true }, "privileged"},
		{"added capability", func(m map[string]any) { host(m)["CapAdd"] = []string{"SYS_ADMIN"} }, "adds capabilities"},
		{"capabilities not dropped", func(m map[string]any) { host(m)["CapDrop"] = nil }, "does not drop all"},
		{"only some dropped", func(m map[string]any) { host(m)["CapDrop"] = []string{"NET_RAW"} }, "does not drop all"},
		{"writable rootfs", func(m map[string]any) { host(m)["ReadonlyRootfs"] = false }, "writable root"},
		{"can gain privileges", func(m map[string]any) { host(m)["SecurityOpt"] = nil }, "no-new-privileges"},
		{"seccomp off", func(m map[string]any) {
			host(m)["SecurityOpt"] = []string{"no-new-privileges", "seccomp=unconfined"}
		}, "disables a security profile"},
		{"apparmor off", func(m map[string]any) {
			host(m)["SecurityOpt"] = []string{"no-new-privileges", "apparmor=unconfined"}
		}, "disables a security profile"},
		{"host network", func(m map[string]any) { host(m)["NetworkMode"] = "host" }, "network mode"},
		{"another container's network", func(m map[string]any) { host(m)["NetworkMode"] = "container:abc" }, "network mode"},
		{"network when none was asked for", func(m map[string]any) { host(m)["NetworkMode"] = "bridge" }, "asked for no network"},
		{"host pid namespace", func(m map[string]any) { host(m)["PidMode"] = "host" }, "pid namespace"},
		{"host ipc namespace", func(m map[string]any) { host(m)["IpcMode"] = "host" }, "ipc namespace"},
		{"host uts namespace", func(m map[string]any) { host(m)["UTSMode"] = "host" }, "uts namespace"},
		{"host devices", func(m map[string]any) { host(m)["Devices"] = []any{map[string]string{"PathOnHost": "/dev/mem"}} }, "devices"},
		{"bind mount in HostConfig", func(m map[string]any) { host(m)["Binds"] = []string{"/:/host"} }, "mounts host paths"},
		{"bind mount in Mounts", func(m map[string]any) {
			m["Mounts"] = []map[string]string{{"Type": "bind", "Source": "/etc", "Destination": "/etc"}}
		}, "bind mount"},
		{"a named volume", func(m map[string]any) {
			m["Mounts"] = []map[string]string{{"Type": "volume", "Source": "v", "Destination": "/v"}}
		}, "volume mount"},
		{"volumes from another container", func(m map[string]any) { host(m)["VolumesFrom"] = []string{"other"} }, "mounts host paths"},
		{"published ports", func(m map[string]any) { host(m)["PublishAllPorts"] = true }, "publishes ports"},
		{"port bindings", func(m map[string]any) { host(m)["PortBindings"] = map[string]any{"80/tcp": []any{}} }, "publishes ports"},
		{"no pid limit", func(m map[string]any) { host(m)["PidsLimit"] = nil }, "process limit"},
		{"pid limit of zero", func(m map[string]any) { host(m)["PidsLimit"] = 0 }, "process limit"},
		{"no memory limit", func(m map[string]any) { host(m)["Memory"] = 0 }, "memory limit"},
		{"no cpu limit", func(m map[string]any) { host(m)["NanoCpus"] = 0 }, "CPU limit"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := Audit(mutate(t, tc.edit), Spec{Network: NetworkNone})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(v, "\n"), tc.want) {
				t.Errorf("violations %v should include one mentioning %q", v, tc.want)
			}
			if e := AuditError("c", v); diag.CodeOf(e) != diag.CodeIsolation {
				t.Errorf("AuditError code = %q", diag.CodeOf(e))
			}
		})
	}
}

func TestAuditRefusesWhatItCannotRead(t *testing.T) {
	for name, in := range map[string]string{"not json": "nope", "empty list": "[]", "two objects": "[{},{}]"} {
		if _, err := Audit([]byte(in), Spec{}); err == nil {
			t.Errorf("%s: an unreadable configuration must never be treated as compliant", name)
		}
	}
	if AuditError("c", nil) != nil {
		t.Error("no violations, no error")
	}
}

func TestSpecValidation(t *testing.T) {
	good := Spec{Name: "job-1", Image: "sha256:" + strings.Repeat("a", 64)}
	got, err := good.Validate()
	if err != nil {
		t.Fatal(err)
	}
	if got.Network != NetworkNone || got.MemoryMiB != DefaultMemoryMiB || got.PidsLimit != DefaultPids || got.CPUs != DefaultCPUs || got.Timeout != DefaultTimeout {
		t.Errorf("defaults = %+v: the default network must be none, and every limit set", got)
	}
	if _, err := (Spec{Name: "j", Image: "registry.example/runner@sha256:" + strings.Repeat("b", 64)}).Validate(); err != nil {
		t.Errorf("a digest-pinned reference must be accepted: %v", err)
	}
	for name, s := range map[string]Spec{
		"a tag":                           {Name: "j", Image: "runner:latest"},
		"a tag with a digest-like suffix": {Name: "j", Image: "runner:sha256-" + strings.Repeat("a", 64)},
		"a short digest":                  {Name: "j", Image: "sha256:abc"},
		"an uppercase digest":             {Name: "j", Image: "sha256:" + strings.Repeat("A", 64)},
		"no image":                        {Name: "j"},
		"a bad name":                      {Name: "a b", Image: good.Image},
		"a name with a slash":             {Name: "a/b", Image: good.Image},
		"an empty name":                   {Image: good.Image},
		"host networking":                 {Name: "j", Image: good.Image, Network: "host"},
		"another container's network":     {Name: "j", Image: good.Image, Network: "container:x"},
		"a bad env name":                  {Name: "j", Image: good.Image, Env: map[string]string{"A B": "x"}},
	} {
		if _, err := s.Validate(); diag.CodeOf(err) != diag.CodeIsolation {
			t.Errorf("%s: err = %v, want BR-E068", name, err)
		}
	}
}

func TestCreateArgsCarryEveryHardeningFlagAndNeverASecret(t *testing.T) {
	s, _ := Spec{Name: "j1", Image: "sha256:" + strings.Repeat("c", 64), Stdin: "SECRET-TOKEN", Env: map[string]string{"MODE": "ci"},
		Args: []string{"run", "--x"}}.Validate()
	args := strings.Join(createArgs(s), " ")
	for _, want := range []string{
		"--read-only", "--cap-drop ALL", "--security-opt no-new-privileges", "--user 65534:65534", "--network none",
		"--pids-limit 512", "--memory 4096m", "--memory-swap 4096m", "--cpus 2", "--log-driver none", "--restart no",
		"--label bladerunner.job=j1", "--env MODE=ci", "--workdir /work", "noexec", "uid=65534,gid=65534,mode=0700", "run --x",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("create args missing %q:\n%s", want, args)
		}
	}
	for _, forbidden := range []string{"--privileged", "--pid host", "--network host", " -v ", "--volume", "--mount", "--device", "--cap-add", "SECRET-TOKEN", "--publish", " -p "} {
		if strings.Contains(args, forbidden) {
			t.Errorf("create args must never contain %q:\n%s", forbidden, args)
		}
	}
}

func TestOutputIsCapped(t *testing.T) {
	var c cappedBuffer
	chunk := strings.Repeat("x", 1<<20)
	for i := 0; i < 10; i++ {
		if n, err := c.Write([]byte(chunk)); n != len(chunk) || err != nil {
			t.Fatalf("a write must always succeed so the job is never slowed: %d %v", n, err)
		}
	}
	if !strings.HasSuffix(c.String(), "[output truncated]") || len(c.String()) > MaxOutputBytes+64 {
		t.Errorf("kept %d bytes, truncated marker %v", len(c.String()), strings.HasSuffix(c.String(), "[output truncated]"))
	}
}

// ---- real Docker ---------------------------------------------------------------------

var (
	imgOnce sync.Once
	imgID   string
	imgErr  error
	real    = &Docker{Exec: execx.OS{}}
)

// needDocker skips when there is no usable Docker daemon, and otherwise returns the ID of a
// FROM scratch image holding the probe program.
func needDocker(t *testing.T) string {
	t.Helper()
	if _, err := real.Preflight(ctx); err != nil {
		t.Skipf("no usable Docker daemon: %v", err)
	}
	dockerlock.Lock(t)
	imgOnce.Do(func() {
		dir, err := os.MkdirTemp("", "probe-img-")
		if err != nil {
			imgErr = err
			return
		}
		build := exec.Command("go", "build", "-o", filepath.Join(dir, "probe"), "github.com/modullar/blade-runner/internal/isolation/testdata/probe")
		build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
		if out, err := build.CombinedOutput(); err != nil {
			imgErr = fmt.Errorf("build probe: %v\n%s", err, out)
			return
		}
		_ = os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\nCOPY probe /probe\nENTRYPOINT [\"/probe\"]\n"), 0o644)
		if out, err := exec.Command("docker", "build", "-q", "-t", "br-isolation-probe", dir).CombinedOutput(); err != nil {
			imgErr = fmt.Errorf("docker build: %v\n%s", err, out)
			return
		}
		out, err := exec.Command("docker", "image", "inspect", "--format", "{{.Id}}", "br-isolation-probe").Output()
		imgID, imgErr = strings.TrimSpace(string(out)), err
	})
	if imgErr != nil {
		t.Fatalf("cannot prepare the probe image: %v", imgErr)
	}
	return imgID
}

var counter int
var counterMu sync.Mutex

func jobName(t *testing.T) string {
	counterMu.Lock()
	defer counterMu.Unlock()
	counter++
	return fmt.Sprintf("br-it-%d-%d", os.Getpid(), counter)
}

func probe(t *testing.T, image string, tweak func(*Spec), args ...string) Result {
	t.Helper()
	s := Spec{Name: jobName(t), Image: image, Args: args, Timeout: 60 * time.Second, MemoryMiB: 256, PidsLimit: 64, WorkTmpfsMiB: 64}
	if tweak != nil {
		tweak(&s)
	}
	res, err := real.Run(ctx, s)
	if err != nil {
		t.Fatalf("probe %v: %v", args, err)
	}
	if left, _ := exec.Command("docker", "ps", "-aq", "--filter", "label="+LabelJob+"="+s.Name).Output(); strings.TrimSpace(string(left)) != "" {
		t.Errorf("container %s was left behind", s.Name)
	}
	return res
}

func TestRealDocker_PreflightReportsTheDaemon(t *testing.T) {
	needDocker(t)
	info, err := real.Preflight(ctx)
	if err != nil || info.ServerVersion == "" || info.OSType != "linux" {
		t.Errorf("info = %+v, %v", info, err)
	}
	t.Logf("Docker %s, %s/%s, rootless=%v, security=%v", info.ServerVersion, info.OSType, info.Architecture, info.Rootless, info.SecurityOpts)
}

func TestRealDocker_JobRunsAsNobodyAndItsOutputIsCaptured(t *testing.T) {
	img := needDocker(t)
	res := probe(t, img, nil, "id")
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "uid=65534 gid=65534") {
		t.Errorf("result = %+v: the job must run as the unprivileged user 65534", res)
	}
	if code := probe(t, img, nil, "exit", "7").ExitCode; code != 7 {
		t.Errorf("exit code = %d, want the job's own 7", code)
	}
}

// What a hostile job tries, and what the container really does about it. Each of these ran for
// real; "BLOCKED" means the kernel refused it inside the container.
func TestRealDocker_WhatAHostileJobCannotDo(t *testing.T) {
	img := needDocker(t)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"write to the root filesystem", []string{"write-rootfs"}},
		{"execute from /tmp (noexec)", []string{"exec-tmp"}},
		{"mount a filesystem", []string{"mount"}},
		{"open a raw socket", []string{"raw-socket"}},
		{"chroot", []string{"chroot"}},
		{"become root", []string{"setuid-root"}},
		{"create a user namespace", []string{"unshare-user"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := probe(t, img, nil, tc.args...)
			t.Logf("%s", strings.TrimSpace(res.Stdout))
			if !strings.HasPrefix(res.Stdout, "BLOCKED") {
				t.Errorf("a hostile job was ALLOWED to %s: %q", tc.name, res.Stdout)
			}
		})
	}
}

func TestRealDocker_WhatAJobCanStillDo(t *testing.T) {
	img := needDocker(t)
	for _, args := range [][]string{{"write-work"}, {"write-tmp"}} {
		if res := probe(t, img, nil, args...); !strings.HasPrefix(res.Stdout, "ALLOWED") {
			t.Errorf("%v: a job needs its work area and /tmp: %q", args, res.Stdout)
		}
	}
}

func TestRealDocker_TheHostIsNotVisible(t *testing.T) {
	img := needDocker(t)
	secret := filepath.Join(t.TempDir(), "host-secret.txt")
	if err := os.WriteFile(secret, []byte("do not read me"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := probe(t, img, nil, "read-host-file", secret); !strings.HasPrefix(res.Stdout, "BLOCKED") {
		t.Errorf("a job read a host file: %q", res.Stdout)
	}
	if res := probe(t, img, nil, "read-host-file", "/etc/shadow"); !strings.HasPrefix(res.Stdout, "BLOCKED") {
		t.Errorf("a job read /etc/shadow: %q", res.Stdout)
	}
}

func TestRealDocker_NoNetworkMeansNoNetwork(t *testing.T) {
	img := needDocker(t)
	l, err := net.Listen("tcp", "0.0.0.0:0") // a service on the HOST that a job must not reach
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	port := l.Addr().(*net.TCPAddr).Port
	for _, target := range []string{fmt.Sprintf("127.0.0.1:%d", port), fmt.Sprintf("172.17.0.1:%d", port), "1.1.1.1:443"} {
		if res := probe(t, img, nil, "connect", target); !strings.HasPrefix(res.Stdout, "BLOCKED") {
			t.Errorf("with network none a job reached %s: %q", target, res.Stdout)
		}
	}
}

// KNOWN LIMIT, recorded as a test so it cannot be forgotten: the "bridge" mode reaches whatever
// the host's network reaches, including services on the host. Blocking that needs firewall
// rules this package does not install. If this ever starts failing because Docker now blocks
// it, good: update the docs.
func TestRealDocker_BridgeNetworkCanReachTheHost_KnownLimit(t *testing.T) {
	img := needDocker(t)
	l, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	port := l.Addr().(*net.TCPAddr).Port
	res := probe(t, img, func(s *Spec) { s.Network = NetworkBridge }, "connect", fmt.Sprintf("172.17.0.1:%d", port))
	t.Logf("bridge -> host service: %s", strings.TrimSpace(res.Stdout))
	if strings.HasPrefix(res.Stdout, "ALLOWED") {
		t.Log("CONFIRMED: bridge mode can reach host services; use network none, or add firewall rules (docs/decisions/0006)")
	}
}

func TestRealDocker_LimitsStopRunawayJobs(t *testing.T) {
	img := needDocker(t)

	t.Run("a fork bomb is stopped by the process limit", func(t *testing.T) {
		res := probe(t, img, func(s *Spec) { s.PidsLimit = 32 }, "fork", "500")
		if !strings.Contains(res.Stdout, "STARTED") {
			t.Fatalf("output %q", res.Stdout)
		}
		var started, total int
		fmt.Sscanf(res.Stdout[strings.Index(res.Stdout, "STARTED"):], "STARTED %d of %d", &started, &total)
		if started >= 32 || started == 0 {
			t.Errorf("started %d of %d processes with a limit of 32", started, total)
		}
	})
	t.Run("memory hunger is killed by the memory limit", func(t *testing.T) {
		res := probe(t, img, func(s *Spec) { s.MemoryMiB = 64 }, "alloc", "512")
		if !res.OOMKilled || res.ExitCode != 137 {
			t.Errorf("result = %+v, want OOMKilled with exit 137", res)
		}
	})
	t.Run("a job that never ends is killed at its timeout and removed", func(t *testing.T) {
		start := time.Now()
		res := probe(t, img, func(s *Spec) { s.Timeout = 3 * time.Second }, "sleep", "120")
		if !res.TimedOut || time.Since(start) > 40*time.Second {
			t.Errorf("result = %+v after %v, want TimedOut well before the job's 120s", res, time.Since(start))
		}
	})
	t.Run("endless output is capped", func(t *testing.T) {
		res := probe(t, img, nil, "flood")
		if len(res.Stdout) > MaxOutputBytes+64 || !strings.HasSuffix(res.Stdout, "[output truncated]") {
			t.Errorf("kept %d bytes of a 20 MB flood", len(res.Stdout))
		}
	})
}

// argvRecorder runs real commands and remembers every argv, so a test can prove a secret was
// never on a command line (where any local user could read it).
type argvRecorder struct {
	inner execx.Runner
	mu    sync.Mutex
	argvs []string
}

func (a *argvRecorder) Run(c context.Context, cmd execx.Cmd) (execx.Result, error) {
	a.mu.Lock()
	a.argvs = append(a.argvs, cmd.Name+" "+strings.Join(cmd.Args, " "))
	a.mu.Unlock()
	return a.inner.Run(c, cmd)
}
func (a *argvRecorder) LookPath(n string) (string, error) { return a.inner.LookPath(n) }

func TestRealDocker_SecretsTravelOnStdinNeverArgvOrEnvironment(t *testing.T) {
	img := needDocker(t)
	const secret = "JIT-CONFIG-SUPER-SECRET-VALUE"
	rec := &argvRecorder{inner: execx.OS{}}
	d := &Docker{Exec: rec}
	res, err := d.Run(ctx, Spec{Name: jobName(t), Image: img, Args: []string{"stdin-hash"}, Stdin: secret, Timeout: 30 * time.Second, MemoryMiB: 128, PidsLimit: 32})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Stdout, fmt.Sprintf("stdin bytes=%d", len(secret))) {
		t.Errorf("the job did not receive the secret on stdin: %q", res.Stdout)
	}
	for _, argv := range rec.argvs {
		if strings.Contains(argv, secret) {
			t.Errorf("the secret appeared on a command line: %s", argv)
		}
	}
	if env := probe(t, img, nil, "env"); strings.Contains(env.Stdout, secret) {
		t.Error("the secret is in the job's environment")
	}
}

func TestRealDocker_AuditReadsRealDockerOutput(t *testing.T) {
	img := needDocker(t)
	make := func(name string, flags ...string) []byte {
		t.Helper()
		args := append([]string{"create", "--name", name}, flags...)
		args = append(args, img, "id")
		if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
			t.Fatalf("docker %v: %v\n%s", args, err, out)
		}
		t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
		out, err := exec.Command("docker", "inspect", name).Output()
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	// A container made the careless way, which Docker accepts happily: the audit must see it.
	careless := make(jobName(t))
	v, err := Audit(careless, Spec{Network: NetworkNone})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"runs as root", "does not drop all", "writable root", "no-new-privileges", "process limit", "memory limit", "CPU limit"} {
		if !strings.Contains(strings.Join(v, "\n"), want) {
			t.Errorf("audit of a default container missed %q: %v", want, v)
		}
	}
	// And the dangerous options, each made for real and read back from Docker.
	for name, flags := range map[string][]string{
		"privileged":     {"--privileged"},
		"host network":   {"--network", "host"},
		"host pid":       {"--pid", "host"},
		"a bind mount":   {"-v", "/etc:/hostetc:ro"},
		"published port": {"-p", "127.0.0.1:0:80"},
		"cap-add":        {"--cap-add", "SYS_ADMIN"},
	} {
		v, err := Audit(make(jobName(t), flags...), Spec{Network: NetworkBridge})
		if err != nil || len(v) == 0 {
			t.Errorf("%s: violations %v, err %v: the audit must flag it", name, v, err)
		}
	}
}

// A runtime that ignores a requested flag must not get to run the job. This wraps the real
// docker CLI and drops --read-only from `create`, as a runtime that did not apply it would.
type dropFlag struct {
	inner execx.Runner
	flag  string
}

func (d dropFlag) Run(c context.Context, cmd execx.Cmd) (execx.Result, error) {
	if len(cmd.Args) > 0 && cmd.Args[0] == "create" {
		var kept []string
		for _, a := range cmd.Args {
			if a != d.flag {
				kept = append(kept, a)
			}
		}
		cmd.Args = kept
	}
	return d.inner.Run(c, cmd)
}
func (d dropFlag) LookPath(n string) (string, error) { return d.inner.LookPath(n) }

func TestRealDocker_AJobIsNeverStartedInAContainerThatFailsTheAudit(t *testing.T) {
	img := needDocker(t)
	for _, flag := range []string{"--read-only"} {
		d := &Docker{Exec: dropFlag{inner: execx.OS{}, flag: flag}}
		name := jobName(t)
		// If this job started, it would write a marker the test can see.
		res, err := d.Run(ctx, Spec{Name: name, Image: img, Args: []string{"write-rootfs"}, Timeout: 30 * time.Second, MemoryMiB: 128, PidsLimit: 32})
		if diag.CodeOf(err) != diag.CodeIsolation || !strings.Contains(err.Error(), "NOT started") || !strings.Contains(err.Error(), "writable root filesystem") {
			t.Fatalf("without %s: err = %v, want a BR-E068 refusal naming the violation", flag, err)
		}
		if res.Stdout != "" {
			t.Errorf("the job ran (%q) although the container failed its audit", res.Stdout)
		}
		if left, _ := exec.Command("docker", "ps", "-aq", "--filter", "name="+name).Output(); strings.TrimSpace(string(left)) != "" {
			t.Errorf("the refused container %s was not removed", name)
		}
	}
}

func TestRealDocker_AnUnpinnedImageNeverReachesDocker(t *testing.T) {
	needDocker(t)
	rec := &argvRecorder{inner: execx.OS{}}
	_, err := (&Docker{Exec: rec}).Run(ctx, Spec{Name: jobName(t), Image: "br-isolation-probe:latest"})
	if diag.CodeOf(err) != diag.CodeIsolation {
		t.Fatalf("err = %v", err)
	}
	if len(rec.argvs) != 0 {
		t.Errorf("docker was invoked for a refused spec: %v", rec.argvs)
	}
}

func TestRealDocker_RemoveStaleClearsLeftoverContainers(t *testing.T) {
	img := needDocker(t)
	for i := 0; i < 2; i++ {
		name := jobName(t)
		if out, err := exec.Command("docker", "create", "--name", name, "--label", LabelJob+"="+name, img, "id").CombinedOutput(); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
	}
	n, err := real.RemoveStale(ctx)
	if err != nil || n < 2 {
		t.Fatalf("removed %d (%v), want at least the 2 just made", n, err)
	}
	if left, _ := exec.Command("docker", "ps", "-aq", "--filter", "label="+LabelJob).Output(); strings.TrimSpace(string(left)) != "" {
		t.Errorf("stale containers remain: %s", left)
	}
	if n, err := real.RemoveStale(ctx); err != nil || n != 0 {
		t.Errorf("a second sweep: %d %v", n, err)
	}
}

func TestPreflightMapsAMissingDaemonToAUsefulError(t *testing.T) {
	d := &Docker{Exec: &execx.Fake{Handler: func(c execx.Cmd) (execx.Result, error) {
		return execx.Fail("docker", 1, "Cannot connect to the Docker daemon")
	}}}
	if _, err := d.Preflight(ctx); diag.CodeOf(err) != diag.CodeIsolation || !strings.Contains(err.Error(), "start Docker") {
		t.Errorf("err = %v", err)
	}
	win := &Docker{Exec: &execx.Fake{Handler: func(c execx.Cmd) (execx.Result, error) {
		return execx.Result{Stdout: `{"ServerVersion":"27.0.0","OSType":"windows"}`}, nil
	}}}
	if _, err := win.Preflight(ctx); diag.CodeOf(err) != diag.CodeIsolation {
		t.Errorf("Windows containers must be refused: %v", err)
	}
}
