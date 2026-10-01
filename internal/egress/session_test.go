package egress

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modullar/blade-runner/internal/execx"
)

// ---- no daemon: what Close, Decisions and Sweep ask Docker to do -------------------------

func TestCloseStopsTheProxyWithAGraceBeforeRemovingIt(t *testing.T) {
	f := &execx.Fake{}
	s := &Session{m: &Manager{Exec: f, ProxyStopGrace: 4 * time.Second}, proxyRef: "p1", netRef: "n1"}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"[docker stop --time 4 p1]", "[docker rm -f -v p1]", "[docker network rm n1]"}
	if got := f.Lines(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("calls = %v, want %v (the proxy must get SIGTERM and time to flush before rm -f)", got, want)
	}
}

func TestCloseStillRemovesTheProxyWhenStopFails(t *testing.T) {
	f := &execx.Fake{Handler: func(c execx.Cmd) (execx.Result, error) {
		if c.Args[0] == "stop" {
			return execx.Fail("docker", 1, "Error: cannot stop")
		}
		return execx.Result{}, nil
	}}
	s := &Session{m: &Manager{Exec: f}, proxyRef: "p1", netRef: "n1"}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.Lines(), "|"); !strings.Contains(got, "[docker rm -f -v p1]") {
		t.Errorf("calls = %s: a failed stop must not leave the proxy behind", got)
	}
}

func decisionLine(d Decision) string { b, _ := json.Marshal(d); return string(b) + "\n" }

func TestDecisionsAsksForAFlushAndWaitsForTheAcknowledgement(t *testing.T) {
	var logs atomic.Int32
	killed := false
	f := &execx.Fake{}
	f.Handler = func(c execx.Cmd) (execx.Result, error) {
		switch c.Args[0] {
		case "kill":
			killed = true
			if strings.Join(c.Args, " ") != "kill --signal USR1 proxy" {
				t.Errorf("kill args %v", c.Args)
			}
			return execx.Result{}, nil
		case "logs":
			n := logs.Add(1)
			out := decisionLine(Decision{Host: "a.test", Reason: ReasonNotAllowlist}) + decisionLine(Decision{Reason: ReasonFlushed, Count: 3})
			switch {
			case !killed: // the read before the request: an old acknowledgement, a stale summary
				out += decisionLine(Decision{Reason: ReasonSuppressed, Count: 1})
			case n < 4: // the proxy has not answered yet
				out += decisionLine(Decision{Reason: ReasonSuppressed, Count: 1})
			default: // answered: the summary is current and the marker rose
				out += decisionLine(Decision{Reason: ReasonSuppressed, Count: 40}) + decisionLine(Decision{Reason: ReasonFlushed, Count: 4})
			}
			return execx.Result{Stdout: out}, nil
		}
		return execx.Result{}, nil
	}
	s := &Session{m: &Manager{Exec: f}, ProxyContainer: "proxy"}
	ds, err := s.Decisions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var sum int
	for _, d := range ds {
		if d.Reason == ReasonFlushed {
			t.Errorf("the acknowledgement marker is bookkeeping and must not be returned: %+v", d)
		}
		if d.Reason == ReasonSuppressed {
			sum = d.Count
		}
	}
	if sum != 40 || len(ds) != 2 {
		t.Errorf("decisions %+v: want the post-flush summary (40), got %d", ds, sum)
	}
}

func TestDecisionsOfAProxyThatIsAlreadyGoneAreReadAsTheyAre(t *testing.T) {
	f := &execx.Fake{Handler: func(c execx.Cmd) (execx.Result, error) {
		switch c.Args[0] {
		case "kill":
			return execx.Fail("docker", 1, "Error response from daemon: container is not running")
		case "logs":
			return execx.Result{Stdout: decisionLine(Decision{Host: "a.test", Reason: ReasonNotAllowlist})}, nil
		}
		return execx.Result{}, nil
	}}
	s := &Session{m: &Manager{Exec: f}, ProxyContainer: "proxy"}
	start := time.Now()
	ds, err := s.Decisions(context.Background())
	if err != nil || len(ds) != 1 || time.Since(start) > time.Second {
		t.Fatalf("ds=%v err=%v after %v", ds, err, time.Since(start))
	}
}

