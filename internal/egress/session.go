package egress

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/execx"
	"github.com/modullar/blade-runner/internal/isolation"
)

// Labels marking what this package creates, so a crash's leftovers can be found and removed.
const (
	LabelKey     = isolation.LabelEgress // the audit in internal/isolation reads these two
	labelSession = isolation.EgressRoleSession
	labelOut     = "out"
	labelID      = isolation.LabelEgressID

	// OutNetwork is the one shared, ordinary bridge network the proxies reach the outside
	// through. Jobs are never attached to it.
	OutNetwork = "br-egress-out"

	// ProxyPort is the port every proxy listens on, on its internal address only.
	ProxyPort = 3128

	// DefaultProxyEntrypoint is where the proxy binary lives in the proxy image.
	DefaultProxyEntrypoint = "/egress-proxy"
)

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)

// Manager builds the egress topology for one job at a time and tears it down again.
type Manager struct {
	Exec execx.Runner
	Bin  string // default "docker"

	// ProxyImage is the image holding the proxy binary, pinned by content like a job image.
	ProxyImage      string
	ProxyEntrypoint string // default DefaultProxyEntrypoint
	// ProxyExtraArgs are appended to the proxy's command line. Production leaves it empty; it
	// exists so a test image can carry fixture-only flags that the production binary lacks.
	ProxyExtraArgs []string

	Allow Allowlist
	Ports []int // default DefaultPorts

	// StartTimeout bounds how long Open waits for the proxy to accept connections; default 20s.
	StartTimeout time.Duration
}

// Session is one job's egress topology: its internal network and its proxy.
type Session struct {
	ID             string
	Network        string // the internal network the job joins
	ProxyContainer string
	ProxyAddr      string // ip:port on that network

	m *Manager

	// What THIS Open call created, by Docker's id. A failed Open removes only these: a name
	// collision with another session's proxy or network must never remove that session's.
	netRef, proxyRef string

	claimed atomic.Bool // Apply has handed this session to a job
}

func (m *Manager) bin() string {
	if m.Bin == "" {
		return "docker"
	}
	return m.Bin
}

func (m *Manager) docker(ctx context.Context, args ...string) (execx.Result, error) {
	return m.Exec.Run(ctx, execx.Cmd{Name: m.bin(), Args: args})
}

func setupErr(err error, what, cause, fix string) error {
	return diag.Wrap(err, diag.CodeEgressSetup, what, cause, fix)
}

