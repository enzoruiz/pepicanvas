// Command evidencecheck independently reads TE-015 run directories and
// recalculates or classifies each run against the current repository pins.
//
//	evidencecheck -manifest <release manifest json> -gomod <go.mod> -lock <package-lock.json> <run dir>...
//
// For each run directory, in argument order, it prints one line:
//
//	<runDir> run=<runId|-> scenario=<id|-> recorded=<result|-> classification=<PASS|FAIL|INCONCLUSIVE|INCOMPLETE> problems=<comma list|->
//
// Exit status is 0 when every run is PASS, 1 when any is not and 2 on a usage
// error or an unreadable or malformed pin file; on exit 2 nothing is printed on
// stdout. Output names manifest fields and artifact names, never artifact content.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/evidence"
	"github.com/enzoruiz/pepicanvas/backend/internal/latency"
)

const usage = "usage: evidencecheck -manifest <release-manifest.json> -gomod <go.mod> -lock <package-lock.json> <run-dir>..."

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("evidencecheck", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	manifestPath := flags.String("manifest", "", "release manifest JSON")
	goModPath := flags.String("gomod", "", "backend go.mod")
	lockPath := flags.String("lock", "", "frontend package-lock.json")
	if err := flags.Parse(args); err != nil || *manifestPath == "" || *goModPath == "" || *lockPath == "" || flags.NArg() == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}

	pins, err := latency.LoadPins(*manifestPath, *goModPath, *lockPath)
	if err != nil {
		fmt.Fprintf(stderr, "evidencecheck: %v\n", err)
		return 2
	}

	now := time.Now()
	code := 0
	for _, dir := range flags.Args() {
		report := evidence.Check(dir, pins, now)
		problems := "-"
		if len(report.Problems) > 0 {
			problems = strings.Join(report.Problems, ",")
		}
		fmt.Fprintf(stdout, "%s run=%s scenario=%s recorded=%s classification=%s problems=%s\n",
			dir, orDash(report.RunID), orDash(report.Scenario), orDash(report.Recorded), report.Classification, problems)
		if report.Classification != evidence.Pass {
			code = 1
		}
	}
	return code
}

func orDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
