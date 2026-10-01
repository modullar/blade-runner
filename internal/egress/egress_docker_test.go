package egress

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/dockerlock"
	"github.com/modullar/blade-runner/internal/execx"
	"github.com/modullar/blade-runner/internal/isolation"
)

// These tests run the real topology on the real Docker daemon: a job in a real container on a
// real internal network, with the proxy in its own container. They skip, with the reason, when
// there is no daemon. Registry pulls are not needed: the images are FROM scratch and hold small
// static Go programs (testdata/probe, testdata/proxyfixture, cmd/egress-proxy).

var ctx = context.Background()

var (
	imgOnce sync.Once
	imgID   string
	imgErr  error
	jobs    = &isolation.Docker{Exec: execx.OS{}}
)

func needDocker(t *testing.T) string {
	t.Helper()
	if _, err := jobs.Preflight(ctx); err != nil {
		t.Skipf("no usable Docker daemon: %v", err)
	}
	dockerlock.Lock(t)
	imgOnce.Do(func() {
		dir, err := os.MkdirTemp("", "egress-img-")
		if err != nil {
			imgErr = err
			return
		}
		for out, pkg := range map[string]string{
			"probe":        "github.com/modullar/blade-runner/internal/egress/testdata/probe",
			"proxyfixture": "github.com/modullar/blade-runner/internal/egress/testdata/proxyfixture",
			"egress-proxy": "github.com/modullar/blade-runner/internal/egress/cmd/egress-proxy",
		} {
			build := exec.Command("go", "build", "-o", filepath.Join(dir, out), pkg)
			build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
			if o, err := build.CombinedOutput(); err != nil {
				imgErr = fmt.Errorf("build %s: %v\n%s", pkg, err, o)
				return
			}
		}
		_ = os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\nCOPY probe /probe\nCOPY proxyfixture /proxyfixture\nCOPY egress-proxy /egress-proxy\nENTRYPOINT [\"/probe\"]\n"), 0o644)
		if o, err := exec.Command("docker", "build", "-q", "-t", "br-egress-test", dir).CombinedOutput(); err != nil {
			imgErr = fmt.Errorf("docker build: %v\n%s", err, o)
			return
		}
		o, err := exec.Command("docker", "image", "inspect", "--format", "{{.Id}}", "br-egress-test").Output()
		imgID, imgErr = strings.TrimSpace(string(o)), err
	})
	if imgErr != nil {
		t.Fatalf("cannot prepare the test image: %v", imgErr)
	}
	return imgID
}

func dockerCLI(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

var (
	seqMu sync.Mutex
	seq   int
)

func uniq(prefix string) string {
	seqMu.Lock()
	defer seqMu.Unlock()
	seq++
	return fmt.Sprintf("%s%d-%d", prefix, os.Getpid()%100000, seq)
}

// hostService is a real listener on the Docker HOST, on every address: the service a job must
// never reach except through the proxy. It counts connections.
type hostService struct {
	l        net.Listener
	port     int
	accepted atomic.Int64
}

func newHostService(t *testing.T) *hostService {
	t.Helper()
	l, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	h := &hostService{l: l, port: l.Addr().(*net.TCPAddr).Port}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			h.accepted.Add(1)
			io.WriteString(c, "hello from host\n")
			c.Close()
		}
	}()
	return h
}

type outNet struct{ Subnet, Gateway string }

// outNetwork makes sure the shared outbound network exists and reports its subnet and gateway
// (the address the host has on it, which is where host services are reached from the proxy).
func outNetwork(t *testing.T, m *Manager) outNet {
	t.Helper()
	if err := m.ensureOut(ctx); err != nil {
		t.Fatal(err)
	}
	var cfg []outNet
	if err := json.Unmarshal([]byte(dockerCLI(t, "network", "inspect", "--format", "{{json .IPAM.Config}}", OutNetwork)), &cfg); err != nil || len(cfg) == 0 || cfg[0].Gateway == "" {
		t.Fatalf("cannot read %s: %v %v", OutNetwork, cfg, err)
	}
	return cfg[0]
}

// fixtureManager is a Manager whose proxy is the TEST build: a fixed name table, and the
// outbound network's subnet standing in for "the internet". Allowed names map to the host.
func fixtureManager(t *testing.T, img string, h *hostService, names ...string) (*Manager, outNet) {
	t.Helper()
	m := &Manager{Exec: execx.OS{}, ProxyImage: img, ProxyEntrypoint: "/proxyfixture"}
	out := outNetwork(t, m)
	m.Allow = allow(t, names...)
	m.Ports = []int{h.port}
	m.ProxyExtraArgs = []string{"-trust-cidr", out.Subnet}
	for _, n := range names {
		m.ProxyExtraArgs = append(m.ProxyExtraArgs, "-resolve", n+"="+out.Gateway)
	}
	return m, out
}

func open(t *testing.T, m *Manager) *Session {
	t.Helper()
	s, err := m.Open(ctx, uniq("t"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(ctx) })
	return s
}

func job(t *testing.T, img string, s *Session, args ...string) isolation.Result {
	t.Helper()
	res, err := runJob(img, s, jobs, args...)
	if err != nil {
		t.Fatalf("job %v: %v", args, err)
	}
	return res
}

