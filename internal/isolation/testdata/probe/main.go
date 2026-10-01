// Command probe runs INSIDE a job container and tries things a hostile job might try. Each
// sub-command prints one line starting with "ALLOWED" or "BLOCKED" so a test can see what the
// container really permits. It is a test fixture, built with CGO_ENABLED=0 into a FROM scratch
// image.
package main

import (
	"bufio"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"syscall"
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
	case "id":
		fmt.Printf("uid=%d gid=%d\n", os.Getuid(), os.Getgid())
	case "write-rootfs":
		report("write to the root filesystem", os.WriteFile("/pwned", []byte("x"), 0o644))
	case "write-work":
		report("write to /work", os.WriteFile("/work/ok", []byte("x"), 0o644))
	case "write-tmp":
		report("write to /tmp", os.WriteFile("/tmp/ok", []byte("x"), 0o644))
	case "exec-tmp": // /tmp is noexec
		_ = os.WriteFile("/tmp/self", mustRead("/probe"), 0o755)
		report("execute a file from /tmp", exec.Command("/tmp/self", "id").Run())
	case "mount":
		_ = os.MkdirAll("/work/m", 0o755)
		report("mount a filesystem", syscall.Mount("tmpfs", "/work/m", "tmpfs", 0, ""))
	case "raw-socket":
		fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_RAW, syscall.IPPROTO_ICMP)
		if err == nil {
			syscall.Close(fd)
		}
		report("open a raw socket", err)
	case "chroot":
		report("chroot", syscall.Chroot("/work"))
	case "setuid-root":
		report("become root", syscall.Setuid(0))
	case "unshare-user":
		report("create a user namespace", syscall.Unshare(syscall.CLONE_NEWUSER))
	case "read-host-file": // a file that exists on the HOST but was never mounted
		_, err := os.ReadFile(os.Args[2])
		report("read the host file "+os.Args[2], err)
	case "connect":
		c, err := net.DialTimeout("tcp", os.Args[2], 3*time.Second)
		if err == nil {
			c.Close()
		}
		report("connect to "+os.Args[2], err)
	case "stdin-hash": // proves a secret arrived on stdin, without printing it
		b, _ := io.ReadAll(bufio.NewReader(os.Stdin))
		fmt.Printf("stdin bytes=%d sha256=%x\n", len(b), sha256.Sum256(b))
	case "env":
		for _, kv := range os.Environ() {
			fmt.Println(kv)
		}
	case "fork": // try to start N processes that stay alive; report how many started
		n, _ := strconv.Atoi(os.Args[2])
		started := 0
		for i := 0; i < n; i++ {
			c := exec.Command("/probe", "sleep", "30")
			if err := c.Start(); err != nil {
				break
			}
			started++
		}
		fmt.Printf("STARTED %d of %d processes\n", started, n)
	case "alloc": // touch N MiB of memory
		n, _ := strconv.Atoi(os.Args[2])
		buf := make([][]byte, 0, n)
		for i := 0; i < n; i++ {
			b := make([]byte, 1<<20)
			for j := range b {
				b[j] = 1
			}
			buf = append(buf, b)
		}
		fmt.Printf("allocated %d MiB\n", len(buf))
	case "sleep":
		n, _ := strconv.Atoi(os.Args[2])
		time.Sleep(time.Duration(n) * time.Second)
	case "exit":
		n, _ := strconv.Atoi(os.Args[2])
		os.Exit(n)
	case "flood": // print without end, to exercise the output cap
		line := make([]byte, 1024)
		for i := range line {
			line[i] = 'x'
		}
		for i := 0; i < 20000; i++ {
			os.Stdout.Write(line)
		}
	default:
		fmt.Println("unknown probe", os.Args[1])
		os.Exit(2)
	}
}

func mustRead(p string) []byte {
	b, err := os.ReadFile(p)
	if err != nil {
		panic(err)
	}
	return b
}
