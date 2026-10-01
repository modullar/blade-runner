package isolation

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modullar/blade-runner/internal/diag"
	"github.com/modullar/blade-runner/internal/execx"
)

// ---- the audit of an allowlist job while it runs, driven by a scriptable runtime ----------
//
// execx.Fake stands in for the docker CLI here because the interesting moments (a read that
// fails once, a kill that fails twice, a container that appears after the job exits) cannot be
// produced on demand with a real daemon. The topology it answers with is the JSON the real one
// printed (see isolation_egress_test.go); the assertions are on what Run returns and on whether
// the job was killed, not on which commands were issued.

type watchRig struct {
	f *execx.Fake

	startFor      time.Duration // how long the job runs when nobody kills it
	psFails       atomic.Int32  // the next N `ps -a --filter network=` calls fail
	psFailsAfter  atomic.Int32  // set when the job exits: that many failures for the final audit
	intruder      atomic.Bool   // an extra container is attached to the network
	intruderOnRun atomic.Bool   // ...from the moment the job starts
	intruderAtEnd atomic.Bool   // ...from the moment the job exits
	killFails     atomic.Int32  // the next N kills fail
	kills         atomic.Int32
	started       atomic.Int32
	sharing       atomic.Bool // a stranger shares the proxy's network stack

	mu      sync.Mutex
	running bool
	stop    chan struct{}
}

func newWatchRig() *watchRig {
	r := &watchRig{startFor: 300 * time.Millisecond, stop: make(chan struct{})}
	r.f = &execx.Fake{Handler: r.handle}
	return r
}

func (r *watchRig) isRunning() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.running }

func (r *watchRig) handle(c execx.Cmd) (execx.Result, error) {
	line := strings.Join(c.Args, " ")
	ok := func(s string) (execx.Result, error) { return execx.Result{Stdout: s}, nil }
	switch {
	case c.Args[0] == "start":
		r.started.Add(1)
		r.mu.Lock()
		r.running = true
		r.mu.Unlock()
		if r.intruderOnRun.Load() {
			r.intruder.Store(true)
		}
		select {
		case <-time.After(r.startFor):
			r.mu.Lock()
			r.running = false
			r.mu.Unlock()
			r.psFails.Store(r.psFailsAfter.Load())
			if r.intruderAtEnd.Load() {
				r.intruder.Store(true)
			}
			return execx.Result{}, nil
		case <-r.stop:
			return execx.Fail("docker", 137, "")
		}
	case c.Args[0] == "kill":
		r.kills.Add(1)
		if r.killFails.Add(-1) >= 0 {
			return execx.Fail("docker", 1, "Error response from daemon: cannot kill container")
		}
		r.mu.Lock()
		if r.running {
			r.running = false
			close(r.stop)
		}
		r.mu.Unlock()
		return ok("")
	case strings.HasPrefix(line, "inspect --format {{.State.Running}}"):
		if r.isRunning() {
			return ok("true\n")
		}
		return ok("false\n")
	case strings.HasPrefix(line, "inspect --format {{.State.OOMKilled}}"):
		return ok("false 0\n")
	case line == "inspect job-1":
		return ok(strings.Replace(allowCompliant, `[{`, `[{"Id": "jobid0123", `, 1))
	case line == "inspect br-egress-s1-proxy":
		return ok(proxyCompliant)
	case c.Args[0] == "network" && c.Args[1] == "inspect":
		return ok(networkCompliant)
	case strings.HasPrefix(line, "ps -a --filter network="):
		if r.psFails.Add(-1) >= 0 {
			return execx.Fail("docker", 1, "Cannot connect to the Docker daemon")
		}
		out := "br-egress-s1-proxy\njob-1\n"
		if r.intruder.Load() {
			out += "intruder\n"
		}
		return ok(out)
	case strings.HasPrefix(line, "ps -a --no-trunc"):
		if r.sharing.Load() {
			return ok("abc|br-egress-s1-proxy|br-egress-s1-net br-egress-out|bladerunner.egress=session\nccc|sneaky||\n")
		}
		return ok("abc|br-egress-s1-proxy|br-egress-s1-net br-egress-out|bladerunner.egress=session\n")
	case strings.HasPrefix(line, "inspect --format {{.Id}}|"):
		if r.sharing.Load() {
			return ok("abc|/br-egress-s1-proxy|br-egress-s1-net|{}\nccc|/sneaky|container:abc|{}\n")
		}
		return ok("abc|/br-egress-s1-proxy|br-egress-s1-net|{}\n")
	}
	return execx.Result{}, nil // create, rm, anything else: success
}