func runJob(img string, s *Session, d *isolation.Docker, args ...string) (isolation.Result, error) {
	spec := isolation.Spec{Name: uniq("br-egj-"), Image: img, Args: args, Timeout: 60 * time.Second, MemoryMiB: 128, PidsLimit: 64, WorkTmpfsMiB: 16}
	applyAgain(s, &spec)
	return d.Run(ctx, spec)
}

func blocked(t *testing.T, what string, res isolation.Result) {
	t.Helper()
	t.Logf("%s: %s", what, strings.TrimSpace(res.Stdout))
	if !strings.HasPrefix(res.Stdout, "BLOCKED") {
		t.Errorf("%s: a job was ALLOWED through: %q", what, res.Stdout)
	}
}

// hostAddresses are the IPv4 addresses the test machine itself has: loopback excluded (that is
// the container's own loopback), everything else a service on the host could be reached at.
func hostAddresses(t *testing.T) []string {
	t.Helper()
	var out []string
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() {
			out = append(out, ipn.IP.String())
		}
	}
	return out
}

func TestRealDocker_AllowedTargetIsReachableOnlyThroughTheProxy(t *testing.T) {
	img := needDocker(t)
	h := newHostService(t)
	m, out := fixtureManager(t, img, h, "allowed.test")
	s := open(t, m)
	port := strconv.Itoa(h.port)

	t.Run("through the proxy it is reachable", func(t *testing.T) {
		res := job(t, img, s, "via-proxy", s.ProxyAddr, "allowed.test:"+port)
		if !strings.HasPrefix(res.Stdout, "ALLOWED") || !strings.Contains(res.Stdout, "hello from host") {
			t.Fatalf("via the proxy: %q (stderr %q)", res.Stdout, res.Stderr)
		}
	})
	t.Run("the very same address and port, directly, is not", func(t *testing.T) {
		before := h.accepted.Load()
		blocked(t, "direct to the host service via the outbound gateway", job(t, img, s, "connect", out.Gateway+":"+port))
		if h.accepted.Load() != before {
			t.Error("the host service saw a direct connection")
		}
	})
	t.Run("the host, its gateways and loopback are unreachable directly", func(t *testing.T) {
		targets := append([]string{"127.0.0.1:" + port, "172.17.0.1:" + port}, func() (l []string) {
			for _, a := range hostAddresses(t) {
				l = append(l, a+":"+port)
			}
			return
		}()...)
		before := h.accepted.Load()
		for _, target := range targets {
			blocked(t, "direct to "+target, job(t, img, s, "connect", target))
		}
		if h.accepted.Load() != before {
			t.Errorf("the host service saw %d direct connections", h.accepted.Load()-before)
		}
	})
	t.Run("cloud metadata and the internet are unreachable directly", func(t *testing.T) {
		for _, target := range []string{"169.254.169.254:80", "169.254.169.254:443", "1.1.1.1:443", "8.8.8.8:53"} {
			blocked(t, "direct to "+target, job(t, img, s, "connect", target))
		}
	})
	t.Run("the job has no default route and no DNS to the outside", func(t *testing.T) {
		routes := job(t, img, s, "routes").Stdout
		for _, line := range strings.Split(routes, "\n")[1:] {
			if f := strings.Fields(line); len(f) > 1 && f[1] == "00000000" {
				t.Errorf("the job has a default route: %q", line)
			}
		}
		blocked(t, "resolving an outside name", job(t, img, s, "lookup", "example.com"))
	})
	t.Run("the job is told about the proxy and about nothing that bypasses it", func(t *testing.T) {
		env := job(t, img, s, "env").Stdout
		for _, want := range []string{"HTTPS_PROXY=http://" + s.ProxyAddr, "https_proxy=http://" + s.ProxyAddr, "NO_PROXY=\n"} {
			if !strings.Contains(env, want) {
				t.Errorf("the job's environment lacks %q:\n%s", want, env)
			}
		}
	})
}

func TestRealDocker_AForbiddenHostnameIsRefusedAndRecorded(t *testing.T) {
	img := needDocker(t)
	h := newHostService(t)
	m, _ := fixtureManager(t, img, h, "allowed.test")
	s := open(t, m)
	port := strconv.Itoa(h.port)

	want := map[string]string{ // target -> the reason the proxy must record
		"forbidden.test:" + port: ReasonNotAllowlist,
		"169.254.169.254:80":     ReasonIPLiteral,
		"127.0.0.1:" + port:      ReasonIPLiteral,
		"allowed.test:22":        ReasonBadPort,
		"x.allowed.test:" + port: ReasonNotAllowlist,
	}
	before := h.accepted.Load()
	for target := range want {
		res := job(t, img, s, "via-proxy", s.ProxyAddr, target)
		if !strings.HasPrefix(res.Stdout, "BLOCKED") || !strings.Contains(res.Stdout, " 403 ") {
			t.Errorf("%s: %q, want a 403 refusal", target, res.Stdout)
		}
	}
	if got := job(t, img, s, "http-get", s.ProxyAddr, "http://allowed.test/").Stdout; !strings.Contains(got, " 405 ") {
		t.Errorf("a plain HTTP request through the proxy: %q, want 405", got)
	}
	if h.accepted.Load() != before {
		t.Error("a refused request reached the host service")
	}

	ds, err := s.Decisions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string]string{}
	for _, d := range ds {
		if !d.Allowed {
			reasons[d.Host+":"+d.Port] = d.Reason
		}
	}
	for target, reason := range want {
		if reasons[target] != reason {
			t.Errorf("the proxy's record for %s is %q, want %q (all: %v)", target, reasons[target], reason, reasons)
		}
	}
}

