package egress

import (
	"bytes"
	"encoding/json"
	"flag"
	"net"
	"net/netip"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestRunProxyRefusesBadConfiguration(t *testing.T) {
	for name, args := range map[string][]string{
		"no allowlist":      {"-listen-cidr", "127.0.0.0/8"},
		"an IP in the list": {"-listen-cidr", "127.0.0.0/8", "-allow", "1.2.3.4"},
		"no cidr":           {"-allow", "github.com"},
		"a bad cidr":        {"-listen-cidr", "everywhere", "-allow", "github.com"},
		"a bad port list":   {"-listen-cidr", "127.0.0.0/8", "-allow", "github.com", "-ports", "443,x"},
		"port zero":         {"-listen-cidr", "127.0.0.0/8", "-allow", "github.com", "-ports", "0"},
		"an unknown flag":   {"-listen-cidr", "127.0.0.0/8", "-allow", "github.com", "-allow-private"},
	} {
		var out, errb syncBuf
		if code := RunProxy(args, &out, &errb, nil); code == 0 {
			t.Errorf("%s: exit 0, want a refusal (stderr %q)", name, errb.String())
		}
	}
	var out, errb syncBuf
	RunProxy([]string{"-listen-cidr", "127.0.0.0/8", "-allow", "1.2.3.4"}, &out, &errb, nil)
	if !strings.Contains(errb.String(), "BR-E081") {
		t.Errorf("an invalid allowlist must say BR-E081: %q", errb.String())
	}
}

func TestRunProxyServesLogsDecisionsAndStopsOnSIGTERM(t *testing.T) {
	var out, errb syncBuf
	exit := make(chan int, 1)
	go func() {
		exit <- RunProxy([]string{"-listen-cidr", "127.0.0.0/8", "-port", "0", "-allow", "allowed.test"}, &out, &errb, nil)
	}()
	var addr string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if _, rest, ok := strings.Cut(errb.String(), "listening on "); ok {
			addr = strings.TrimSpace(strings.SplitN(rest, "\n", 2)[0])
			break
		}
	}
	if addr == "" {
		t.Fatalf("the proxy never reported its address: %q", errb.String())
	}
	if err := Check(addr); err != nil {
		t.Errorf("Check on a running proxy: %v", err)
	}
	status, _, _ := connectVia(t, addr, "forbidden.test:443", "")
	wantStatus(t, status, 403)

	// The decision is on stdout as one JSON line.
	// (the readiness check's refused GET comes first, then ours).
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("stdout = %q, want two decision lines", out.String())
	}
	var d Decision
	if err := json.Unmarshal([]byte(lines[1]), &d); err != nil || d.Host != "forbidden.test" || d.Reason != ReasonNotAllowlist || d.Allowed {
		t.Errorf("logged decision %q -> %+v, %v", lines[1], d, err)
	}

	// The handler is installed once the address is printed, so this signal stops the proxy
	// rather than the test binary.
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-exit:
		if code != 0 {
			t.Errorf("exit code = %d after SIGTERM", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the proxy did not stop on SIGTERM")
	}
}

func TestRunProxyLetsATestImageExtendIt(t *testing.T) {
	var errb syncBuf
	var seen string
	code := RunProxy([]string{"-bogus-extra", "x"}, &syncBuf{}, &errb, func(fs *flag.FlagSet) func(*Proxy) {
		fs.StringVar(&seen, "bogus-extra", "", "")
		return nil
	})
	if code == 0 || seen != "x" { // it parsed the extra flag, then refused for lacking the real ones
		t.Errorf("code = %d, extra flag = %q", code, seen)
	}
}

func TestListenInCIDRBindsOnlyInsideTheCIDR(t *testing.T) {
	l, err := ListenInCIDR(netip.MustParsePrefix("127.0.0.0/8"), 0, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if ip := l.Addr().(*net.TCPAddr).IP; !ip.IsLoopback() || ip.IsUnspecified() {
		t.Errorf("listening on %v: must be a specific address inside the CIDR, never a wildcard", ip)
	}
	start := time.Now()
	if _, err := ListenInCIDR(netip.MustParsePrefix("203.0.113.0/24"), 0, 300*time.Millisecond); err == nil {
		t.Error("with no address inside the CIDR it must fail, not fall back to a wildcard")
	}
	if time.Since(start) < 250*time.Millisecond {
		t.Error("it should have waited for the network to appear")
	}
}

func TestCheckDistinguishesAProxyFromAnythingElse(t *testing.T) {
	if err := Check("127.0.0.1:1"); err == nil {
		t.Error("nothing listens there")
	}
	// Something that accepts and speaks another protocol is not a ready proxy.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Write([]byte("SSH-2.0-test\r\n"))
			c.Close()
		}
	}()
	if err := Check(l.Addr().String()); err == nil {
		t.Error("an SSH banner must not pass as a proxy")
	}
}
