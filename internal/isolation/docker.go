package isolation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/execx"
)

// MaxOutputBytes caps what is kept of a job's stdout and of its stderr, each. A job that prints
// without end must not fill this machine's memory.
const MaxOutputBytes = 4 << 20

// LabelJob marks every container this package creates, so stale ones can be found and removed
// after a crash.
const LabelJob = "bladerunner.job"

// Result is the outcome of one job.
type Result struct {
	ExitCode  int
	Stdout    string
	Stderr    string
	TimedOut  bool // the job was killed for running past its timeout
	OOMKilled bool // the job was killed for exceeding its memory limit
}

// Docker runs jobs through the docker CLI.
type Docker struct {
	Exec execx.Runner
	Bin  string // default "docker"

	// RecheckEvery is how often a RUNNING allowlist job's network is audited again (default
	// 500ms). A container created on the network after the pre-start audit is caught by the
	// next check, and the job is killed.
	RecheckEvery time.Duration
}

func (d *Docker) bin() string {
	if d.Bin == "" {
		return "docker"
	}
	return d.Bin
}

func (d *Docker) docker(ctx context.Context, stdin string, args ...string) (execx.Result, error) {
	return d.Exec.Run(ctx, execx.Cmd{Name: d.bin(), Args: args, Stdin: stdin})
}

// Info describes the Docker daemon the jobs will run on.
type Info struct {
	ServerVersion string
	OSType        string
	Architecture  string
	Rootless      bool
	SecurityOpts  []string
}

