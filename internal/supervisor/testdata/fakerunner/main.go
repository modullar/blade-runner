// Command fakerunner is a test fixture: a stand-in for the runner image's entrypoint, built with
// CGO_ENABLED=0 into a FROM scratch image. The real image's contract (decision 0007) is that it
// reads the just-in-time runner config from STANDARD INPUT; this one checks that it arrived
// there, and reports on its own confinement, then exits. It exits 0 only if the config arrived
// on stdin, so a test can tell from the exit code that it did.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"
)

func main() {
	in, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Println("cannot read stdin:", err)
		os.Exit(2)
	}
	for _, a := range os.Args[1:] {
		if strings.Contains(a, "JIT-") {
			fmt.Println("the config is on the command line")
			os.Exit(4)
		}
	}
	for _, e := range os.Environ() {
		if strings.Contains(e, "JIT-") {
			fmt.Println("the config is in the environment")
			os.Exit(5)
		}
	}
	if !strings.HasPrefix(string(in), "JIT-") {
		fmt.Printf("stdin did not carry a runner config: %q\n", in)
		os.Exit(3)
	}
	fmt.Printf("runner config received on stdin, uid=%d, bytes=%d\n", os.Getuid(), len(in))
}
