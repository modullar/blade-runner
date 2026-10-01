// Command probe runs INSIDE a job container (or, for "listen", a stand-in neighbour) and reports
// what the network lets it do. Each sub-command prints one line starting with "ALLOWED" or
// "BLOCKED". It is a test fixture, built with CGO_ENABLED=0 into a FROM scratch image.
package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

func report(what string, err error) {
	if err != nil {
		fmt.Printf("BLOCKED %s: %v\n", what, err)
		return
	}
	fmt.Printf("ALLOWED %s\n", what)
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: probe <what>")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "connect": // a direct TCP connection, to an IP or (resolved by Docker's DNS) a name
		c, err := net.DialTimeout("tcp", os.Args[2], 3*time.Second)
		if err == nil {
			c.Close()
		}
		report("connect to "+os.Args[2], err)
	case "via-proxy": // via-proxy PROXY HOST:PORT: CONNECT, then talk to whatever answers
		viaProxy(os.Args[2], os.Args[3])
	case "http-get": // http-get PROXY URL: a plain-HTTP request through the proxy
		c, err := net.DialTimeout("tcp", os.Args[2], 3*time.Second)
		if err != nil {
			report("reach the proxy", err)
			return
		}
		defer c.Close()
		fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n", os.Args[3])
		line, _ := bufio.NewReader(c).ReadString('\n')
		fmt.Printf("STATUS %s\n", strings.TrimSpace(line))
	case "lookup":
		addrs, err := net.LookupHost(os.Args[2])
		report(fmt.Sprintf("resolve %s -> %v", os.Args[2], addrs), err)
	case "env":
		for _, kv := range os.Environ() {
			fmt.Println(kv)
		}
	case "routes": // the kernel's IPv4 routes, so a test can see there is no default route
		b, _ := os.ReadFile("/proc/net/route")
		fmt.Print(string(b))
	case "listen": // listen PORT: a stand-in service; replies "hello from <port>" to every connection
		l, err := net.Listen("tcp", ":"+os.Args[2])
		if err != nil {
			fmt.Println("listen:", err)
			os.Exit(1)
		}
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			fmt.Fprintf(c, "hello from %s\n", os.Args[2])
			c.Close()
		}
	case "sleep":
		time.Sleep(time.Hour)
	default:
		fmt.Println("unknown probe", os.Args[1])
		os.Exit(2)
	}
}

func viaProxy(proxy, target string) {
	c, err := net.DialTimeout("tcp", proxy, 3*time.Second)
	if err != nil {
		report("reach the proxy "+proxy, err)
		return
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	r := bufio.NewReader(c)
	status, err := r.ReadString('\n')
	if err != nil {
		report("CONNECT "+target, err)
		return
	}
	status = strings.TrimSpace(status)
	if !strings.Contains(status, " 200 ") {
		fmt.Printf("BLOCKED CONNECT %s: %s\n", target, status)
		return
	}
	for { // the rest of the response headers
		line, err := r.ReadString('\n')
		if err != nil || strings.TrimSpace(line) == "" {
			break
		}
	}
	reply, err := r.ReadString('\n')
	if err != nil {
		report("read through the tunnel to "+target, err)
		return
	}
	fmt.Printf("ALLOWED CONNECT %s: %s\n", target, strings.TrimSpace(reply))
}