// Open creates the internal network and the proxy for the session id, and returns once the
// proxy accepts connections. On any failure everything IT made is removed again, and nothing
// else: if the names are already taken (another live session with this id), Open fails without
// touching what is there.
func (m *Manager) Open(ctx context.Context, id string) (s *Session, err error) {
	if !idRe.MatchString(id) {
		return nil, setupErr(nil, fmt.Sprintf("%q is not a valid egress session id", id), "a bug in the caller", "report this")
	}
	if len(m.Allow.Entries()) == 0 {
		return nil, policyInvalid("it is empty (use network mode none for no network)")
	}
	ports := m.Ports
	if len(ports) == 0 {
		ports = DefaultPorts
	}
	var portArgs []string
	for _, p := range ports {
		if p < 1 || p > 65535 {
			return nil, policyInvalid(fmt.Sprintf("port %d is not between 1 and 65535", p))
		}
		portArgs = append(portArgs, strconv.Itoa(p))
	}
	if _, err := (isolation.Spec{Name: "egress-proxy", Image: m.ProxyImage}).Validate(); err != nil {
		return nil, setupErr(err, "the egress proxy image is not usable", "it must be pinned by content (name@sha256:... or sha256:...)", "build the proxy image and pass its digest")
	}

	s = &Session{ID: id, Network: "br-egress-" + id + "-net", ProxyContainer: "br-egress-" + id + "-proxy", m: m}
	defer func() {
		if err != nil {
			cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = s.Close(cctx) // removes only what the refs below record
			s = nil
		}
	}()

	if err := m.ensureOut(ctx); err != nil {
		return s, err
	}
	// The network: no route out, and the host gets no address on it. The second property is what
	// plain --internal lacks: there the host's bridge address stays reachable from the job.
	netRes, err := m.docker(ctx, "network", "create", "--internal", "--driver", "bridge",
		"--opt", isolation.OptInhibitIPv4+"=true",
		"--label", LabelKey+"="+labelSession, "--label", labelID+"="+id, s.Network)
	if err != nil {
		return s, setupErr(err, "cannot create the job's internal network", "Docker refused (is the id already in use, or are Docker's address pools exhausted?)", "check `docker network ls`; a crash can leave networks behind, the next start removes them")
	}
	s.netRef = createdRef(netRes.Stdout, s.Network)
	subnet, err := m.subnetOf(ctx, s.Network)
	if err != nil {
		return s, err
	}

	args := []string{"create", "--name", s.ProxyContainer, "--network", s.Network,
		"--entrypoint", m.entrypoint(),
		"--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--user", "65534:65534",
		"--pids-limit", "128", "--memory", "128m", "--memory-swap", "128m", "--cpus", "1",
		"--restart", "no", "--log-driver", "local", "--log-opt", "max-size=1m", "--log-opt", "max-file=2",
		"--label", LabelKey + "=" + labelSession, "--label", labelID + "=" + id,
		m.ProxyImage,
		"-listen-cidr", subnet.String(), "-port", strconv.Itoa(ProxyPort), "-ports", strings.Join(portArgs, ","),
	}
	for _, e := range m.Allow.Entries() {
		args = append(args, "-allow", e)
	}
	args = append(args, m.ProxyExtraArgs...)
	proxyRes, err := m.docker(ctx, args...)
	if err != nil {
		return s, setupErr(err, "cannot create the egress proxy container", "the proxy image is missing, the name is taken, or Docker refused", "check `docker images` and `docker ps -a`")
	}
	s.proxyRef = createdRef(proxyRes.Stdout, s.ProxyContainer)
	if _, err := m.docker(ctx, "network", "connect", OutNetwork, s.ProxyContainer); err != nil {
		return s, setupErr(err, "cannot attach the proxy to the outbound network", "Docker refused", "check `docker network ls`")
	}
	if err := m.auditProxy(ctx, s); err != nil {
		return s, err
	}
	if _, err := m.docker(ctx, "start", s.ProxyContainer); err != nil {
		return s, setupErr(err, "cannot start the egress proxy", "Docker refused", "check `docker logs "+s.ProxyContainer+"`")
	}
	ip, err := m.proxyIP(ctx, s)
	if err != nil {
		return s, err
	}
	s.ProxyAddr = netip.AddrPortFrom(ip, ProxyPort).String()
	if err := m.waitReady(ctx, s); err != nil {
		return s, err
	}
	return s, nil
}

// createdRef is the id `docker create` / `docker network create` printed for what it just made,
// or the name when it printed nothing usable. The id is what later removal uses, so it can only
// ever remove that object, never another one that has the same name.
func createdRef(stdout, name string) string {
	if f := strings.Fields(stdout); len(f) > 0 && regexp.MustCompile(`^[0-9a-f]{12,64}$`).MatchString(f[0]) {
		return f[0]
	}
	return name
}

func (m *Manager) entrypoint() string {
	if m.ProxyEntrypoint == "" {
		return DefaultProxyEntrypoint
	}
	return m.ProxyEntrypoint
}

// ensureOut makes the shared outbound network if it does not exist. Two jobs starting at once
// may both try; the loser's "already exists" is fine as long as the network is there.
func (m *Manager) ensureOut(ctx context.Context) error {
	if _, err := m.docker(ctx, "network", "inspect", OutNetwork); err == nil {
		return nil
	}
	_, cerr := m.docker(ctx, "network", "create", "--driver", "bridge", "--label", LabelKey+"="+labelOut, OutNetwork)
	if _, err := m.docker(ctx, "network", "inspect", OutNetwork); err != nil {
		return setupErr(cerr, "cannot create the outbound network the proxies use", "Docker refused or is not answering", "check `docker network ls`")
	}
	return nil
}