func (r *watchRig) run() (Result, error) {
	d := &Docker{Exec: r.f, RecheckEvery: 10 * time.Millisecond, KillRetryEvery: 2 * time.Millisecond}
	s := allowSpec()
	s.Timeout = 30 * time.Second
	return d.Run(context.Background(), s)
}

func TestAWatchedJobSurvivesASingleFailedRead(t *testing.T) {
	r := newWatchRig()
	r.psFails.Store(1) // the daemon hiccups once while the job runs... (after the pre-start audit)
	r.psFails.Store(0)
	r.startFor = 400 * time.Millisecond
	// The hiccup is armed when the job starts.
	armed := r.f.Handler
	r.f.Handler = func(c execx.Cmd) (execx.Result, error) {
		if c.Args[0] == "start" {
			r.psFails.Store(1)
		}
		return armed(c)
	}
	res, err := r.run()
	if err != nil {
		t.Fatalf("one unreadable audit killed a clean job: %v", err)
	}
	if r.kills.Load() != 0 || res.ExitCode != 0 {
		t.Errorf("kills = %d, exit = %d: nothing should have touched the job", r.kills.Load(), res.ExitCode)
	}
}

func TestTwoFailedReadsInARowKillTheJobAndReportThatItRan(t *testing.T) {
	r := newWatchRig()
	r.startFor = 5 * time.Second
	armed := r.f.Handler
	r.f.Handler = func(c execx.Cmd) (execx.Result, error) {
		if c.Args[0] == "start" {
			r.psFails.Store(1 << 20)
		}
		return armed(c)
	}
	start := time.Now()
	_, err := r.run()
	if err == nil || diag.CodeOf(err) != diag.CodeEgressSetup {
		t.Fatalf("err = %v, want a BR-E082 refusal", err)
	}
	if strings.Contains(err.Error(), "NOT started") || !strings.Contains(err.Error(), "RAN") {
		t.Errorf("a job that ran must not be reported as not started: %v", err)
	}
	if r.kills.Load() == 0 || time.Since(start) > 3*time.Second {
		t.Errorf("kills = %d after %v: the job was left running", r.kills.Load(), time.Since(start))
	}
}

func TestAViolationThatWasReadKillsTheJobAtOnce(t *testing.T) {
	r := newWatchRig()
	r.startFor = 5 * time.Second
	r.intruderOnRun.Store(true)
	start := time.Now()
	_, err := r.run()
	if time.Since(start) > 2*time.Second {
		t.Errorf("the job ran for %v after an intruder joined its network", time.Since(start))
	}
	if diag.CodeOf(err) != diag.CodeEgressSetup || !strings.Contains(err.Error(), "intruder") {
		t.Errorf("err = %v, want BR-E082 naming the intruder", err)
	}
	if strings.Contains(err.Error(), "NOT started") {
		t.Errorf("the job ran: %v", err)
	}
}