func TestRealDocker_TheProductionProxyRefusesAnAllowedNameThatResolvesInside(t *testing.T) {
	img := needDocker(t)
	h := newHostService(t)
	m0 := &Manager{Exec: execx.OS{}}
	outNetwork(t, m0)
	// A real neighbour of the proxies on the outbound network, reachable by NAME through Docker's
	// own DNS: an allowlisted name that resolves to a private address, as a hostile or careless
	// DNS answer would. The PRODUCTION proxy (no fixture flags) must refuse it.
	victim := uniq("br-egv-")
	dockerCLI(t, "run", "-d", "--name", victim, "--network", OutNetwork, "--entrypoint", "/probe", img, "listen", "9000")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", victim).Run() })
	m := &Manager{Exec: execx.OS{}, ProxyImage: img, Allow: allow(t, victim), Ports: []int{9000}}
	s := open(t, m)
	res := job(t, img, s, "via-proxy", s.ProxyAddr, victim+":9000")
	if !strings.HasPrefix(res.Stdout, "BLOCKED") || !strings.Contains(res.Stdout, " 403 ") {
		t.Fatalf("through the production proxy: %q", res.Stdout)
	}
	ds, err := s.Decisions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range ds {
		if d.Host == victim && !d.Allowed && d.Reason == ReasonForbiddenAddr && len(d.Resolved) > 0 {
			found = true
		}
	}
	if !found {
		t.Errorf("the proxy's record should show %s refused as forbidden-address: %+v", victim, ds)
	}
	// The name is asked for as an absolute name, so /etc/hosts does not apply: "localhost" is not
	// resolved from the proxy container's hosts file (where it would be refused as loopback
	// anyway) but asked of DNS, which has no such name. Either way nothing is reachable.
	m1 := &Manager{Exec: execx.OS{}, ProxyImage: img, Allow: allow(t, "localhost"), Ports: []int{h.port}}
	s1 := open(t, m1)
	if res := job(t, img, s1, "via-proxy", s1.ProxyAddr, "localhost:"+strconv.Itoa(h.port)); !strings.HasPrefix(res.Stdout, "BLOCKED") {
		t.Errorf("localhost through the production proxy: %q", res.Stdout)
	}
	// And that production proxy really is the production policy: asked for an allowlisted name
	// that does not resolve at all, it fails closed.
	m2 := &Manager{Exec: execx.OS{}, ProxyImage: img, Allow: allow(t, "no-such-name.invalid"), Ports: []int{443}, StartTimeout: 30 * time.Second}
	s2 := open(t, m2)
	if res := job(t, img, s2, "via-proxy", s2.ProxyAddr, "no-such-name.invalid:443"); !strings.Contains(res.Stdout, " 502 ") {
		t.Errorf("an unresolvable name: %q, want 502", res.Stdout)
	}
}

func TestRealDocker_NeighboursAndOtherSessionsAreUnreachable(t *testing.T) {
	img := needDocker(t)
	h := newHostService(t)
	m, _ := fixtureManager(t, img, h, "allowed.test")
	a, b := open(t, m), open(t, m)

	// A neighbour on Docker's default bridge, listening, standing in for "another container".
	name := uniq("br-egn-")
	dockerCLI(t, "run", "-d", "--name", name, "--entrypoint", "/probe", img, "listen", "9000")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	neighbour := dockerCLI(t, "inspect", "--format", "{{(index .NetworkSettings.Networks \"bridge\").IPAddress}}", name)
	if net.ParseIP(neighbour) == nil {
		t.Fatalf("no neighbour address: %q", neighbour)
	}
	if res := job(t, img, a, "connect", neighbour+":9000"); !strings.HasPrefix(res.Stdout, "BLOCKED") {
		t.Errorf("a job reached another container: %q", res.Stdout)
	}
	// Sanity: the neighbour really is serving (reached from the host).
	if c, err := net.DialTimeout("tcp", neighbour+":9000", 3*time.Second); err != nil {
		t.Errorf("the neighbour is not actually listening, so the test above proved nothing: %v", err)
	} else {
		c.Close()
	}

	// Another job's proxy, by its internal address, and this job's own proxy on its OUTBOUND address.
	if res := job(t, img, b, "connect", a.ProxyAddr); !strings.HasPrefix(res.Stdout, "BLOCKED") {
		t.Errorf("a job reached another session's proxy: %q", res.Stdout)
	}
	outIP := dockerCLI(t, "inspect", "--format", "{{(index .NetworkSettings.Networks \""+OutNetwork+"\").IPAddress}}", a.ProxyContainer)
	if net.ParseIP(outIP) == nil {
		t.Fatalf("no outbound address: %q", outIP)
	}
	if res := job(t, img, a, "connect", outIP+":"+strconv.Itoa(ProxyPort)); !strings.HasPrefix(res.Stdout, "BLOCKED") {
		t.Errorf("a job reached its proxy on the outbound network: %q", res.Stdout)
	}
	// The proxy listens on its job-facing address only: reached on the outbound one from a
	// neighbour of the proxies (the host is one), it does not answer.
	if c, err := net.DialTimeout("tcp", outIP+":"+strconv.Itoa(ProxyPort), 2*time.Second); err == nil {
		c.Close()
		t.Error("the proxy accepts connections on its outbound address")
	}
}