func (m *Manager) subnetOf(ctx context.Context, name string) (netip.Prefix, error) {
	res, err := m.docker(ctx, "network", "inspect", "--format", "{{json .IPAM.Config}}", name)
	if err != nil {
		return netip.Prefix{}, setupErr(err, "cannot read the new network back", "Docker stopped answering", "check Docker")
	}
	var cfg []struct{ Subnet string }
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &cfg); err == nil {
		for _, c := range cfg {
			if p, err := netip.ParsePrefix(c.Subnet); err == nil && p.Addr().Is4() {
				return p, nil
			}
		}
	}
	return netip.Prefix{}, setupErr(nil, "the new network has no IPv4 subnet", "an unexpected Docker version", "update Docker")
}

// auditProxy holds the proxy to the same hardening as a job (it runs our code, but it is the
// one piece with a foot in the outside world) and requires it to be on exactly the two
// networks it needs.
func (m *Manager) auditProxy(ctx context.Context, s *Session) error {
	ins, err := m.docker(ctx, "inspect", s.ProxyContainer)
	if err != nil {
		return setupErr(err, "cannot read back the proxy's configuration", "Docker stopped answering", "check Docker")
	}
	v, err := isolation.Audit([]byte(ins.Stdout), isolation.Spec{Network: isolation.NetworkBridge})
	if err != nil {
		return setupErr(err, "cannot audit the proxy container", "an unexpected Docker version", "update Docker")
	}
	var parsed []struct {
		NetworkSettings struct {
			Networks map[string]json.RawMessage
		}
	}
	if err := json.Unmarshal([]byte(ins.Stdout), &parsed); err != nil || len(parsed) != 1 {
		return setupErr(err, "cannot read the proxy's networks", "an unexpected Docker version", "update Docker")
	}
	nets := parsed[0].NetworkSettings.Networks
	if len(nets) != 2 || nets[s.Network] == nil || nets[OutNetwork] == nil {
		v = append(v, fmt.Sprintf("the proxy must be on exactly %s and %s", s.Network, OutNetwork))
	}
	if len(v) > 0 {
		return setupErr(nil, "the egress proxy was NOT started: it is not confined as required",
			"Docker applied a different configuration than was asked for:\n    - "+strings.Join(v, "\n    - "),
			"update Docker, or report this")
	}
	return nil
}

func (m *Manager) proxyIP(ctx context.Context, s *Session) (netip.Addr, error) {
	res, err := m.docker(ctx, "inspect", "--format", "{{json .NetworkSettings.Networks}}", s.ProxyContainer)
	if err != nil {
		return netip.Addr{}, setupErr(err, "cannot read the proxy's address", "Docker stopped answering", "check Docker")
	}
	var nets map[string]struct{ IPAddress string }
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &nets); err == nil {
		if ip, err := netip.ParseAddr(nets[s.Network].IPAddress); err == nil && ip.Is4() {
			return ip, nil
		}
	}
	return netip.Addr{}, setupErr(nil, "the egress proxy has no address on its network", "the proxy exited at start (see `docker logs "+s.ProxyContainer+"`)", "check the proxy image")
}

func (m *Manager) waitReady(ctx context.Context, s *Session) error {
	limit := m.StartTimeout
	if limit <= 0 {
		limit = 20 * time.Second
	}
	deadline := time.Now().Add(limit)
	var last error
	for time.Now().Before(deadline) {
		if _, err := m.docker(ctx, "exec", s.ProxyContainer, m.entrypoint(), "check", s.ProxyAddr); err == nil {
			return nil
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return setupErr(ctx.Err(), "waiting for the egress proxy was cancelled", "the caller gave up", "retry")
		case <-time.After(100 * time.Millisecond):
		}
	}
	logs, _ := m.docker(ctx, "logs", "--tail", "5", s.ProxyContainer)
	return setupErr(last, "the egress proxy did not start accepting connections",
		"it crashed or is misconfigured; its output: "+strings.TrimSpace(logs.Stdout+logs.Stderr),
		"check `docker logs "+s.ProxyContainer+"`")
}

