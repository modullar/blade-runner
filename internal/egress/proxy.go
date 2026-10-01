package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// Resolver turns a hostname into addresses. *net.Resolver satisfies it; tests supply fixtures
// so they need no DNS.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Reasons recorded in a Decision. They are the vocabulary documented under BR-E083.
const (
	ReasonAllowed       = "allowed"
	ReasonMethod        = "method"
	ReasonBadHost       = "bad-host"
	ReasonIPLiteral     = "ip-literal"
	ReasonBadPort       = "bad-port"
	ReasonNotAllowlist  = "not-allowlisted"
	ReasonUnresolvable  = "unresolvable"
	ReasonForbiddenAddr = "forbidden-address"
	ReasonConnectFailed = "connect-failed"
	ReasonTooMany       = "too-many-connections"
)

// Decision is the proxy's record of one request. It holds only what a CONNECT carries (a host
// and a port) and what the proxy did about it: never a payload, never a credential.
type Decision struct {
	Time     time.Time `json:"time"`
	Client   string    `json:"client"`
	Host     string    `json:"host"`
	Port     string    `json:"port"`
	Allowed  bool      `json:"allowed"`
	Reason   string    `json:"reason"`
	Detail   string    `json:"detail,omitempty"`
	Resolved []string  `json:"resolved,omitempty"`
}

// Proxy is an HTTP CONNECT forward proxy that tunnels only to allowlisted hostnames.
//
// For each request it: accepts only CONNECT to a plain hostname (never an IP literal) on an
// allowed port; checks the allowlist; resolves the name ONCE; refuses if ANY resolved address
// fails the AddressPolicy; and connects to the address it checked, not to the name. Connecting
// to the checked address is what makes DNS rebinding irrelevant: a second answer from the
// resolver is never asked for. A dial-time check on the address the socket really connects to
// backs that up.
type Proxy struct {
	Allow    Allowlist
	Ports    []int          // default DefaultPorts
	Resolver Resolver       // default net.DefaultResolver
	Policy   AddressPolicy  // default PublicOnly{}
	Observe  func(Decision) // called once per request; may be nil

	MaxConns          int           // simultaneous tunnels; default 256
	ResolveTimeout    time.Duration // default 5s
	DialTimeout       time.Duration // default 10s per address
	IdleTimeout       time.Duration // a tunnel with no traffic this long is closed; default 2m
	ReadHeaderTimeout time.Duration // default 10s

	mu     sync.Mutex
	srv    *http.Server
	tunnel map[net.Conn]struct{}
	slots  chan struct{}
}

func (p *Proxy) ports() []int {
	if len(p.Ports) == 0 {
		return DefaultPorts
	}
	return p.Ports
}

func (p *Proxy) resolver() Resolver {
	if p.Resolver == nil {
		return net.DefaultResolver
	}
	return p.Resolver
}

func (p *Proxy) policy() AddressPolicy {
	if p.Policy == nil {
		return PublicOnly{}
	}
	return p.Policy
}

func dflt(d, v time.Duration) time.Duration {
	if d <= 0 {
		return v
	}
	return d
}