// WHY the design needs inhibit_ipv4, recorded as a test like the "bridge" known limit in
// internal/isolation: on a plain `--internal` network the job has no route out, but the host's
// own bridge address is still on that network, so a service the host listens for on every
// address answers the job. If a future Docker closes this, the test says so and fails nothing:
// update docs/decisions/0008-egress.md.
func TestRealDocker_PlainInternalNetworkStillReachesTheHost_WhyTheDesignNeedsInhibitIPv4(t *testing.T) {
	img := needDocker(t)
	h := newHostService(t)
	name := uniq("br-egw-")
	dockerCLI(t, "network", "create", "--internal", name)
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", name).Run() })
	gw := dockerCLI(t, "network", "inspect", "--format", "{{(index .IPAM.Config 0).Gateway}}", name)
	out := dockerCLI(t, "run", "--rm", "--network", name, img, "connect", gw+":"+strconv.Itoa(h.port))
	t.Logf("plain --internal network, job -> host bridge address %s: %s", gw, out)
	if strings.HasPrefix(out, "ALLOWED") {
		t.Log("CONFIRMED: --internal alone leaves the host reachable; the design sets com.docker.network.bridge.inhibit_ipv4")
	} else {
		t.Log("this Docker no longer exposes the host on a plain internal network: update docs/decisions/0008-egress.md")
	}
}

// ---- the audit refuses what is not the design ---------------------------------------------

// editCreate runs the real docker CLI but changes the `create` command, as a runtime that
// ignored or altered a flag would; after, if set, runs once the container exists.
type editCreate struct {
	inner execx.Runner
	edit  func(args []string) []string
	after func(args []string)
}

func (e editCreate) Run(c context.Context, cmd execx.Cmd) (execx.Result, error) {
	if len(cmd.Args) > 0 && cmd.Args[0] == "create" {
		if e.edit != nil {
			cmd.Args = e.edit(cmd.Args)
		}
		res, err := e.inner.Run(c, cmd)
		if err == nil && e.after != nil {
			e.after(cmd.Args)
		}
		return res, err
	}
	return e.inner.Run(c, cmd)
}
func (e editCreate) LookPath(n string) (string, error) { return e.inner.LookPath(n) }

// skipVerb runs the real docker CLI but silently does not run one sub-command, as a runtime
// that did not apply it would.
type skipVerb struct {
	inner execx.Runner
	verb  []string // e.g. {"network", "connect"}
}

func (s skipVerb) Run(c context.Context, cmd execx.Cmd) (execx.Result, error) {
	if len(cmd.Args) >= len(s.verb) && strings.Join(cmd.Args[:len(s.verb)], " ") == strings.Join(s.verb, " ") {
		return execx.Result{}, nil
	}
	return s.inner.Run(c, cmd)
}
func (s skipVerb) LookPath(n string) (string, error) { return s.inner.LookPath(n) }

func TestRealDocker_TheProxyItselfIsAuditedBeforeItStarts(t *testing.T) {
	img := needDocker(t)
	cleanSlate(t)
	h := newHostService(t)
	for name, tc := range map[string]struct {
		exec execx.Runner
		want string
	}{
		"a writable root filesystem": {editCreate{inner: execx.OS{}, edit: func(a []string) []string {
			var o []string
			for _, x := range a {
				if x != "--read-only" {
					o = append(o, x)
				}
			}
			return o
		}}, "writable root filesystem"},
		"not attached to the outbound network": {skipVerb{inner: execx.OS{}, verb: []string{"network", "connect"}}, "exactly"},
		"running as root": {editCreate{inner: execx.OS{}, edit: func(a []string) []string {
			var o []string
			for i, x := range a {
				if i > 0 && a[i-1] == "--user" {
					x = "0:0"
				}
				o = append(o, x)
			}
			return o
		}}, "runs as root"},
	} {
		m, _ := fixtureManager(t, img, h, "allowed.test")
		m.Exec = tc.exec
		_, err := m.Open(ctx, uniq("a"))
		if diag.CodeOf(err) != diag.CodeEgressSetup || !strings.Contains(err.Error(), "NOT started") || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want a BR-E082 refusal naming %q", name, err, tc.want)
		}
		if c, n := leftovers(t); c != "" || n != "" {
			t.Errorf("%s: left %q %q behind", name, c, n)
		}
	}
}