// ErrSessionInUse is returned by Apply when the session was already handed to a job.
var ErrSessionInUse = errors.New("this egress session already serves a job: open another session for each job")

// Apply fills in the spec fields that put a job on this session's network. A session serves ONE
// job: jobs sharing a network could reach each other and would share one proxy's log, so a
// second Apply fails (and leaves the spec unable to pass validation, so a caller that ignores
// the error still cannot run it). isolation.Docker.Run enforces the same from Docker's side:
// it refuses a job whose network has any container besides the proxy and that job.
func (s *Session) Apply(spec *isolation.Spec) error {
	if !s.claimed.CompareAndSwap(false, true) {
		spec.Network = isolation.NetworkAllowlist
		spec.EgressNetwork, spec.EgressProxy, spec.EgressProxyContainer = "", "", ""
		return ErrSessionInUse
	}
	spec.Network = isolation.NetworkAllowlist
	spec.EgressNetwork = s.Network
	spec.EgressProxy = s.ProxyAddr
	spec.EgressProxyContainer = s.ProxyContainer
	return nil
}

// Decisions reads back what the proxy decided, oldest first. It is the record of what a job
// asked for and what it was refused.
func (s *Session) Decisions(ctx context.Context) ([]Decision, error) {
	res, err := s.m.docker(ctx, "logs", s.ProxyContainer)
	if err != nil {
		return nil, setupErr(err, "cannot read the proxy's log", "the proxy is gone", "the session was closed")
	}
	var out []Decision
	sc := bufio.NewScanner(strings.NewReader(res.Stdout + res.Stderr))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var d Decision
		if json.Unmarshal(sc.Bytes(), &d) == nil && d.Reason != "" {
			out = append(out, d)
		}
	}
	return out, sc.Err()
}

// Close removes the proxy and the network this session created. It is idempotent and tolerates a
// half-built session: what Open never created (a name that was already taken, a step it did not
// reach) is left alone.
func (s *Session) Close(ctx context.Context) error {
	var firstErr error
	if s.proxyRef != "" {
		if _, err := s.m.docker(ctx, "rm", "-f", "-v", s.proxyRef); err != nil && !notFound(err) {
			firstErr = err
		}
	}
	if s.netRef != "" {
		if _, err := s.m.docker(ctx, "network", "rm", s.netRef); err != nil && !notFound(err) && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return setupErr(firstErr, "cannot remove the job's egress network", "something is still attached to it", "run `docker rm -f "+s.ProxyContainer+"` and `docker network rm "+s.Network+"`")
	}
	return nil
}

func notFound(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "no such") || strings.Contains(m, "not found")
}

// Sweep removes every proxy and network a crashed run left behind. The shared outbound network
// stays: it is cheap, and another job may be using it.
func (m *Manager) Sweep(ctx context.Context) (int, error) {
	filter := "label=" + LabelKey + "=" + labelSession
	n := 0
	cs, err := m.docker(ctx, "ps", "-aq", "--filter", filter)
	if err != nil {
		return 0, setupErr(err, "cannot list stale egress proxies", "Docker is not answering", "check Docker")
	}
	if ids := strings.Fields(cs.Stdout); len(ids) > 0 {
		if _, err := m.docker(ctx, append([]string{"rm", "-f", "-v"}, ids...)...); err != nil {
			return 0, setupErr(err, "cannot remove stale egress proxies", "Docker refused", "run `docker rm -f` on them by hand")
		} else {
			n += len(ids)
		}
	}
	ns, err := m.docker(ctx, "network", "ls", "-q", "--filter", filter)
	if err != nil {
		return 0, setupErr(err, "cannot list stale egress networks", "Docker is not answering", "check Docker")
	}
	for _, id := range strings.Fields(ns.Stdout) {
		if _, err := m.docker(ctx, "network", "rm", id); err != nil {
			return n, setupErr(err, "cannot remove a stale egress network", "a container is still attached", "run `docker network rm "+id+"` after removing it")
		}
		n++
	}
	return n, nil
}