// Serve accepts connections on l until Close. It returns nil after Close.
func (p *Proxy) Serve(l net.Listener) error {
	p.mu.Lock()
	n := p.MaxConns
	if n <= 0 {
		n = 256
	}
	p.slots = make(chan struct{}, n)
	p.tunnel = map[net.Conn]struct{}{}
	p.srv = &http.Server{
		Handler:           http.HandlerFunc(p.handle),
		ReadHeaderTimeout: dflt(p.ReadHeaderTimeout, 10*time.Second),
		MaxHeaderBytes:    8 << 10,
	}
	srv := p.srv
	p.mu.Unlock()
	if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Close stops accepting and closes every open tunnel.
func (p *Proxy) Close() error {
	p.mu.Lock()
	srv := p.srv
	conns := make([]net.Conn, 0, len(p.tunnel))
	for c := range p.tunnel {
		conns = append(conns, c)
	}
	p.mu.Unlock()
	var err error
	if srv != nil {
		err = srv.Close()
	}
	for _, c := range conns {
		_ = c.Close()
	}
	return err
}

func (p *Proxy) observe(d Decision) {
	d.Time = time.Now().UTC()
	if p.Observe != nil {
		p.Observe(d)
	}
}

// deny answers with a status the client can read and records why.
func (p *Proxy) deny(w http.ResponseWriter, d Decision, status int) {
	d.Allowed = false
	p.observe(d)
	w.Header().Set("X-Egress-Denied", d.Reason)
	if status == http.StatusMethodNotAllowed {
		w.Header().Set("Allow", http.MethodConnect)
	}
	http.Error(w, "BR-E083 egress denied: "+d.Reason, status)
}

func (p *Proxy) handle(w http.ResponseWriter, r *http.Request) {
	d := Decision{Client: r.RemoteAddr}
	if r.Method != http.MethodConnect {
		d.Reason = ReasonMethod
		p.deny(w, d, http.StatusMethodNotAllowed)
		return
	}
	hostRaw, port, err := net.SplitHostPort(r.Host)
	d.Host, d.Port = hostRaw, port
	if err != nil {
		d.Reason, d.Detail = ReasonBadHost, "the target must be host:port"
		p.deny(w, d, http.StatusBadRequest)
		return
	}
	if _, err := netip.ParseAddr(hostRaw); err == nil {
		d.Reason = ReasonIPLiteral
		p.deny(w, d, http.StatusForbidden)
		return
	}
	host, ok := NormalizeHost(hostRaw)
	if !ok {
		d.Reason = ReasonBadHost
		p.deny(w, d, http.StatusBadRequest)
		return
	}
	d.Host = host
	if !p.portAllowed(port) {
		d.Reason = ReasonBadPort
		p.deny(w, d, http.StatusForbidden)
		return
	}
	if !p.Allow.Allowed(host) {
		d.Reason = ReasonNotAllowlist
		p.deny(w, d, http.StatusForbidden)
		return
	}

	select {
	case p.slots <- struct{}{}:
		defer func() { <-p.slots }()
	default:
		d.Reason = ReasonTooMany
		p.deny(w, d, http.StatusServiceUnavailable)
		return
	}

	rctx, cancel := context.WithTimeout(r.Context(), dflt(p.ResolveTimeout, 5*time.Second))
	addrs, err := p.resolver().LookupNetIP(rctx, "ip", host)
	cancel()
	if err != nil || len(addrs) == 0 {
		d.Reason, d.Detail = ReasonUnresolvable, "the name did not resolve"
		p.deny(w, d, http.StatusBadGateway)
		return
	}
	for i := range addrs {
		addrs[i] = addrs[i].Unmap()
		d.Resolved = append(d.Resolved, addrs[i].String())
	}
	// ALL addresses must pass, not just the one we would use: a name that also points inside is
	// not one we trust to be answered the same way next time.
	for _, a := range addrs {
		if err := p.policy().Check(a); err != nil {
			d.Reason, d.Detail = ReasonForbiddenAddr, err.Error()
			p.deny(w, d, http.StatusForbidden)
			return
		}
	}

	upstream, err := p.dial(r.Context(), addrs, port)
	if err != nil {
		d.Reason, d.Detail = ReasonConnectFailed, err.Error()
		p.deny(w, d, http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		upstream.Close()
		http.Error(w, "cannot hijack", http.StatusInternalServerError)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		upstream.Close()
		return
	}
	d.Allowed, d.Reason = true, ReasonAllowed
	p.observe(d)

	idle := dflt(p.IdleTimeout, 2*time.Minute)
	c, u := &idleConn{Conn: client, idle: idle}, &idleConn{Conn: upstream, idle: idle}
	p.track(c, u)
	defer p.untrack(c, u)
	defer c.Close()
	defer u.Close()
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	// Anything the client pipelined behind the CONNECT is already in the server's buffer.
	if n := buf.Reader.Buffered(); n > 0 {
		pending, _ := buf.Reader.Peek(n)
		if _, err := u.Write(pending); err != nil {
			return
		}
	}
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}
	go pipe(u, c)
	go pipe(c, u)
	<-done
	<-done
}

func (p *Proxy) portAllowed(port string) bool {
	n, err := strconv.Atoi(port)
	if err != nil || strconv.Itoa(n) != port {
		return false
	}
	for _, ok := range p.ports() {
		if n == ok {
			return true
		}
	}
	return false
}

// dial connects to one of the already-checked addresses. The address is passed as a literal, so
// nothing is resolved again, and Control re-checks the address the kernel is about to connect to.
func (p *Proxy) dial(ctx context.Context, addrs []netip.Addr, port string) (net.Conn, error) {
	dialer := &net.Dialer{
		Timeout: dflt(p.DialTimeout, 10*time.Second),
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return fmt.Errorf("unreadable dial target %q", address)
			}
			return p.policy().Check(ap.Addr())
		},
	}
	var last error
	for _, a := range addrs {
		c, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(a.String(), port))
		if err == nil {
			return c, nil
		}
		last = err
	}
	return nil, last
}

func (p *Proxy) track(cs ...net.Conn) {
	p.mu.Lock()
	for _, c := range cs {
		p.tunnel[c] = struct{}{}
	}
	p.mu.Unlock()
}

func (p *Proxy) untrack(cs ...net.Conn) {
	p.mu.Lock()
	for _, c := range cs {
		delete(p.tunnel, c)
	}
	p.mu.Unlock()
}

// idleConn closes a connection that has moved no data for idle: a stalled tunnel must not hold
// a slot forever.
type idleConn struct {
	net.Conn
	idle time.Duration
}

func (c *idleConn) Read(b []byte) (int, error) {
	_ = c.Conn.SetReadDeadline(time.Now().Add(c.idle))
	return c.Conn.Read(b)
}

func (c *idleConn) Write(b []byte) (int, error) {
	_ = c.Conn.SetWriteDeadline(time.Now().Add(c.idle))
	return c.Conn.Write(b)
}

func (c *idleConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}
