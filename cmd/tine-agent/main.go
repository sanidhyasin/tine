// Command tine-agent runs inside every microVM. It serves tined's requests
// (running commands, handling files) over vsock.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/sanidhyasin/tine/internal/version"
)

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("tine-agent", version.String())
		return
	}

	fmt.Fprintln(os.Stderr, "tine-agent: the agent isn't built yet (docs/PLAN.md, task 1.6)")
	os.Exit(1)
}
