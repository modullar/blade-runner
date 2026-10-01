package egress

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- fixtures: real sockets, no DNS --------------------------------------------------

// target is a real TCP listener standing in for a service the job might reach. It echoes
// whatever it receives, prefixed by a greeting, and counts how many connections it accepted:
// for a forbidden target the count must stay zero, which is the only proof that nothing connected.
type target struct {
	l        net.Listener
	accepted atomic.Int64
}

func newTarget(t *testing.T) *target {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tg := &target{l: l}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			tg.accepted.Add(1)
			go func() {
				defer c.Close()
				io.WriteString(c, "hello\n")
				io.Copy(c, c) // echo
			}()
		}
	}()
	return tg
}

func (tg *target) port() string { return strconv.Itoa(tg.l.Addr().(*net.TCPAddr).Port) }

// fixedResolver answers from a table, and counts lookups. A name may have a different answer
// on each call (answers[i]), which is how DNS rebinding looks to the proxy.
type fixedResolver struct {
	mu      sync.Mutex
	answers map[string][][]netip.Addr
	calls   map[string]int
	queried []string // the names exactly as the proxy asked for them
}

func newResolver(m map[string][]string) *fixedResolver {
	r := &fixedResolver{answers: map[string][][]netip.Addr{}, calls: map[string]int{}}
	for name, ips := range m {
		r.add(name, ips...)
	}
	return r
}

func (r *fixedResolver) add(name string, ips ...string) {
	var as []netip.Addr
	for _, ip := range ips {
		as = append(as, netip.MustParseAddr(ip))
	}
	r.answers[name] = append(r.answers[name], as)
}

func (r *fixedResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queried = append(r.queried, host)
	host = strings.TrimSuffix(host, ".") // the absolute form names the same host
	seq, ok := r.answers[host]
	if !ok {
		return nil, fmt.Errorf("no such host %q", host)
	}
	i := r.calls[host]
	r.calls[host]++
	if i >= len(seq) {
		i = len(seq) - 1
	}
	return seq[i], nil
}

func (r *fixedResolver) lookups(host string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[host]
}

// loopbackOK is a test-only policy that treats loopback as the "internet" so a local listener
// can be an allowed target. Everything else is judged by the production policy.
type loopbackOK struct{}

func (loopbackOK) Check(a netip.Addr) error {
	if a.Unmap().IsLoopback() {
		return nil
	}
	return PublicOnly{}.Check(a)
}

type recorder struct {
	mu sync.Mutex
	ds []Decision
}

func (r *recorder) observe(d Decision) { r.mu.Lock(); r.ds = append(r.ds, d); r.mu.Unlock() }
func (r *recorder) last() Decision {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.ds) == 0 {
		return Decision{}
	}
	return r.ds[len(r.ds)-1]
}
func (r *recorder) count() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.ds) }

// startProxy serves p on a real loopback socket and returns its address and decision log.
func startProxy(t *testing.T, p *Proxy) (string, *recorder) {
	t.Helper()
	rec := &recorder{}
	p.Observe = rec.observe
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- p.Serve(l) }()
	t.Cleanup(func() {
		p.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return after Close")
		}
	})
	return l.Addr().String(), rec
}