func TestRealDocker_ARefusedJobNeverStartsAndLeavesNothingBehind(t *testing.T) {
	img := needDocker(t)
	h := newHostService(t)
	m, out := fixtureManager(t, img, h, "allowed.test")
	s := open(t, m)
	port := strconv.Itoa(h.port)

	cases := []struct {
		name string
		run  editCreate
		want string
	}{
		{"a runtime that put the job on the default bridge",
			editCreate{edit: func(a []string) []string {
				var o []string
				for i, x := range a {
					if x == s.Network && i > 0 && a[i-1] == "--network" {
						x = "bridge"
					}
					o = append(o, x)
				}
				return o
			}}, "not the egress network"},
		{"an extra host entry pointing a name at the host",
			editCreate{edit: func(a []string) []string {
				return append([]string{a[0], "--add-host", "github.com:" + out.Gateway}, a[1:]...)
			}},
			"extra host entries"},
		{"a second network (a bridge with a route out)",
			editCreate{after: func(a []string) {
				name := a[2]
				_ = exec.Command("docker", "network", "connect", "bridge", name).Run()
			}}, "attached to 2 networks"},
		{"a proxy setting pointing elsewhere",
			editCreate{edit: func(a []string) []string {
				var o []string
				for _, x := range a {
					if strings.HasPrefix(x, "HTTPS_PROXY=") {
						x = "HTTPS_PROXY=http://" + out.Gateway + ":" + port
					}
					o = append(o, x)
				}
				return o
			}}, "proxy setting HTTPS_PROXY"},
		{"a host exempted from the proxy",
			editCreate{edit: func(a []string) []string {
				var o []string
				for _, x := range a {
					if x == "NO_PROXY=" {
						x = "NO_PROXY=" + out.Gateway
					}
					o = append(o, x)
				}
				return o
			}}, "proxy setting NO_PROXY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.run.inner = execx.OS{}
			spec := isolation.Spec{Name: uniq("br-egr-"), Image: img, Args: []string{"connect", out.Gateway + ":" + port}, Timeout: 30 * time.Second, MemoryMiB: 128, PidsLimit: 32}
			applyAgain(s, &spec)
			before := h.accepted.Load()
			res, err := (&isolation.Docker{Exec: tc.run}).Run(ctx, spec)
			if diag.CodeOf(err) != diag.CodeEgressSetup || !strings.Contains(err.Error(), "NOT started") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a BR-E082 refusal naming %q", err, tc.want)
			}
			if res.Stdout != "" {
				t.Errorf("the job ran (%q) although it failed the audit", res.Stdout)
			}
			if h.accepted.Load() != before {
				t.Error("a refused job reached the host service")
			}
			if left, _ := exec.Command("docker", "ps", "-aq", "--filter", "name="+spec.Name).Output(); strings.TrimSpace(string(left)) != "" {
				t.Errorf("the refused container %s was not removed", spec.Name)
			}
		})
	}
}

func TestRealDocker_TheNetworkAuditRefusesNetworksThatAreNotTheDesign(t *testing.T) {
	img := needDocker(t)
	h := newHostService(t)
	m, _ := fixtureManager(t, img, h, "allowed.test")
	s := open(t, m)

	try := func(t *testing.T, network string, wantInErr ...string) {
		t.Helper()
		spec := isolation.Spec{Name: uniq("br-egn-"), Image: img, Args: []string{"routes"}, Timeout: 30 * time.Second, MemoryMiB: 128, PidsLimit: 32}
		applyAgain(s, &spec)
		spec.EgressNetwork = network
		res, err := jobs.Run(ctx, spec)
		if diag.CodeOf(err) != diag.CodeEgressSetup {
			t.Fatalf("err = %v, want a BR-E082 refusal", err)
		}
		for _, w := range wantInErr {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("refusal should mention %q:\n%v", w, err)
			}
		}
		if res.Stdout != "" {
			t.Errorf("the job ran: %q", res.Stdout)
		}
	}

	t.Run("a plain --internal network: the host keeps an address on it", func(t *testing.T) {
		// This is the hole the design closes: without inhibit_ipv4 the host's bridge address is
		// reachable from the job even on an internal network (see the ADR).
		name := uniq("br-egt-")
		dockerCLI(t, "network", "create", "--internal", name)
		t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", name).Run() })
		try(t, name, "inhibit_ipv4", "the host owns an address")
	})
	t.Run("an ordinary bridge: a route out that bypasses the proxy", func(t *testing.T) {
		name := uniq("br-egt-")
		dockerCLI(t, "network", "create", name)
		t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", name).Run() })
		try(t, name, "not internal")
	})
	t.Run("the default bridge cannot even be named as the egress network", func(t *testing.T) {
		spec := isolation.Spec{Name: uniq("br-egn-"), Image: img, Args: []string{"routes"}, Timeout: 30 * time.Second, MemoryMiB: 128, PidsLimit: 32}
		applyAgain(s, &spec)
		spec.EgressNetwork = "bridge"
		if _, err := jobs.Run(ctx, spec); diag.CodeOf(err) != diag.CodeIsolation {
			t.Fatalf("err = %v, want a BR-E068 refusal before anything is created", err)
		}
		if containerExists(spec.Name) {
			t.Error("a container was created")
		}
	})
	t.Run("an intruder sharing the network", func(t *testing.T) {
		name := uniq("br-egi-")
		dockerCLI(t, "run", "-d", "--name", name, "--network", s.Network, "--entrypoint", "/probe", img, "sleep")
		t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
		try(t, s.Network, "another member, "+name)
	})
	t.Run("the proxy is not running", func(t *testing.T) {
		dockerCLI(t, "stop", "-t", "1", s.ProxyContainer)
		try(t, s.Network, "is not running")
	})
}