func TestSweepCarriesOnPastFailuresAndReportsAllOfThem(t *testing.T) {
	f := &execx.Fake{}
	f.Handler = func(c execx.Cmd) (execx.Result, error) {
		line := strings.Join(c.Args, " ")
		switch {
		case strings.HasPrefix(line, "ps -aq"):
			return execx.Result{Stdout: "c1\nc2\nc3\n"}, nil
		case strings.HasPrefix(line, "network ls"):
			return execx.Result{Stdout: "n1\nn2\n"}, nil
		case line == "rm -f -v c1", line == "network rm n1":
			return execx.Fail("docker", 1, "Error: refused "+c.Args[len(c.Args)-1])
		}
		return execx.Result{}, nil
	}
	n, err := (&Manager{Exec: f}).Sweep(context.Background())
	if n != 3 { // c2, c3 and n2
		t.Errorf("removed %d, want 3: a failure must not stop the others", n)
	}
	if err == nil || !strings.Contains(err.Error(), "c1") || !strings.Contains(err.Error(), "n1") {
		t.Errorf("err = %v, want both failures (c1 and n1) reported", err)
	}
	removed := strings.Join(f.Lines(), "|")
	for _, want := range []string{"rm -f -v c2", "rm -f -v c3", "network rm n2"} {
		if !strings.Contains(removed, want) {
			t.Errorf("never ran %q: %s", want, removed)
		}
	}
}

func TestSweepListsOnlyThisOwnersSessions(t *testing.T) {
	f := &execx.Fake{}
	if _, err := (&Manager{Exec: f, Owner: "mini"}).Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i, l := range f.Lines() {
		if !strings.Contains(l, "label="+LabelOwner+"=mini") {
			t.Errorf("call %d %s is not scoped to the owner", i, l)
		}
	}
	f2 := &execx.Fake{}
	_, _ = (&Manager{Exec: f2}).Sweep(context.Background())
	if !strings.Contains(f2.Line(0), "label="+LabelOwner+"="+DefaultOwner) {
		t.Errorf("an unset owner sweeps %s", f2.Line(0))
	}
}

// ---- real Docker ----------------------------------------------------------------------------

