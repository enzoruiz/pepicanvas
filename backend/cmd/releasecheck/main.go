package main

import (
	"fmt"
	"io"
	"os"

	"github.com/enzoruiz/pepicanvas/backend/internal/releasecheck"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: releasecheck <release-manifest.json>")
		return 2
	}

	report, err := releasecheck.EvaluateFile(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "releasecheck: %v\n", err)
		return 2
	}
	fmt.Fprintln(stdout, report.Classification)
	for _, problem := range report.Problems {
		fmt.Fprintf(stdout, "- %s\n", problem)
	}
	if report.Classification == releasecheck.Pass {
		return 0
	}
	return 1
}
