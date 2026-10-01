package egress

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
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

	// LabelOwner records which Manager owner started a session; Sweep removes only its own.
	LabelOwner = "bladerunner.egress.owner"
	// labelOpen is unique per Open call. It is how a failed Open finds what ITS create calls
	// made even when the CLI never reported an id (cancelled, killed), without being able to
	// match another session that merely has the same session id.
	labelOpen = "bladerunner.egress.open"

	// DefaultOwner is the owner of a Manager that sets none. Two supervisors that share one
	// Docker daemon must each set their own, or one's Sweep removes the other's live sessions.
	DefaultOwner = "default"

	// OutNetwork is the one shared, ordinary bridge network the proxies reach the outside
	// through. Jobs are never attached to it.
	OutNetwork = isolation.EgressOutNetwork

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

	// Owner names whoever runs this Manager (the supervisor's runner name, say). Sessions carry
	// it as a label and Sweep removes only sessions with the same value, so one supervisor's
	// start-up sweep cannot tear down another's live sessions on a shared daemon. Default
	// DefaultOwner.
	Owner string

	// ProxyStopGrace is how long Close lets the proxy run after SIGTERM, so it can write its
	// pending log lines, before the container is removed. Default 5s.
	ProxyStopGrace time.Duration
}

func (m *Manager) owner() string {
	if m.Owner == "" {
		return DefaultOwner
	}
	return m.Owner
}

func (m *Manager) stopGrace() time.Duration {
	if m.ProxyStopGrace <= 0 {
		return 5 * time.Second
	}
	return m.ProxyStopGrace
}

// ownerFilter selects the sessions of this Manager's owner.
func (m *Manager) ownerFilter() []string {
	return []string{"--filter", "label=" + LabelKey + "=" + labelSession, "--filter", "label=" + LabelOwner + "=" + m.owner()}
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
	token            string // this Open call's labelOpen value

	// What Open built the proxy from: the audit requires the running proxy to match exactly.
	proxyImage                string
	proxyEntrypoint, proxyCmd []string

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

	var tok [8]byte
	if _, rerr := rand.Read(tok[:]); rerr != nil {
		return nil, setupErr(rerr, "cannot make a session token", "the system's random source failed", "report this")
	}
	s = &Session{ID: id, Network: "br-egress-" + id + "-net", ProxyContainer: "br-egress-" + id + "-proxy", m: m, token: hex.EncodeToString(tok[:])}
	defer func() {
		if err != nil {
			cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = s.Close(cctx)   // removes what the refs below record
			_ = s.reclaim(cctx) // and what a create call made without ever reporting an id
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
		"--label", LabelKey+"="+labelSession, "--label", labelID+"="+id, "--label", LabelOwner+"="+m.owner(), "--label", labelOpen+"="+s.token, s.Network)
	if err != nil {
		return s, setupErr(err, "cannot create the job's internal network", "Docker refused (is the id already in use, or are Docker's address pools exhausted?)", "check `docker network ls`; a crash can leave networks behind, the next start removes them")
	}
	s.netRef = createdRef(netRes.Stdout, s.Network)
	subnet, err := m.subnetOf(ctx, s.Network)
	if err != nil {
		return s, err
	}

	cmd := []string{"-listen-cidr", subnet.String(), "-port", strconv.Itoa(ProxyPort), "-ports", strings.Join(portArgs, ",")}
	for _, e := range m.Allow.Entries() {
		cmd = append(cmd, "-allow", e)
	}
	cmd = append(cmd, m.ProxyExtraArgs...)
	s.proxyImage, s.proxyEntrypoint, s.proxyCmd = m.ProxyImage, []string{m.entrypoint()}, cmd // what the audit holds the running proxy to
	args := append([]string{"create", "--name", s.ProxyContainer, "--network", s.Network,
		"--entrypoint", m.entrypoint(),
		"--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--user", "65534:65534",
		"--pids-limit", "128", "--memory", "128m", "--memory-swap", "128m", "--cpus", "1",
		"--restart", "no", "--log-driver", "local", "--log-opt", "max-size=1m", "--log-opt", "max-file=2",
		"--label", LabelKey + "=" + labelSession, "--label", labelID + "=" + id,
		"--label", LabelOwner + "=" + m.owner(), "--label", labelOpen + "=" + s.token,
		m.ProxyImage}, cmd...)
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
		spec.EgressProxyImage, spec.EgressProxyEntrypoint, spec.EgressProxyCmd = "", nil, nil
		return ErrSessionInUse
	}
	spec.Network = isolation.NetworkAllowlist
	spec.EgressNetwork = s.Network
	spec.EgressProxy = s.ProxyAddr
	spec.EgressProxyContainer = s.ProxyContainer
	spec.EgressProxyImage = s.proxyImage
	spec.EgressProxyEntrypoint = append([]string(nil), s.proxyEntrypoint...)
	spec.EgressProxyCmd = append([]string(nil), s.proxyCmd...)
	return nil
}