// Preflight proves Docker is reachable and reports what it is.
func (d *Docker) Preflight(ctx context.Context) (Info, error) {
	res, err := d.docker(ctx, "", "info", "--format", "{{json .}}")
	if err != nil {
		return Info{}, diag.Wrap(err, diag.CodeIsolation, "Docker is not available",
			"the Docker daemon is not running or this user cannot reach it",
			"start Docker (on a Mac: Docker Desktop, Colima or OrbStack), then re-run")
	}
	var raw struct {
		ServerVersion   string   `json:"ServerVersion"`
		OSType          string   `json:"OSType"`
		Architecture    string   `json:"Architecture"`
		SecurityOptions []string `json:"SecurityOptions"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &raw); err != nil {
		return Info{}, diag.Wrap(err, diag.CodeIsolation, "cannot read Docker's description of itself", "an unexpected Docker version", "update Docker")
	}
	info := Info{ServerVersion: raw.ServerVersion, OSType: raw.OSType, Architecture: raw.Architecture, SecurityOpts: raw.SecurityOptions}
	for _, o := range raw.SecurityOptions {
		if strings.Contains(o, "rootless") {
			info.Rootless = true
		}
	}
	if info.OSType != "linux" {
		return info, diag.New(diag.CodeIsolation, "Docker is not running Linux containers ("+info.OSType+")",
			"the runner and its jobs are Linux programs", "switch Docker to Linux containers")
	}
	return info, nil
}

// createArgs builds the `docker create` command line. Every flag here is also checked after the
// fact by Audit, because a flag is a request, not a guarantee.
func createArgs(s Spec) []string {
	args := []string{
		"create", "--name", s.Name, "-i",
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--user", "65534:65534",
		"--network", networkFlag(s),
		"--pids-limit", strconv.Itoa(s.PidsLimit),
		"--memory", fmt.Sprintf("%dm", s.MemoryMiB),
		"--memory-swap", fmt.Sprintf("%dm", s.MemoryMiB), // no swap: the memory cap is real
		"--cpus", strconv.FormatFloat(s.CPUs, 'f', -1, 64),
		"--tmpfs", "/tmp:rw,nosuid,nodev,noexec,size=256m",
		// The work area belongs to the job's own unprivileged user; nobody else in the container
		// (there is nobody else) and nothing outside it can use it.
		"--tmpfs", fmt.Sprintf("/work:rw,nosuid,nodev,exec,uid=65534,gid=65534,mode=0700,size=%dm", s.WorkTmpfsMiB),
		"--workdir", "/work",
		"--restart", "no",
		"--log-driver", "none", // the host keeps no copy of the job's output
		"--label", LabelJob + "=" + s.Name,
	}
	for _, k := range sortedKeys(s.Labels) {
		args = append(args, "--label", k+"="+s.Labels[k])
	}
	for _, k := range sortedKeys(s.Env) {
		args = append(args, "--env", k+"="+s.Env[k])
	}
	if s.Network == NetworkAllowlist {
		for _, k := range ProxyEnv(s) {
			args = append(args, "--env", k)
		}
	}
	args = append(args, s.Image)
	return append(args, s.Args...)
}

// networkFlag is the value of --network: the mode, or for allowlist the internal network itself.
func networkFlag(s Spec) string {
	if s.Network == NetworkAllowlist {
		return s.EgressNetwork
	}
	return s.Network
}

// ProxyEnv is the proxy configuration an allowlist job gets, as KEY=VALUE lines, in both
// spellings clients use. NO_PROXY is set empty so no host is exempted from the proxy.
func ProxyEnv(s Spec) []string {
	u := "http://" + s.EgressProxy
	return []string{"HTTP_PROXY=" + u, "HTTPS_PROXY=" + u, "http_proxy=" + u, "https_proxy=" + u, "NO_PROXY=", "no_proxy="}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// cappedBuffer keeps the first MaxOutputBytes and discards the rest.
type cappedBuffer struct {
	buf       bytes.Buffer
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := MaxOutputBytes - c.buf.Len(); room > 0 {
		n := len(p)
		if n > room {
			n = room
			c.truncated = true
		}
		c.buf.Write(p[:n])
	} else if len(p) > 0 {
		c.truncated = true
	}
	return len(p), nil // never fail a write: the job must not be slowed by a full buffer
}

func (c *cappedBuffer) String() string {
	if c.truncated {
		return c.buf.String() + "\n[output truncated]"
	}
	return c.buf.String()
}

// Run creates the container, audits it, runs it to completion or timeout, and always removes it.
func (d *Docker) Run(ctx context.Context, spec Spec) (Result, error) {
	s, err := spec.Validate()
	if err != nil {
		return Result{}, err
	}
	// Cleanup must not depend on the caller's context: a cancelled job still has to be removed.
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = d.docker(cctx, "", "rm", "-f", "-v", s.Name)
	}()

	if _, err := d.docker(ctx, "", createArgs(s)...); err != nil {
		return Result{}, diag.Wrap(err, diag.CodeIsolation, "cannot create the job's container",
			"Docker refused it (is the image present, and is the name free?)", "check `docker images` and `docker ps -a`")
	}
	ins, err := d.docker(ctx, "", "inspect", s.Name)
	if err != nil {
		return Result{}, diag.Wrap(err, diag.CodeIsolation, "cannot read back the container's configuration", "Docker stopped answering", "check Docker")
	}
	violations, err := Audit([]byte(ins.Stdout), s)
	if err != nil {
		return Result{}, diag.Wrap(err, diag.CodeIsolation, "cannot audit the container", "an unexpected Docker version", "update Docker")
	}
	if s.Network == NetworkAllowlist {
		// The container's own record says which network it joined; the network's record says
		// whether that network is what the design needs; the proxy's record says whether the
		// neighbour is the proxy; the container list says who else is attached. All must hold.
		nv, err := d.auditEgress(ctx, s)
		if err != nil {
			return Result{}, err
		}
		violations = append(violations, nv...)
	}
	if err := AuditError(s.Name, violations); err != nil {
		return Result{}, err // never started
	}

	runCtx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	var out, errOut cappedBuffer
	var watch *egressWatch
	if s.Network == NetworkAllowlist {
		watch = d.watchEgress(s)
	}
	_, runErr := d.Exec.Run(runCtx, execx.Cmd{Name: d.bin(), Args: []string{"start", "-a", "-i", s.Name}, Stdin: s.Stdin, Stdout: &out, Stderr: &errOut})

	var watchViolations []string
	if watch != nil {
		watchViolations = watch.stop()
	}
	res := Result{Stdout: out.String(), Stderr: errOut.String()}
	if runCtx.Err() != nil { // the timeout (or the caller) cut the job off: stop the container itself
		res.TimedOut = errors.Is(runCtx.Err(), context.DeadlineExceeded)
		kctx, kcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer kcancel()
		_, _ = d.docker(kctx, "", "kill", s.Name)
	}
	var ee *execx.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &ee):
		res.ExitCode = ee.Result.ExitCode
	case res.TimedOut:
	default:
		return res, diag.Wrap(runErr, diag.CodeIsolation, "the job could not be run", "Docker failed while starting it", "check Docker")
	}
	sctx, scancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer scancel()
	if st, err := d.docker(sctx, "", "inspect", "--format", "{{.State.OOMKilled}} {{.State.ExitCode}}", s.Name); err == nil {
		f := strings.Fields(st.Stdout)
		if len(f) == 2 {
			res.OOMKilled = f[0] == "true"
			if code, err := strconv.Atoi(f[1]); err == nil && !res.TimedOut {
				res.ExitCode = code
			}
		}
	}
	if len(watchViolations) > 0 { // the job ran on a network that stopped being the design's: its result is not to be trusted
		return res, AuditError(s.Name, watchViolations)
	}
	return res, nil
}

// auditEgress reads the egress network, its attached containers (including ones that are only
// created) and the proxy back from Docker and returns every way they are not the design.
func (d *Docker) auditEgress(ctx context.Context, s Spec) ([]string, error) {
	fail := func(err error, what, cause, fix string) ([]string, error) {
		return nil, diag.Wrap(err, diag.CodeIsolation, what, cause, fix)
	}
	nw, err := d.docker(ctx, "", "network", "inspect", s.EgressNetwork)
	if err != nil {
		return fail(err, "cannot read back the egress network's configuration", "the network is gone or Docker stopped answering", "check `docker network ls`")
	}
	v, err := AuditNetwork([]byte(nw.Stdout), s)
	if err != nil {
		return fail(err, "cannot audit the egress network", "an unexpected Docker version", "update Docker")
	}
	ps, err := d.docker(ctx, "", "ps", "-a", "--filter", "network="+s.EgressNetwork, "--format", "{{.Names}}")
	if err != nil {
		return fail(err, "cannot list the containers on the egress network", "Docker stopped answering", "check Docker")
	}
	v = append(v, AuditMembers(strings.Fields(ps.Stdout), s)...)
	px, err := d.docker(ctx, "", "inspect", s.EgressProxyContainer)
	if err != nil {
		return fail(err, "cannot read back the egress proxy's configuration", "the proxy is gone or Docker stopped answering", "check `docker ps -a`")
	}
	pv, err := AuditProxy([]byte(px.Stdout), []byte(nw.Stdout), s)
	if err != nil {
		return fail(err, "cannot audit the egress proxy", "an unexpected Docker version", "update Docker")
	}
	return append(v, pv...), nil
}

// egressWatch re-audits the egress topology while a job runs, and kills the job the moment it
// is no longer the design (an unexpected container joined its network, the proxy changed).
type egressWatch struct {
	cancel context.CancelFunc
	done   chan struct{}
	d      *Docker
	s      Spec

	mu         sync.Mutex
	violations []string
}

func (d *Docker) watchEgress(s Spec) *egressWatch {
	ctx, cancel := context.WithCancel(context.Background())
	w := &egressWatch{cancel: cancel, done: make(chan struct{}), d: d, s: s}
	every := d.RecheckEvery
	if every <= 0 {
		every = 500 * time.Millisecond
	}
	go func() {
		defer close(w.done)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if w.check(ctx) {
					kctx, kcancel := context.WithTimeout(context.Background(), 30*time.Second)
					_, _ = d.docker(kctx, "", "kill", s.Name)
					kcancel()
					return
				}
			}
		}
	}()
	return w
}

// check audits once and records what it found; it reports whether the job must be stopped. A
// topology that cannot be read is treated as a violation: not knowing is not a pass.
func (w *egressWatch) check(ctx context.Context) bool {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	v, err := w.d.auditEgress(cctx, w.s)
	if err != nil {
		if ctx.Err() != nil {
			return false // we were told to stop: that is not a finding
		}
		v = []string{egressPrefix + "cannot re-audit the egress network while the job runs: " + err.Error()}
	}
	if len(v) == 0 {
		return false
	}
	w.mu.Lock()
	w.violations = append(w.violations, v...)
	w.mu.Unlock()
	return true
}

// stop ends the watch, audits one last time, and returns every violation seen during the run.
func (w *egressWatch) stop() []string {
	w.cancel()
	<-w.done
	w.mu.Lock()
	found := len(w.violations) > 0
	w.mu.Unlock()
	if !found {
		w.check(context.Background())
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.violations
}

// RemoveStale removes every container this package created, for use at start-up: a crash can
// leave one behind, still holding memory and CPU.
func (d *Docker) RemoveStale(ctx context.Context) (int, error) {
	res, err := d.docker(ctx, "", "ps", "-aq", "--filter", "label="+LabelJob)
	if err != nil {
		return 0, diag.Wrap(err, diag.CodeIsolation, "cannot list stale job containers", "Docker is not answering", "check Docker")
	}
	ids := strings.Fields(res.Stdout)
	if len(ids) == 0 {
		return 0, nil
	}
	if _, err := d.docker(ctx, "", append([]string{"rm", "-f", "-v"}, ids...)...); err != nil {
		return 0, diag.Wrap(err, diag.CodeIsolation, "cannot remove stale job containers", "Docker refused", "run `docker rm -f` on them by hand")
	}
	return len(ids), nil
}
