package egress

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// listFlag collects a repeatable flag.
type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(s string) error { *l = append(*l, s); return nil }

// RunProxy is the body of the proxy command, shared by the production binary
// (cmd/egress-proxy, which passes nil) and by test images. extend may add flags to fs and
// return a function that adjusts the Proxy once flags are parsed; production has no use for it.
//
//	egress-proxy -listen-cidr 172.19.0.0/16 -port 3128 -ports 443 -allow github.com ...
//	egress-proxy check 172.19.0.2:3128
//
// Decisions are written to stdout as JSON lines. It returns the process exit code.
func RunProxy(args []string, stdout, stderr io.Writer, extend func(fs *flag.FlagSet) func(*Proxy)) int {
	if len(args) >= 2 && args[0] == "check" {
		if err := Check(args[1]); err != nil {
			fmt.Fprintln(stderr, "egress-proxy check:", err)
			return 1
		}
		return 0
	}
	fs := flag.NewFlagSet("egress-proxy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cidr := fs.String("listen-cidr", "", "listen only on this machine's address inside this CIDR (the job network)")
	port := fs.Int("port", ProxyPort, "port to listen on")
	ports := fs.String("ports", "443", "comma-separated ports a CONNECT may target")
	maxDenied := fs.Int("max-logged-denials", DefaultMaxLoggedDenials, "refused requests to itemise in the decision log; later ones are counted in a summary line")
	var allow listFlag
	fs.Var(&allow, "allow", "an allowed hostname or *.domain wildcard, which matches every depth below domain (repeatable)")
	var adjust func(*Proxy)
	if extend != nil {
		adjust = extend(fs)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	list, err := ParseAllowlist(allow)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	var pp []int
	for _, f := range strings.Split(*ports, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || n < 1 || n > 65535 {
			fmt.Fprintf(stderr, "invalid port %q\n", f)
			return 2
		}
		pp = append(pp, n)
	}
	prefix, err := netip.ParsePrefix(*cidr)
	if err != nil {
		fmt.Fprintf(stderr, "-listen-cidr is required and must be a CIDR: %v\n", err)
		return 2
	}
	l, err := ListenInCIDR(prefix, *port, 10*time.Second)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	dlog := NewDecisionLog(stdout)
	dlog.MaxDenied = *maxDenied
	defer dlog.Flush() // SIGTERM ends Serve, and the return path writes what is pending
	p := &Proxy{Allow: list, Ports: pp, Observe: dlog.Record}
	if adjust != nil {
		adjust(p)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	go dlog.Run(ctx) // a quiet proxy's log must not stay stale: summaries and aggregates are timed
	if sigs := flushSignals(); len(sigs) > 0 {
		// A flush request (docker kill -s USR1): write everything pending, then say so, so the
		// reader can tell when the log is current.
		fc := make(chan os.Signal, 1)
		signal.Notify(fc, sigs...)
		defer signal.Stop(fc)
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-fc:
					dlog.Ack()
				}
			}
		}()
	}
	go func() { <-ctx.Done(); _ = p.Close() }()
	fmt.Fprintf(stderr, "egress-proxy listening on %s\n", l.Addr())
	if err := p.Serve(l); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// ListenInCIDR listens on this machine's address inside prefix, retrying for up to wait because
// a network attached at container start can show up a moment late. It never listens on a
// wildcard address: the proxy must not be reachable from the outbound network it also sits on.
func ListenInCIDR(prefix netip.Prefix, port int, wait time.Duration) (net.Listener, error) {
	deadline := time.Now().Add(wait)
	for {
		addrs, _ := net.InterfaceAddrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(ipn.IP)
			if ok && prefix.Contains(ip.Unmap()) {
				return net.Listen("tcp", net.JoinHostPort(ip.Unmap().String(), strconv.Itoa(port)))
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("this machine has no address inside %s to listen on", prefix)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Check is the readiness probe: it asks the proxy at addr for something it must refuse and
// expects the refusal, which proves the HTTP server is up and answering.
func Check(addr string) error {
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(c, "GET / HTTP/1.1\r\nHost: check\r\nConnection: close\r\n\r\n"); err != nil {
		return err
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if !strings.HasPrefix(line, "HTTP/1.1 405") {
		return fmt.Errorf("unexpected answer %q", strings.TrimSpace(line))
	}
	return nil
}