// Decisions reads back what the proxy decided, oldest first. It is the record of what a job
// asked for and what it was refused.
//
// The proxy batches some of what it writes (summaries of refusals, aggregates of repeated
// allowed tunnels), so before reading it is asked to flush (SIGUSR1) and the read waits for the
// proxy's acknowledgement marker. A proxy that is already gone is simply read as it is.
func (s *Session) Decisions(ctx context.Context) ([]Decision, error) {
	res, err := s.m.docker(ctx, "logs", s.ProxyContainer)
	if err != nil {
		return nil, setupErr(err, "cannot read the proxy's log", "the proxy is gone", "the session was closed")
	}
	all := parseDecisionLines(res.Stdout + res.Stderr)
	seen := flushMarker(all)
	if _, err := s.m.docker(ctx, "kill", "--signal", "USR1", s.ProxyContainer); err == nil {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if r, err := s.m.docker(ctx, "logs", s.ProxyContainer); err == nil {
				all = parseDecisionLines(r.Stdout + r.Stderr)
				if flushMarker(all) > seen {
					break
				}
			}
			select {
			case <-ctx.Done():
				deadline = time.Time{}
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	out := all[:0:0]
	for _, d := range all {
		if d.Reason != ReasonFlushed {
			out = append(out, d)
		}
	}
	return out, nil
}

// flushMarker is the highest flush count the log acknowledges.
func flushMarker(ds []Decision) int {
	n := 0
	for _, d := range ds {
		if d.Reason == ReasonFlushed && d.Count > n {
			n = d.Count
		}
	}
	return n
}

// maxDecisionLine is the longest log line Decisions parses. The proxy clips what it writes, so
// a longer line is not one of its records (or is a record cut by log rotation): it is skipped.
const maxDecisionLine = 64 << 10

// parseDecisionLines reads the proxy's log text. A line that is too long, not JSON, or not a
// decision is skipped; it never stops the lines after it from being read.
func parseDecisionLines(text string) []Decision {
	var out []Decision
	r := bufio.NewReaderSize(strings.NewReader(text), 4<<10)
	for {
		line, err := readLine(r, maxDecisionLine)
		if len(line) > 0 {
			var d Decision
			if json.Unmarshal(line, &d) == nil && d.Reason != "" {
				out = append(out, d)
			}
		}
		if err != nil {
			return out
		}
	}
}

// readLine returns the next line without its newline, or nil for a line longer than max (which
// it consumes to the newline so the next call starts on the following line).
func readLine(r *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	tooLong := false
	for {
		chunk, err := r.ReadSlice('\n')
		if !tooLong {
			buf = append(buf, chunk...)
			if len(buf) > max {
				buf, tooLong = nil, true
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if tooLong {
			return nil, err
		}
		return []byte(strings.TrimRight(string(buf), "\r\n")), err
	}
}

// Close removes the proxy and the network this session created. It is idempotent and tolerates a
// half-built session: what Open never created (a name that was already taken, a step it did not
// reach) is left alone. The proxy is stopped first, with a grace period, so that it writes the
// log lines it still holds before the container (and its log) is removed.
func (s *Session) Close(ctx context.Context) error {
	var firstErr error
	if s.proxyRef != "" {
		grace := s.m.stopGrace()
		_, _ = s.m.docker(ctx, "stop", "--time", strconv.Itoa(int((grace+time.Second-1)/time.Second)), s.proxyRef) // best effort: rm -f below is the guarantee
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

// reclaim removes what this Open call's create commands made but never reported an id for (the
// context was cancelled, or the CLI was killed, after the daemon had done the work). They are
// found by this call's own token label together with the session label and id, so another
// session, even one with the same id, is never matched.
func (s *Session) reclaim(ctx context.Context) error {
	var errs []error
	sel := []string{"--filter", "label=" + LabelKey + "=" + labelSession, "--filter", "label=" + labelID + "=" + s.ID, "--filter", "label=" + labelOpen + "=" + s.token}
	if cs, err := s.m.docker(ctx, append([]string{"ps", "-aq"}, sel...)...); err != nil {
		errs = append(errs, err)
	} else {
		for _, id := range strings.Fields(cs.Stdout) {
			if _, err := s.m.docker(ctx, "rm", "-f", "-v", id); err != nil && !notFound(err) {
				errs = append(errs, err)
			}
		}
	}
	if ns, err := s.m.docker(ctx, append([]string{"network", "ls", "-q"}, sel...)...); err != nil {
		errs = append(errs, err)
	} else {
		for _, id := range strings.Fields(ns.Stdout) {
			if _, err := s.m.docker(ctx, "network", "rm", id); err != nil && !notFound(err) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func notFound(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "no such") || strings.Contains(m, "not found")
}

// Sweep removes every proxy and network this Manager's owner left behind (a crash's leftovers),
// and returns how many it removed. Sessions of another owner are not touched, so a supervisor's
// start-up sweep cannot remove a neighbour's live sessions. It carries on past a failure and
// reports every one, joined: a proxy that will not go must not keep the others in place. The
// shared outbound network stays: it is cheap, and another job may be using it.
func (m *Manager) Sweep(ctx context.Context) (int, error) {
	n := 0
	var errs []error
	cs, err := m.docker(ctx, append([]string{"ps", "-aq"}, m.ownerFilter()...)...)
	if err != nil {
		errs = append(errs, fmt.Errorf("cannot list stale egress proxies: %w", err))
	} else {
		for _, id := range strings.Fields(cs.Stdout) {
			if _, err := m.docker(ctx, "rm", "-f", "-v", id); err != nil && !notFound(err) {
				errs = append(errs, fmt.Errorf("cannot remove stale egress proxy %s: %w", id, err))
			} else {
				n++
			}
		}
	}
	ns, err := m.docker(ctx, append([]string{"network", "ls", "-q"}, m.ownerFilter()...)...)
	if err != nil {
		errs = append(errs, fmt.Errorf("cannot list stale egress networks: %w", err))
	} else {
		for _, id := range strings.Fields(ns.Stdout) {
			if _, err := m.docker(ctx, "network", "rm", id); err != nil && !notFound(err) {
				errs = append(errs, fmt.Errorf("cannot remove stale egress network %s: %w", id, err))
			} else {
				n++
			}
		}
	}
	if len(errs) > 0 {
		return n, setupErr(errors.Join(errs...), "cannot remove every stale egress object", "Docker is not answering, refused, or something is still attached", "run `docker rm -f` and `docker network rm` on what is listed above")
	}
	return n, nil
}