func TestRealDocker_SweepRemovesOnlyItsOwnersSessions(t *testing.T) {
	img := needDocker(t)
	h := newHostService(t)
	mine, _ := fixtureManager(t, img, h, "allowed.test")
	mine.Owner = "owner-a"
	theirs, _ := fixtureManager(t, img, h, "allowed.test")
	theirs.Owner = "owner-b"
	cleanSlate(t)
	_ = mustSweep(t, &Manager{Exec: execx.OS{}, Owner: "owner-a"})
	_ = mustSweep(t, &Manager{Exec: execx.OS{}, Owner: "owner-b"})

	a, err := mine.Open(ctx, uniq("sa"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := theirs.Open(ctx, uniq("sb"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(ctx); _ = b.Close(ctx) })

	if n := mustSweep(t, mine); n != 2 {
		t.Errorf("owner-a's sweep removed %d objects, want its own proxy and network (2)", n)
	}
	if containerExists(a.ProxyContainer) || networkExists(a.Network) {
		t.Error("the sweep left its own session behind")
	}
	if !containerExists(b.ProxyContainer) || !networkExists(b.Network) {
		t.Error("owner-a's sweep removed owner-b's live session")
	}
}

func mustSweep(t *testing.T, m *Manager) int {
	t.Helper()
	n, err := m.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// killedAfter runs the real docker command and THEN reports what a killed CLI or a cancelled
// context reports: the daemon did the work, the caller never learns the id.
type killedAfter struct {
	inner  execx.Runner
	match  func(args []string) bool
	cancel context.CancelFunc
}

func (k killedAfter) Run(c context.Context, cmd execx.Cmd) (execx.Result, error) {
	if !k.match(cmd.Args) {
		return k.inner.Run(c, cmd)
	}
	if _, err := k.inner.Run(context.Background(), cmd); err != nil {
		return execx.Result{}, err
	}
	k.cancel()
	return execx.Result{}, fmt.Errorf("signal: killed: %w", context.Canceled)
}
func (k killedAfter) LookPath(n string) (string, error) { return k.inner.LookPath(n) }

func TestRealDocker_AnOpenCancelledAfterTheDaemonCreatedSomethingRemovesIt(t *testing.T) {
	img := needDocker(t)
	h := newHostService(t)
	cleanSlate(t)
	cases := map[string]func(args []string) bool{
		"network create": func(a []string) bool {
			return len(a) > 1 && a[0] == "network" && a[1] == "create" && strings.Contains(strings.Join(a, " "), "--internal")
		},
		"container create": func(a []string) bool { return len(a) > 0 && a[0] == "create" },
	}
	for name, match := range cases {
		t.Run(name, func(t *testing.T) {
			m, _ := fixtureManager(t, img, h, "allowed.test")
			cctx, cancel := context.WithCancel(ctx)
			defer cancel()
			m.Exec = killedAfter{inner: execx.OS{}, match: match, cancel: cancel}
			id := uniq("k")
			// A live session with the SAME id (name collision) must survive: only what THIS
			// Open made may be removed.
			if _, err := m.Open(cctx, id); err == nil {
				t.Fatal("Open succeeded although its create call was killed")
			}
			if c, n := leftovers(t); c != "" || n != "" {
				t.Errorf("the cancelled Open left %q %q behind", c, n)
			}
		})
	}
}

func TestRealDocker_ACancelledOpenNeverRemovesALiveSessionWithTheSameID(t *testing.T) {
	img := needDocker(t)
	h := newHostService(t)
	cleanSlate(t)
	m, _ := fixtureManager(t, img, h, "allowed.test")
	id := uniq("same")
	live, err := m.Open(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.Close(ctx) })

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m2, _ := fixtureManager(t, img, h, "allowed.test")
	m2.Exec = killedAfter{inner: execx.OS{}, cancel: cancel, match: func(a []string) bool { return len(a) > 0 && a[0] == "ps" }}
	if _, err := m2.Open(cctx, id); err == nil {
		t.Fatal("a second Open with a live session's id must fail")
	}
	if !containerExists(live.ProxyContainer) || !networkExists(live.Network) {
		t.Error("the failed Open removed the live session that has the same id")
	}
}

// SIGTERM is the path Close takes (stop, then rm): the proxy must write its pending summary on
// the way out, not only when it next happens to be asked something.
func TestRealDocker_ASigtermedProxyWritesItsPendingSummaryBeforeItExits(t *testing.T) {
	img := needDocker(t)
	h := newHostService(t)
	m, _ := fixtureManager(t, img, h, "allowed.test")
	m.ProxyExtraArgs = append(m.ProxyExtraArgs, "-max-logged-denials", "1")
	s := open(t, m)
	port := fmt.Sprint(h.port)
	for i := 0; i < 6; i++ {
		job(t, img, s, "via-proxy", s.ProxyAddr, fmt.Sprintf("flood%d.test:%s", i, port))
	}
	dockerCLI(t, "stop", "--time", "5", s.ProxyContainer)
	logs, err := exec.Command("docker", "logs", s.ProxyContainer).CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	final := 0
	for _, d := range parseDecisionLines(string(logs)) {
		if d.Reason == ReasonSuppressed {
			final = d.Count
		}
	}
	if final != 5 {
		t.Errorf("the last summary in the proxy's log counts %d refusals, want 5 (6 refused, 1 itemised):\n%s", final, logs)
	}
}
