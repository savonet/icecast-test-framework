// icetest measures how many listeners an Icecast-compatible server sustains.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = runCmd(os.Args[2:])
	case "report":
		err = reportCmd(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "icetest:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  icetest run [flags] <scenario-dir>   start the scenario, ramp listeners, write results/<scenario>/<time>/
  icetest report <run-dir>             re-render report.md and report.html from run.json
run -h lists the flags.`)
	os.Exit(2)
}