// ---- lifecycle ---------------------------------------------------------------------------

// cleanSlate removes egress sessions an earlier (crashed or failed) run left on the daemon, so a
// test that counts them starts from nothing instead of skipping. The docker lock means no other
// test process of this repo has live sessions.
func cleanSlate(t *testing.T) {
	t.Helper()
	if _, err := (&Manager{Exec: execx.OS{}}).Sweep(ctx); err != nil {
		t.Fatalf("cannot clear stale sessions: %v", err)
	}
	if c, n := leftovers(t); c != "" || n != "" {
		t.Fatalf("sessions remain after a sweep: %q %q", c, n)
	}
}

func leftovers(t *testing.T) (containers, networks string) {
	t.Helper()
	f := "label=" + LabelKey + "=" + labelSession
	return dockerCLI(t, "ps", "-aq", "--filter", f), dockerCLI(t, "network", "ls", "-q", "--filter", f)
}

func TestRealDocker_CloseAndSweepRemoveEverythingAndAreIdempotent(t *testing.T) {
	img := needDocker(t)
	h := newHostService(t)
	m, _ := fixtureManager(t, img, h, "allowed.test")
	cleanSlate(t)

	s, err := m.Open(ctx, uniq("c"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(ctx); err != nil {
		t.Errorf("a second Close must be a no-op: %v", err)
	}
	if c, n := leftovers(t); c != "" || n != "" {
		t.Errorf("Close left %q %q", c, n)
	}

	// A crash: sessions that were never closed. Sweep is what the next start runs.
	for i := 0; i < 2; i++ {
		if _, err := m.Open(ctx, uniq("x")); err != nil {
			t.Fatal(err)
		}
	}
	n, err := m.Sweep(ctx)
	if err != nil || n != 4 {
		t.Errorf("Sweep removed %d (%v), want 2 proxies + 2 networks", n, err)
	}
	if c, nn := leftovers(t); c != "" || nn != "" {
		t.Errorf("Sweep left %q %q", c, nn)
	}
	if n, err := m.Sweep(ctx); err != nil || n != 0 {
		t.Errorf("a second sweep: %d %v", n, err)
	}
	if dockerCLI(t, "network", "ls", "-q", "--filter", "name="+OutNetwork) == "" {
		t.Error("the shared outbound network must survive a sweep")
	}
}

func TestRealDocker_OpenFailsCleanly(t *testing.T) {
	img := needDocker(t)
	cleanSlate(t)
	// An image that is not pinned never reaches Docker; one that is pinned but absent fails at
	// create, after the network exists, and must take the network down with it.
	for name, m := range map[string]*Manager{
		"an unpinned image": {Exec: execx.OS{}, ProxyImage: "br-egress-test:latest", Allow: allow(t, "github.com")},
		"an absent image":   {Exec: execx.OS{}, ProxyImage: "sha256:" + strings.Repeat("0", 64), Allow: allow(t, "github.com")},
		"a missing binary":  {Exec: execx.OS{}, ProxyImage: img, ProxyEntrypoint: "/nonexistent", Allow: allow(t, "github.com"), StartTimeout: 3 * time.Second},
	} {
		if _, err := m.Open(ctx, uniq("f")); diag.CodeOf(err) != diag.CodeEgressSetup {
			t.Errorf("%s: err = %v, want BR-E082", name, err)
		}
		if c, n := leftovers(t); c != "" || n != "" {
			t.Errorf("%s: left %q %q behind", name, c, n)
		}
	}
	// Policy errors are BR-E081 and need no Docker at all.
	if _, err := (&Manager{Exec: execx.OS{}, ProxyImage: img}).Open(ctx, "p1"); diag.CodeOf(err) != diag.CodeEgressPolicyInvalid {
		t.Errorf("empty allowlist: %v", err)
	}
	if _, err := (&Manager{Exec: execx.OS{}, ProxyImage: img, Allow: allow(t, "github.com"), Ports: []int{70000}}).Open(ctx, "p2"); diag.CodeOf(err) != diag.CodeEgressPolicyInvalid {
		t.Errorf("bad port: %v", err)
	}
	if _, err := (&Manager{Exec: execx.OS{}, ProxyImage: img, Allow: allow(t, "github.com")}).Open(ctx, "Bad ID"); diag.CodeOf(err) != diag.CodeEgressSetup {
		t.Errorf("bad id: %v", err)
	}
}

// applyAgain hands the session to another job. Production never does that (Apply refuses a
// second job); these tests probe ONE topology with many short jobs in sequence, and the Docker
// side of the rule (a network with anything besides the proxy and that job is refused) is still
// in force for each of them.
func applyAgain(s *Session, spec *isolation.Spec) {
	s.claimed.Store(false)
	_ = s.Apply(spec)
}

// ---- collisions, one job per session, proxy identity ---------------------------------------

func containerExists(name string) bool {
	out, _ := exec.Command("docker", "ps", "-aq", "--filter", "name=^"+name+"$").Output()
	return strings.TrimSpace(string(out)) != ""
}

func networkExists(name string) bool {
	out, _ := exec.Command("docker", "network", "ls", "-q", "--filter", "name=^"+name+"$").Output()
	return strings.TrimSpace(string(out)) != ""
}

func TestRealDocker_AFailedOpenOnANameCollisionLeavesTheLiveSessionAlone(t *testing.T) {
	img := needDocker(t)
	h := newHostService(t)
	m, _ := fixtureManager(t, img, h, "allowed.test")
	live := open(t, m)

	if s, err := m.Open(ctx, live.ID); err == nil {
		_ = s.Close(ctx)
		t.Fatal("Open with an id that is already in use must fail")
	} else if diag.CodeOf(err) != diag.CodeEgressSetup {
		t.Errorf("err = %v, want BR-E082", err)
	}

	// The live session must be exactly as it was: proxy running, network there, and usable.
	if got := dockerCLI(t, "inspect", "--format", "{{.State.Running}}", live.ProxyContainer); got != "true" {
		t.Fatalf("the live session's proxy is running = %q after another Open failed on its name", got)
	}
	if !networkExists(live.Network) {
		t.Fatal("the live session's network was removed by another Open's failure")
	}
	res := job(t, img, live, "via-proxy", live.ProxyAddr, "allowed.test:"+strconv.Itoa(h.port))
	if !strings.HasPrefix(res.Stdout, "ALLOWED") {
		t.Errorf("the live session no longer works: %q", res.Stdout)
	}
}

func TestRealDocker_AFailedOpenRemovesOnlyWhatItCreated(t *testing.T) {
	img := needDocker(t)
	h := newHostService(t)
	m, _ := fixtureManager(t, img, h, "allowed.test")

	// A bystander already holds the proxy's name (nothing to do with egress). Open creates the
	// network, fails at the proxy, and must remove its own network but not the bystander.
	id := uniq("b")
	bystander := "br-egress-" + id + "-proxy"
	dockerCLI(t, "create", "--name", bystander, "--entrypoint", "/probe", img, "sleep")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", bystander).Run() })

	if s, err := m.Open(ctx, id); err == nil {
		_ = s.Close(ctx)
		t.Fatal("Open must fail when the proxy's name is taken")
	}
	if !containerExists(bystander) {
		t.Error("Open's failure removed a container it did not create")
	}
	if networkExists("br-egress-" + id + "-net") {
		t.Error("Open's failure left the network it created behind")
	}
}

func TestSessionServesOneJobAndASecondApplyCannotProduceARunnableSpec(t *testing.T) {
	s := &Session{ID: "x", Network: "br-egress-x-net", ProxyContainer: "br-egress-x-proxy", ProxyAddr: "172.19.0.1:3128"}
	img := "sha256:" + strings.Repeat("a", 64)

	first := isolation.Spec{Name: "job-1", Image: img}
	if err := s.Apply(&first); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Validate(); err != nil {
		t.Fatalf("the first spec must be valid: %v", err)
	}
	if first.EgressNetwork != s.Network || first.EgressProxy != s.ProxyAddr || first.EgressProxyContainer != s.ProxyContainer {
		t.Errorf("spec = %+v", first)
	}

	second := isolation.Spec{Name: "job-2", Image: img}
	if err := s.Apply(&second); err != ErrSessionInUse {
		t.Fatalf("err = %v, want ErrSessionInUse", err)
	}
	// A caller that ignores the error still cannot run the spec, nor fall back to another mode.
	if _, err := second.Validate(); diag.CodeOf(err) != diag.CodeIsolation {
		t.Errorf("the refused spec validated: %v", err)
	}
	if second.Network == isolation.NetworkBridge || second.EgressNetwork != "" {
		t.Errorf("refused spec = %+v", second)
	}
}

func TestRealDocker_AContainerCreatedButNotStartedOnTheNetworkRefusesTheJob(t *testing.T) {
	img := needDocker(t)
	h := newHostService(t)
	m, _ := fixtureManager(t, img, h, "allowed.test")
	s := open(t, m)

	// `docker network inspect` does not list a container that has not started; the job's audit
	// must still see it (a second job, or an intruder, waiting to be started).
	stranger := uniq("br-egs-")
	dockerCLI(t, "create", "--name", stranger, "--network", s.Network, "--entrypoint", "/probe", img, "sleep")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", stranger).Run() })

	res, err := runJob(img, s, jobs, "routes")
	if diag.CodeOf(err) != diag.CodeEgressSetup || !strings.Contains(err.Error(), "container "+stranger+" is attached") {
		t.Fatalf("err = %v, want a BR-E082 refusal naming the unstarted container %s", err, stranger)
	}
	if res.Stdout != "" {
		t.Errorf("the job ran: %q", res.Stdout)
	}
}

func TestRealDocker_AContainerThatJoinsTheNetworkWhileTheJobRunsStopsTheJob(t *testing.T) {
	img := needDocker(t)
	h := newHostService(t)
	m, _ := fixtureManager(t, img, h, "allowed.test")
	s := open(t, m)

	d := &isolation.Docker{Exec: execx.OS{}, RecheckEvery: 100 * time.Millisecond}
	spec := isolation.Spec{Name: uniq("br-egw-"), Image: img, Args: []string{"sleep"}, Timeout: 90 * time.Second, MemoryMiB: 128, PidsLimit: 64, WorkTmpfsMiB: 16}
	applyAgain(s, &spec)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", spec.Name).Run() }) // unblocks Run if the watch fails

	type outcome struct {
		res isolation.Result
		err error
	}
	done := make(chan outcome, 1)
	start := time.Now()
	go func() {
		res, err := d.Run(ctx, spec)
		done <- outcome{res, err}
	}()

	// Wait until the job is really running, then create (not start) a container on its network.
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		out, _ := exec.Command("docker", "ps", "-q", "--filter", "name=^"+spec.Name+"$", "--filter", "status=running").Output()
		if strings.TrimSpace(string(out)) != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the job never started")
		}
	}
	stranger := uniq("br-egl-")
	dockerCLI(t, "create", "--name", stranger, "--network", s.Network, "--entrypoint", "/probe", img, "sleep")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", stranger).Run() })

	select {
	case o := <-done:
		if diag.CodeOf(o.err) != diag.CodeEgressSetup || !strings.Contains(o.err.Error(), "container "+stranger+" is attached") {
			t.Fatalf("err = %v, want a BR-E082 refusal naming %s", o.err, stranger)
		}
		if o.res.TimedOut {
			t.Error("the job was stopped by its timeout, not by the audit")
		}
		if time.Since(start) > 60*time.Second {
			t.Errorf("the job ran for %v before it was stopped", time.Since(start))
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the job kept running although an unexpected container joined its network")
	}
	if containerExists(spec.Name) {
		t.Error("the stopped job's container was not removed")
	}
}

func TestRealDocker_ANetworkNeighbourThatOnlyHasTheProxysNameAndAddressIsRefused(t *testing.T) {
	img := needDocker(t)
	// Everything a lookalike needs to pass the old audit: an internal network without a host
	// address, the egress manager's labels on the network, and a container with the proxy's
	// name at the address the job is told. What it lacks is being the proxy egress.Open built.
	hardened := []string{"--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--user", "65534:65534", "--pids-limit", "64", "--memory", "64m", "--cpus", "1"}
	type variant struct {
		name  string
		flags []string
		want  string
	}
	for _, v := range []variant{
		{"hardened but without the egress labels", hardened, "lacks the bladerunner.egress label"},
		{"labelled but not hardened", []string{"--label", "bladerunner.egress=session", "--label", "bladerunner.egress.id=lookalike"}, "is not hardened"},
	} {
		t.Run(v.name, func(t *testing.T) {
			net1, proxy := uniq("br-egf-net"), uniq("br-egf-proxy")
			dockerCLI(t, "network", "create", "--internal", "--opt", isolation.OptInhibitIPv4+"=true",
				"--label", "bladerunner.egress=session", "--label", "bladerunner.egress.id=lookalike", net1)
			t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", net1).Run() })
			args := append([]string{"run", "-d", "--name", proxy, "--network", net1, "--entrypoint", "/probe"}, v.flags...)
			dockerCLI(t, append(args, img, "sleep")...)
			t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", proxy).Run() })
			ip := dockerCLI(t, "inspect", "--format", "{{(index .NetworkSettings.Networks \""+net1+"\").IPAddress}}", proxy)

			spec := isolation.Spec{Name: uniq("br-egj-"), Image: img, Args: []string{"routes"}, Timeout: 30 * time.Second, MemoryMiB: 128, PidsLimit: 32,
				Network: isolation.NetworkAllowlist, EgressNetwork: net1, EgressProxy: ip + ":3128", EgressProxyContainer: proxy}
			res, err := jobs.Run(ctx, spec)
			if diag.CodeOf(err) != diag.CodeEgressSetup || !strings.Contains(err.Error(), v.want) {
				t.Fatalf("err = %v, want a BR-E082 refusal mentioning %q", err, v.want)
			}
			if res.Stdout != "" {
				t.Errorf("the job ran next to a container that is not the proxy: %q", res.Stdout)
			}
		})
	}
}

func TestRealDocker_ADenialFloodIsCountedNotItemisedAndTheAllowedRecordSurvives(t *testing.T) {
	img := needDocker(t)
	h := newHostService(t)
	m, _ := fixtureManager(t, img, h, "allowed.test")
	m.ProxyExtraArgs = append(m.ProxyExtraArgs, "-max-logged-denials", "3")
	s := open(t, m)
	port := strconv.Itoa(h.port)

	if res := job(t, img, s, "via-proxy", s.ProxyAddr, "allowed.test:"+port); !strings.HasPrefix(res.Stdout, "ALLOWED") {
		t.Fatalf("the allowed request: %q", res.Stdout)
	}
	for i := 0; i < 8; i++ {
		job(t, img, s, "via-proxy", s.ProxyAddr, fmt.Sprintf("flood%d.test:%s", i, port))
	}
	ds, err := s.Decisions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var allowed, itemised, summaries int
	for _, d := range ds {
		switch {
		case d.Allowed && d.Host == "allowed.test":
			allowed++
		case d.Reason == ReasonSuppressed:
			summaries++
		case !d.Allowed:
			itemised++
		}
	}
	if allowed != 1 || itemised != 3 || summaries == 0 {
		t.Errorf("allowed=%d itemised denials=%d summaries=%d, want 1, 3 (the cap) and at least one summary: %+v", allowed, itemised, summaries, ds)
	}
}