func TestTheKillIsRetriedUntilTheContainerIsGone(t *testing.T) {
	r := newWatchRig()
	r.startFor = 20 * time.Second
	r.intruderOnRun.Store(true)
	r.killFails.Store(3)
	done := make(chan struct{})
	go func() { r.run(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the job was never stopped although the 4th kill would have worked")
	}
	if got := r.kills.Load(); got < 4 {
		t.Errorf("kills = %d, want it retried past the 3 failures", got)
	}
}

func TestACleanJobWhoseFinalAuditFindsAViolationIsReportedAsHavingRun(t *testing.T) {
	r := newWatchRig()
	r.startFor = 30 * time.Millisecond
	d := &Docker{Exec: r.f, RecheckEvery: time.Hour, KillRetryEvery: time.Millisecond} // no mid-run audit: only the final one
	r.intruderAtEnd.Store(true)
	s := allowSpec()
	s.Timeout = 30 * time.Second
	res, err := d.Run(context.Background(), s)
	if err == nil || strings.Contains(err.Error(), "NOT started") || !strings.Contains(err.Error(), "RAN") {
		t.Fatalf("err = %v, want a refusal that says the job RAN", err)
	}
	if diag.CodeOf(err) != diag.CodeEgressSetup || res.ExitCode != 0 {
		t.Errorf("code = %s, exit = %d", diag.CodeOf(err), res.ExitCode)
	}
}

func TestTheFinalAuditRetriesOneUnreadableAnswerButNotTwo(t *testing.T) {
	for _, tc := range []struct {
		fails   int32
		wantErr bool
	}{{1, false}, {2, true}} {
		r := newWatchRig()
		r.startFor = 30 * time.Millisecond
		r.psFailsAfter.Store(tc.fails)
		d := &Docker{Exec: r.f, RecheckEvery: time.Hour, KillRetryEvery: time.Millisecond}
		s := allowSpec()
		s.Timeout = 30 * time.Second
		_, err := d.Run(context.Background(), s)
		if (err != nil) != tc.wantErr {
			t.Errorf("%d unreadable final audits: err = %v, want error = %v", tc.fails, err, tc.wantErr)
		}
		if err != nil && !strings.Contains(err.Error(), "RAN") {
			t.Errorf("%v", err)
		}
	}
}

func TestAnUnreadableTopologyBeforeStartIsBREgressNotBRIsolation(t *testing.T) {
	r := newWatchRig()
	r.psFails.Store(1 << 20)
	_, err := r.run()
	if diag.CodeOf(err) != diag.CodeEgressSetup {
		t.Fatalf("code = %q (%v), want BR-E082 as the ADR says", diag.CodeOf(err), err)
	}
	if r.started.Load() != 0 {
		t.Error("the job was started although its network could not be audited")
	}
}

func TestAContainerSharingTheProxysStackRefusesTheJobBeforeItStarts(t *testing.T) {
	r := newWatchRig()
	r.sharing.Store(true)
	_, err := r.run()
	if diag.CodeOf(err) != diag.CodeEgressSetup || !strings.Contains(err.Error(), "sneaky shares the network stack of the proxy") {
		t.Fatalf("err = %v", err)
	}
	if r.started.Load() != 0 {
		t.Error("the job was started next to a container inside the proxy's network namespace")
	}
}

func TestAContainerThatJoinsTheProxysStackWhileTheJobRunsStopsIt(t *testing.T) {
	r := newWatchRig()
	r.startFor = 5 * time.Second
	armed := r.f.Handler
	r.f.Handler = func(c execx.Cmd) (execx.Result, error) {
		if c.Args[0] == "start" {
			r.sharing.Store(true)
		}
		return armed(c)
	}
	start := time.Now()
	_, err := r.run()
	if err == nil || !strings.Contains(err.Error(), "shares the network stack") || time.Since(start) > 2*time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(start))
	}
}

// ---- namespace sharing, pure ---------------------------------------------------------------

