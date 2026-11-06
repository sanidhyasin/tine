// Command tined is the tine service. It runs on the Linux host and owns every microVM.
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
		fmt.Println("tined", version.String())
		return
	}

	fmt.Fprintln(os.Stderr, "tined: the service isn't built yet (docs/PLAN.md, task 1.10)")
	os.Exit(1)
}