func allow(t *testing.T, entries ...string) Allowlist {
	t.Helper()
	a, err := ParseAllowlist(entries)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// connectVia sends one CONNECT through the proxy and returns the status line, and the open
// connection and reader if it was accepted.
func connectVia(t *testing.T, proxy, authority string, pipelined string) (status string, c net.Conn, r *bufio.Reader) {
	t.Helper()
	c, err := net.DialTimeout("tcp", proxy, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n%s", authority, authority, pipelined)
	r = bufio.NewReader(c)
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("CONNECT %s: %v", authority, err)
	}
	status = strings.TrimSpace(line)
	if strings.Contains(status, " 200 ") {
		for { // header block
			l, err := r.ReadString('\n')
			if err != nil || strings.TrimSpace(l) == "" {
				break
			}
		}
	}
	return status, c, r
}

// connectWithHost sends a CONNECT whose request target is authority but whose Host header is
// hostHeader, and returns the status line. It lets a test send a header Go's server accepts, so
// the request reaches the proxy's own checks instead of being rejected as malformed first.
func connectWithHost(t *testing.T, proxy, authority, hostHeader string) string {
	t.Helper()
	c, err := net.DialTimeout("tcp", proxy, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", authority, hostHeader)
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("CONNECT %s: %v", authority, err)
	}
	return strings.TrimSpace(line)
}

func wantStatus(t *testing.T, status string, code int) {
	t.Helper()
	if !strings.HasPrefix(status, "HTTP/1.1 "+strconv.Itoa(code)) {
		t.Errorf("status = %q, want %d", status, code)
	}
}

// ---- the proxy ------------------------------------------------------------------------

func TestAnAllowedNameIsTunnelledBothWays(t *testing.T) {
	tg := newTarget(t)
	addr, rec := startProxy(t, &Proxy{
		Allow: allow(t, "allowed.test"), Ports: []int{atoi(tg.port())},
		Resolver: newResolver(map[string][]string{"allowed.test": {"127.0.0.1"}}), Policy: loopbackOK{},
	})
	status, c, r := connectVia(t, addr, "allowed.test:"+tg.port(), "")
	wantStatus(t, status, 200)
	if greeting, _ := r.ReadString('\n'); greeting != "hello\n" {
		t.Errorf("greeting through the tunnel = %q", greeting)
	}
	io.WriteString(c, "ping\n")
	if echo, _ := r.ReadString('\n'); echo != "ping\n" {
		t.Errorf("echo through the tunnel = %q", echo)
	}
	if d := rec.last(); !d.Allowed || d.Reason != ReasonAllowed || d.Host != "allowed.test" || len(d.Resolved) != 1 {
		t.Errorf("decision = %+v", d)
	}
}

func TestBytesPipelinedBehindTheConnectAreDelivered(t *testing.T) {
	tg := newTarget(t)
	addr, _ := startProxy(t, &Proxy{
		Allow: allow(t, "allowed.test"), Ports: []int{atoi(tg.port())},
		Resolver: newResolver(map[string][]string{"allowed.test": {"127.0.0.1"}}), Policy: loopbackOK{},
	})
	status, _, r := connectVia(t, addr, "allowed.test:"+tg.port(), "early\n")
	wantStatus(t, status, 200)
	r.ReadString('\n') // greeting
	if echo, _ := r.ReadString('\n'); echo != "early\n" {
		t.Errorf("echo of the pipelined bytes = %q: data sent right after CONNECT must not be lost", echo)
	}
}

func TestANameNotOnTheListIsRefusedAndNothingConnects(t *testing.T) {
	tg := newTarget(t)
	res := newResolver(map[string][]string{"allowed.test": {"127.0.0.1"}, "forbidden.test": {"127.0.0.1"}})
	addr, rec := startProxy(t, &Proxy{Allow: allow(t, "allowed.test"), Ports: []int{atoi(tg.port())}, Resolver: res, Policy: loopbackOK{}})
	status, _, _ := connectVia(t, addr, "forbidden.test:"+tg.port(), "")
	wantStatus(t, status, 403)
	if d := rec.last(); d.Allowed || d.Reason != ReasonNotAllowlist {
		t.Errorf("decision = %+v", d)
	}
	if res.lookups("forbidden.test") != 0 {
		t.Error("a name that is not allowed must not even be resolved")
	}
	if tg.accepted.Load() != 0 {
		t.Error("the target was contacted for a forbidden name")
	}
}

// The central guarantee: a name on the allowlist that resolves to somewhere internal does not
// become a way to reach it.
func TestAnAllowedNameThatResolvesInsideIsRefused(t *testing.T) {
	tg := newTarget(t) // a real listener on loopback: if the proxy connected, accepted would be 1
	for _, ip := range []string{"127.0.0.1", "::1", "::ffff:127.0.0.1", "10.0.0.5", "192.168.1.1", "172.16.0.9", "169.254.169.254", "100.100.100.200", "0.0.0.0", "fd00:ec2::254"} {
		t.Run(ip, func(t *testing.T) {
			addr, rec := startProxy(t, &Proxy{ // the PRODUCTION policy
				Allow: allow(t, "allowed.test"), Ports: []int{atoi(tg.port())},
				Resolver: newResolver(map[string][]string{"allowed.test": {ip}}),
			})
			status, _, _ := connectVia(t, addr, "allowed.test:"+tg.port(), "")
			wantStatus(t, status, 403)
			if d := rec.last(); d.Allowed || d.Reason != ReasonForbiddenAddr {
				t.Errorf("decision = %+v", d)
			}
		})
	}
	if tg.accepted.Load() != 0 {
		t.Errorf("the loopback target was contacted %d times although every name resolved inside", tg.accepted.Load())
	}
}

func TestOneInternalAddressAmongPublicOnesRefusesTheName(t *testing.T) {
	tg := newTarget(t)
	addr, rec := startProxy(t, &Proxy{
		Allow: allow(t, "allowed.test"), Ports: []int{atoi(tg.port())},
		Resolver: newResolver(map[string][]string{"allowed.test": {"93.184.216.34", "127.0.0.1"}}),
	})
	status, _, _ := connectVia(t, addr, "allowed.test:"+tg.port(), "")
	wantStatus(t, status, 403)
	if d := rec.last(); d.Reason != ReasonForbiddenAddr || len(d.Resolved) != 2 {
		t.Errorf("decision = %+v", d)
	}
	if tg.accepted.Load() != 0 {
		t.Error("the proxy connected although one answer was internal")
	}
}

func TestAnIPLiteralIsNeverAccepted(t *testing.T) {
	tg := newTarget(t)
	addr, rec := startProxy(t, &Proxy{Allow: allow(t, "allowed.test"), Ports: []int{atoi(tg.port())}, Resolver: newResolver(nil), Policy: loopbackOK{}})
	for _, authority := range []string{"127.0.0.1:" + tg.port(), "[::1]:" + tg.port(), "169.254.169.254:" + tg.port(), "93.184.216.34:" + tg.port(), "[::ffff:127.0.0.1]:" + tg.port()} {
		status, _, _ := connectVia(t, addr, authority, "")
		wantStatus(t, status, 403)
		if d := rec.last(); d.Reason != ReasonIPLiteral {
			t.Errorf("%s: decision = %+v", authority, d)
		}
	}
	if tg.accepted.Load() != 0 {
		t.Error("an IP literal reached the target")
	}
}

func TestMalformedTargetsAreRefusedWithTheirOwnCodes(t *testing.T) {
	tg := newTarget(t)
	addr, rec := startProxy(t, &Proxy{
		Allow: allow(t, "allowed.test"), Ports: []int{atoi(tg.port())},
		Resolver: newResolver(map[string][]string{"allowed.test": {"127.0.0.1"}}), Policy: loopbackOK{},
	})
	good := "allowed.test:" + tg.port() // a valid Host header, so Go's server hands the request over
	for name, tc := range map[string]struct {
		authority string
		status    int
		reason    string // what the proxy records; "" when Go's server refused it before the handler
	}{
		"no port":          {"allowed.test", 400, ReasonBadHost},
		"a path":           {"allowed.test/x:" + tg.port(), 400, ReasonBadHost},
		"a percent escape": {"allowed.test%2e:" + tg.port(), 400, ""},
		"an empty host":    {":" + tg.port(), 400, ReasonBadHost},
	} {
		before := rec.count()
		status := connectWithHost(t, addr, tc.authority, good)
		wantStatus(t, status, tc.status)
		if got := rec.count() - before; tc.reason != "" && (got != 1 || rec.last().Reason != tc.reason) {
			t.Errorf("%s: recorded %d decisions, last %+v; want one with reason %q", name, got, rec.last(), tc.reason)
		}
	}
	if tg.accepted.Load() != 0 {
		t.Error("a malformed target reached the target")
	}
}

// Go's HTTP server strips userinfo from a CONNECT authority before the handler sees it, so
// "evil.com@allowed.test" is the host allowed.test and "allowed.test@evil.com" is the host
// evil.com. The proxy must act on that host and nothing else: the part before the "@" must
// neither grant access (an allowed name used as a user) nor be dialled (a forbidden name used
// as a user). Each request carries a VALID Host header, so the request reaches the proxy.
func TestUserinfoInTheTargetIsIgnoredNotTrusted(t *testing.T) {
	tg := newTarget(t)
	addr, rec := startProxy(t, &Proxy{
		Allow: allow(t, "allowed.test"), Ports: []int{atoi(tg.port())},
		Resolver: newResolver(map[string][]string{"allowed.test": {"127.0.0.1"}, "evil.com": {"127.0.0.1"}}), Policy: loopbackOK{},
	})
	good := "allowed.test:" + tg.port()

	t.Run("a forbidden name as the user does not matter: the host is the allowed one", func(t *testing.T) {
		wantStatus(t, connectWithHost(t, addr, "evil.com@allowed.test:"+tg.port(), good), 200)
		d := rec.last()
		if !d.Allowed || d.Host != "allowed.test" || d.Reason != ReasonAllowed {
			t.Errorf("decision = %+v, want allowed.test", d)
		}
	})
	t.Run("an allowed name as the user grants nothing: the host is evil.com", func(t *testing.T) {
		before := tg.accepted.Load()
		wantStatus(t, connectWithHost(t, addr, "allowed.test@evil.com:"+tg.port(), good), 403)
		d := rec.last()
		if d.Allowed || d.Host != "evil.com" || d.Reason != ReasonNotAllowlist {
			t.Errorf("decision = %+v, want a refusal of evil.com as not-allowlisted", d)
		}
		if tg.accepted.Load() != before {
			t.Error("evil.com was connected to")
		}
	})
}

func TestTheNameIsResolvedAsAbsoluteSoNoSearchDomainApplies(t *testing.T) {
	tg := newTarget(t)
	res := newResolver(map[string][]string{"allowed.test": {"127.0.0.1"}})
	addr, _ := startProxy(t, &Proxy{Allow: allow(t, "allowed.test"), Ports: []int{atoi(tg.port())}, Resolver: res, Policy: loopbackOK{}})
	for _, authority := range []string{"allowed.test:" + tg.port(), "ALLOWED.test.:" + tg.port()} {
		status, c, _ := connectVia(t, addr, authority, "")
		wantStatus(t, status, 200)
		c.Close()
	}
	res.mu.Lock()
	defer res.mu.Unlock()
	if len(res.queried) != 2 || res.queried[0] != "allowed.test." || res.queried[1] != "allowed.test." {
		t.Errorf("the resolver was asked for %q, want the absolute name \"allowed.test.\" both times", res.queried)
	}
}

func TestLoggedHostTextIsCapped(t *testing.T) {
	tg := newTarget(t)
	addr, rec := startProxy(t, &Proxy{
		Allow: allow(t, "allowed.test"), Ports: []int{atoi(tg.port())},
		Resolver: newResolver(nil), Policy: loopbackOK{},
	})
	long := strings.Repeat(strings.Repeat("a", 60)+".", 4) + "test" // 248 bytes, a valid name
	wantStatus(t, connectWithHost(t, addr, long+":"+tg.port(), "allowed.test:1"), 403)
	d := rec.last()
	if d.Reason != ReasonNotAllowlist || len(d.Host) > maxLoggedHost || !strings.HasPrefix(long, strings.TrimSuffix(d.Host, "...")) {
		t.Errorf("logged host = %q (%d bytes), want a prefix of the name, at most %d bytes", d.Host, len(d.Host), maxLoggedHost)
	}
	wantStatus(t, connectWithHost(t, addr, "allowed.test:"+strings.Repeat("9", 40), "allowed.test:1"), 403)
	if d := rec.last(); len(d.Port) > maxLoggedPort {
		t.Errorf("logged port = %q", d.Port)
	}
}

func TestHostCaseAndTrailingDotAreNormalisedBeforeTheAllowlist(t *testing.T) {
	tg := newTarget(t)
	res := newResolver(map[string][]string{"allowed.test": {"127.0.0.1"}})
	addr, _ := startProxy(t, &Proxy{Allow: allow(t, "allowed.test"), Ports: []int{atoi(tg.port())}, Resolver: res, Policy: loopbackOK{}})
	for _, authority := range []string{"ALLOWED.Test:" + tg.port(), "allowed.test.:" + tg.port()} {
		status, _, _ := connectVia(t, addr, authority, "")
		wantStatus(t, status, 200)
	}
}

func TestOnlyAllowedPortsAreReachable(t *testing.T) {
	tg := newTarget(t)
	addr, rec := startProxy(t, &Proxy{
		Allow: allow(t, "allowed.test"), Ports: []int{atoi(tg.port())},
		Resolver: newResolver(map[string][]string{"allowed.test": {"127.0.0.1"}}), Policy: loopbackOK{},
	})
	for _, port := range []string{"22", "80", "0", "65536", "0443"} {
		status, _, _ := connectVia(t, addr, "allowed.test:"+port, "")
		wantStatus(t, status, 403)
		if d := rec.last(); d.Reason != ReasonBadPort {
			t.Errorf("port %q: decision = %+v", port, d)
		}
	}
	// Spellings Go's own HTTP parser or the proxy's strict parse refuses: either answer is a refusal.
	for _, port := range []string{"-1", "abc", "+" + tg.port(), tg.port() + " ", ""} {
		status, _, _ := connectVia(t, addr, "allowed.test:"+port, "")
		if !strings.HasPrefix(status, "HTTP/1.1 400") && !strings.HasPrefix(status, "HTTP/1.1 403") {
			t.Errorf("port %q: status = %q, want a refusal", port, status)
		}
	}
	if tg.accepted.Load() != 0 {
		t.Error("a disallowed port reached the target")
	}
}

func TestOnlyConnectIsSupported(t *testing.T) {
	tg := newTarget(t)
	addr, rec := startProxy(t, &Proxy{
		Allow: allow(t, "allowed.test"), Ports: []int{atoi(tg.port())},
		Resolver: newResolver(map[string][]string{"allowed.test": {"127.0.0.1"}}), Policy: loopbackOK{},
	})
	for _, req := range []string{
		"GET http://allowed.test:" + tg.port() + "/ HTTP/1.1\r\nHost: allowed.test\r\n\r\n",
		"POST http://allowed.test/ HTTP/1.1\r\nHost: allowed.test\r\nContent-Length: 0\r\n\r\n",
		"GET / HTTP/1.1\r\nHost: allowed.test\r\n\r\n",
	} {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		io.WriteString(c, req)
		line, _ := bufio.NewReader(c).ReadString('\n')
		c.Close()
		wantStatus(t, strings.TrimSpace(line), 405)
		if d := rec.last(); d.Reason != ReasonMethod {
			t.Errorf("decision = %+v", d)
		}
	}
	if tg.accepted.Load() != 0 {
		t.Error("a plain HTTP request reached the target")
	}
}

func TestAnUnresolvableAllowedNameFails(t *testing.T) {
	addr, rec := startProxy(t, &Proxy{Allow: allow(t, "allowed.test"), Ports: []int{443}, Resolver: newResolver(nil)})
	status, _, _ := connectVia(t, addr, "allowed.test:443", "")
	wantStatus(t, status, 502)
	if d := rec.last(); d.Reason != ReasonUnresolvable {
		t.Errorf("decision = %+v", d)
	}
}

// DNS rebinding: a name that answers with a safe address once and an internal one the next
// time. The proxy asks once and connects to the address it checked, so the second answer is
// never seen.
func TestRebindingCannotRedirectAConnectionAfterTheCheck(t *testing.T) {
	tg := newTarget(t)
	res := newResolver(nil)
	res.add("rebind.test", "127.0.0.1") // first answer: passes the (test) policy
	res.add("rebind.test", "10.9.9.9")  // every later answer: internal
	addr, _ := startProxy(t, &Proxy{Allow: allow(t, "rebind.test"), Ports: []int{atoi(tg.port())}, Resolver: res, Policy: loopbackOK{}})
	status, _, r := connectVia(t, addr, "rebind.test:"+tg.port(), "")
	wantStatus(t, status, 200)
	if g, _ := r.ReadString('\n'); g != "hello\n" {
		t.Errorf("greeting = %q: the connection must have gone to the checked address", g)
	}
	if n := res.lookups("rebind.test"); n != 1 {
		t.Errorf("the name was resolved %d times, want exactly once", n)
	}
}

// The address the socket really connects to is checked again, at connect time. This policy
// approves an address when asked about the resolution and refuses the same address afterwards,
// as if the world changed in between; only a check at dial time can catch that.
type flipPolicy struct{ calls atomic.Int64 }

func (f *flipPolicy) Check(netip.Addr) error {
	if f.calls.Add(1) == 1 {
		return nil
	}
	return fmt.Errorf("refused at connect time")
}

func TestTheDialedAddressIsCheckedAgain(t *testing.T) {
	tg := newTarget(t)
	pol := &flipPolicy{}
	addr, rec := startProxy(t, &Proxy{
		Allow: allow(t, "allowed.test"), Ports: []int{atoi(tg.port())},
		Resolver: newResolver(map[string][]string{"allowed.test": {"127.0.0.1"}}), Policy: pol,
	})
	status, _, _ := connectVia(t, addr, "allowed.test:"+tg.port(), "")
	wantStatus(t, status, 502)
	if d := rec.last(); d.Reason != ReasonConnectFailed || !strings.Contains(d.Detail, "refused at connect time") {
		t.Errorf("decision = %+v", d)
	}
	if tg.accepted.Load() != 0 {
		t.Error("the connection was made although the dial-time check refused it")
	}
}

func TestTooManyTunnelsAreRefusedAndASlotFreesUp(t *testing.T) {
	tg := newTarget(t)
	addr, rec := startProxy(t, &Proxy{
		Allow: allow(t, "allowed.test"), Ports: []int{atoi(tg.port())}, MaxConns: 1,
		Resolver: newResolver(map[string][]string{"allowed.test": {"127.0.0.1"}}), Policy: loopbackOK{},
	})
	status, first, _ := connectVia(t, addr, "allowed.test:"+tg.port(), "")
	wantStatus(t, status, 200)
	status, _, _ = connectVia(t, addr, "allowed.test:"+tg.port(), "")
	wantStatus(t, status, 503)
	if d := rec.last(); d.Reason != ReasonTooMany {
		t.Errorf("decision = %+v", d)
	}
	first.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, c, _ := connectVia(t, addr, "allowed.test:"+tg.port(), "")
		if strings.Contains(status, " 200 ") {
			c.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the slot never freed after the first tunnel closed: %s", status)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// keepAliveDenied opens a connection, makes one request the proxy refuses (a plain GET), reads
// the answer and leaves the connection open: an idle, kept-alive client.
func keepAliveDenied(t *testing.T, addr string) (net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprint(c, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	r := bufio.NewReader(c)
	line, err := r.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "HTTP/1.1 405") {
		t.Fatalf("a refused request: %q %v", line, err)
	}
	for { // the rest of the response: headers, then the short body
		l, err := r.ReadString('\n')
		if err != nil || strings.TrimSpace(l) == "" {
			break
		}
	}
	r.ReadString('\n') // the body line
	return c, r
}

func TestEveryConnectionCountsAgainstTheCapNotOnlyTunnels(t *testing.T) {
	addr, rec := startProxy(t, &Proxy{Allow: allow(t, "allowed.test"), MaxConns: 3, IdleTimeout: 400 * time.Millisecond})
	for i := 0; i < 3; i++ { // three idle connections, none of them a tunnel
		keepAliveDenied(t, addr)
	}
	// The fourth connection is over the cap: answered 503 and closed, whatever it asks for.
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("the over-cap connection got no answer: %v", err)
	}
	wantStatus(t, strings.TrimSpace(line), 503)
	if d := rec.last(); d.Reason != ReasonTooMany {
		t.Errorf("decision = %+v", d)
	}
	// The idle ones are closed by IdleTimeout, which frees their slots.
	deadline := time.Now().Add(5 * time.Second)
	for {
		c2, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = c2.SetDeadline(time.Now().Add(2 * time.Second))
			fmt.Fprint(c2, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
			l, _ := bufio.NewReader(c2).ReadString('\n')
			c2.Close()
			if strings.HasPrefix(l, "HTTP/1.1 405") {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("slots held by idle connections were never freed")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestAnIdleKeepAliveConnectionIsClosedByIdleTimeout(t *testing.T) {
	addr, _ := startProxy(t, &Proxy{Allow: allow(t, "allowed.test"), IdleTimeout: 300 * time.Millisecond})
	c, r := keepAliveDenied(t, addr)
	start := time.Now()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := r.ReadByte(); err != io.EOF {
		t.Errorf("read = %v, want the proxy to close the idle connection (EOF)", err)
	}
	if time.Since(start) > 4*time.Second {
		t.Errorf("the idle connection stayed open for %v", time.Since(start))
	}
}

func TestAnIdleTunnelIsClosed(t *testing.T) {
	// A target that accepts and then says nothing.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	port := l.Addr().(*net.TCPAddr).Port
	addr, _ := startProxy(t, &Proxy{
		Allow: allow(t, "allowed.test"), Ports: []int{port}, IdleTimeout: 300 * time.Millisecond,
		Resolver: newResolver(map[string][]string{"allowed.test": {"127.0.0.1"}}), Policy: loopbackOK{},
	})
	status, c, r := connectVia(t, addr, "allowed.test:"+strconv.Itoa(port), "")
	wantStatus(t, status, 200)
	start := time.Now()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := r.ReadByte(); err != io.EOF {
		t.Errorf("read = %v, want the proxy to close the idle tunnel (EOF)", err)
	}
	if time.Since(start) > 4*time.Second {
		t.Errorf("the idle tunnel stayed open for %v", time.Since(start))
	}
}

func TestCloseEndsOpenTunnels(t *testing.T) {
	tg := newTarget(t)
	p := &Proxy{
		Allow: allow(t, "allowed.test"), Ports: []int{atoi(tg.port())},
		Resolver: newResolver(map[string][]string{"allowed.test": {"127.0.0.1"}}), Policy: loopbackOK{},
	}
	addr, _ := startProxy(t, p)
	status, c, r := connectVia(t, addr, "allowed.test:"+tg.port(), "")
	wantStatus(t, status, 200)
	r.ReadString('\n')
	p.Close()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := r.ReadByte(); err == nil || strings.Contains(err.Error(), "timeout") {
		t.Errorf("read = %v, want the tunnel closed by Close", err)
	}
}

func TestDefaultsApplyWhenNothingIsConfigured(t *testing.T) {
	p := &Proxy{}
	if got := p.ports(); len(got) != 1 || got[0] != 443 {
		t.Errorf("default ports = %v", got)
	}
	if _, ok := p.policy().(PublicOnly); !ok {
		t.Errorf("default policy = %T, must be PublicOnly", p.policy())
	}
	if p.resolver() != net.DefaultResolver {
		t.Error("default resolver must be the system resolver")
	}
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

// ---- idle is per tunnel, not per direction ---------------------------------------------

// A long download: the upstream trickles bytes for far longer than IdleTimeout while the client
// says nothing. The client's silence must not half-close the tunnel under the download.
func TestADownloadToASilentClientIsNotCutByIdleTimeout(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	const chunks = 12
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		var clientGone atomic.Bool
		go func() { // like most servers, give up on a client that has half-closed
			io.Copy(io.Discard, c)
			clientGone.Store(true)
		}()
		for i := 0; i < chunks && !clientGone.Load(); i++ {
			time.Sleep(100 * time.Millisecond) // 1.2s in total, 4x the idle timeout
			io.WriteString(c, "x")
		}
	}()
	port := l.Addr().(*net.TCPAddr).Port
	addr, _ := startProxy(t, &Proxy{
		Allow: allow(t, "allowed.test"), Ports: []int{port}, IdleTimeout: 300 * time.Millisecond,
		Resolver: newResolver(map[string][]string{"allowed.test": {"127.0.0.1"}}), Policy: loopbackOK{},
	})
	status, c, r := connectVia(t, addr, "allowed.test:"+strconv.Itoa(port), "")
	wantStatus(t, status, 200)
	_ = c.SetReadDeadline(time.Now().Add(8 * time.Second))
	got, err := io.ReadAll(r) // the client sends nothing at all
	if err != nil || len(got) != chunks {
		t.Fatalf("download delivered %d of %d bytes (err %v): the silent client's side idled out", len(got), chunks, err)
	}
}

// A long upload: the client trickles bytes for longer than IdleTimeout while the upstream says
// nothing until the client is done. The upstream's silence must not half-close the client's
// read side, or the final reply (here "done") never reaches it.
func TestAnUploadToASilentUpstreamIsNotCutByIdleTimeout(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	received := make(chan int, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		n, _ := io.Copy(io.Discard, c) // silent until the client half-closes
		received <- int(n)
		io.WriteString(c, "done")
	}()
	port := l.Addr().(*net.TCPAddr).Port
	addr, _ := startProxy(t, &Proxy{
		Allow: allow(t, "allowed.test"), Ports: []int{port}, IdleTimeout: 300 * time.Millisecond,
		Resolver: newResolver(map[string][]string{"allowed.test": {"127.0.0.1"}}), Policy: loopbackOK{},
	})
	status, c, r := connectVia(t, addr, "allowed.test:"+strconv.Itoa(port), "")
	wantStatus(t, status, 200)
	for i := 0; i < 12; i++ {
		time.Sleep(100 * time.Millisecond)
		if _, err := io.WriteString(c, "y"); err != nil {
			t.Fatalf("upload write %d: %v", i, err)
		}
	}
	c.(*net.TCPConn).CloseWrite()
	_ = c.SetReadDeadline(time.Now().Add(8 * time.Second))
	got, err := io.ReadAll(r)
	if string(got) != "done" || err != nil {
		t.Fatalf("reply = %q (err %v), want \"done\": the upstream's silence idled the tunnel out", got, err)
	}
	if n := <-received; n != 12 {
		t.Errorf("upstream received %d bytes, want 12", n)
	}
}