func TestAuditNamespaceSharing(t *testing.T) {
	spec := allowSpec()
	tests := []struct {
		name string
		line string
		want string // "" = clean
	}{
		{"shares the proxy by full id", "c1|/x|container:abcdef0123|{}", "the proxy"},
		{"shares the proxy by id prefix", "c1|/x|container:abcd|{}", "the proxy"},
		{"shares the proxy by name", "c1|/x|container:br-egress-s1-proxy|{}", "the proxy"},
		{"shares the job by id", "c1|/x|container:jobid0123|{}", "the job"},
		{"shares the job by name", "c1|/x|container:job-1|{}", "the job"},
		{"a Blade Runner container joining any other stack", `c1|/x|container:other|{"bladerunner.job":"j"}`, "joins another container"},
		{"an unrelated container sharing an unrelated stack", "c1|/x|container:other|{}", ""},
		{"an ordinary container on a bridge", "c1|/x|bridge|{}", ""},
		{"the job itself", "jobid0123|/job-1|br-egress-s1-net|{}", ""},
		{"the proxy itself", "abcdef0123|/br-egress-s1-proxy|br-egress-s1-net|{}", ""},
	}
	for _, tc := range tests {
		v := AuditNamespaceSharing(tc.line+"\n", spec, "jobid0123", "abcdef0123")
		got := strings.Join(v, "\n")
		if tc.want == "" && len(v) != 0 || tc.want != "" && !strings.Contains(got, tc.want) {
			t.Errorf("%s: violations %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := NamespaceCandidates("a|n1|bridge|x=y\nb|n2||\nc|n3|none|bladerunner.job=j\nd|n4|host|\n"); strings.Join(got, ",") != "b,c" {
		t.Errorf("candidates = %v, want the container with no network and the labelled one", got)
	}
}

func TestAuditProxyHoldsTheProxyToExactlyTwoNetworksAndToWhatWasBuilt(t *testing.T) {
	setNets := func(names ...string) func(m map[string]any) {
		return func(m map[string]any) {
			n := map[string]any{}
			for _, x := range names {
				n[x] = map[string]any{}
			}
			m["NetworkSettings"] = map[string]any{"Networks": n}
		}
	}
	cfg := func(m map[string]any) map[string]any { return m["Config"].(map[string]any) }
	built := allowSpec()
	built.EgressProxyImage = "sha256:" + strings.Repeat("b", 64)
	built.EgressProxyEntrypoint = []string{"/egress-proxy"}
	built.EgressProxyCmd = []string{"-listen-cidr", "172.19.0.0/16", "-allow", "github.com"}
	asBuilt := func(m map[string]any) {
		cfg(m)["Image"] = built.EgressProxyImage
		cfg(m)["Entrypoint"] = built.EgressProxyEntrypoint
		cfg(m)["Cmd"] = built.EgressProxyCmd
	}
	tests := []struct {
		name string
		spec Spec
		edit func(m map[string]any)
		want string
	}{
		{"a third network", allowSpec(), setNets("br-egress-s1-net", "br-egress-out", "bridge"), "not exactly"},
		{"no outbound network", allowSpec(), setNets("br-egress-s1-net"), "not exactly"},
		{"the outbound network but not the session's", allowSpec(), setNets("br-egress-out", "other"), "not exactly"},
		{"a different image", built, func(m map[string]any) { asBuilt(m); cfg(m)["Image"] = "sha256:" + strings.Repeat("c", 64) }, "runs image"},
		{"a different entrypoint", built, func(m map[string]any) { asBuilt(m); cfg(m)["Entrypoint"] = []string{"/bin/sh"} }, "has entrypoint"},
		{"an added flag", built, func(m map[string]any) {
			asBuilt(m)
			cfg(m)["Cmd"] = append(append([]string{}, built.EgressProxyCmd...), "-allow", "evil.test")
		}, "has command"},
		{"a string entrypoint", built, func(m map[string]any) { asBuilt(m); cfg(m)["Entrypoint"] = "/egress-proxy" }, ""},
	}
	for _, tc := range tests {
		v, err := AuditProxy(mutateJSON(t, proxyCompliant, tc.edit), []byte(networkCompliant), tc.spec)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		got := strings.Join(v, "\n")
		if tc.want == "" && len(v) != 0 || tc.want != "" && !strings.Contains(got, tc.want) {
			t.Errorf("%s: violations %q, want %q", tc.name, got, tc.want)
		}
	}
	// What Open built passes.
	if v, err := AuditProxy(mutateJSON(t, proxyCompliant, asBuilt), []byte(networkCompliant), built); err != nil || len(v) != 0 {
		t.Errorf("the proxy exactly as built: %v %v", v, err)
	}
	// The proxy fields are for allowlist jobs only.
	s := Spec{Name: "j", Image: "sha256:" + strings.Repeat("a", 64), Network: NetworkNone, EgressProxyImage: "x"}
	if _, err := s.Validate(); err == nil {
		t.Error("proxy identity fields outside allowlist mode must be refused")
	}
}
