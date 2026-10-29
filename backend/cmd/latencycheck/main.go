// Command latencycheck evaluates TE-010 remote render latency evidence against
// the current release manifest and repository pins.
//
//	latencycheck -manifest <release manifest json> -gomod <go.mod> -lock <package-lock.json> <evidence.json>...
//
// For each evidence file, in argument order, it prints three lines:
//
//	<file> <transmissionId> result=<MEASURED|INCONCLUSIVE> samples=<n> valid=<n> rtt_p95_ms=<x|-> latency_p95_ms=<x|-> reasons=<comma list|->
//	  kinds create=<n> move=<n> resize=<n> rotate=<n> delete=<n> media=<n>
//	  invalid too_few_pings=<n> negative_rtt=<n> rtt_p95_exceeded=<n> disconnected=<n> before_measurement_start=<n> correlation_mismatch=<n> duplicate_sample=<n> negative_latency=<n>
//
// Percentiles are nearest-rank p95 in milliseconds with three decimals, or "-"
// when there is no valid sample. kinds counts valid samples per kind; invalid
// counts samples per failed reason, so one sample may count under several.
//
// Exit status is 0 when every file is MEASURED, 1 when any is INCONCLUSIVE and
// 2 on a usage error, an unreadable or malformed pin file, or a structurally
// invalid evidence file. On exit 2 nothing is printed on stdout and stderr
// names files and schema paths but never echoes evidence content.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/enzoruiz/pepicanvas/backend/internal/latency"
)

const usage = "usage: latencycheck -manifest <release-manifest.json> -gomod <go.mod> -lock <package-lock.json> <evidence.json>..."

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("latencycheck", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	manifestPath := flags.String("manifest", "", "release manifest JSON")
	goModPath := flags.String("gomod", "", "backend go.mod")
	lockPath := flags.String("lock", "", "frontend package-lock.json")
	if err := flags.Parse(args); err != nil || *manifestPath == "" || *goModPath == "" || *lockPath == "" || flags.NArg() == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}

	pins, err := loadPins(*manifestPath, *goModPath, *lockPath)
	if err != nil {
		fmt.Fprintf(stderr, "latencycheck: %v\n", err)
		return 2
	}

	files := flags.Args()
	evidence := make([]latency.Evidence, len(files))
	failed := false
	for i, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(stderr, "latencycheck: read %s: %v\n", path, errors.Unwrap(err))
			failed = true
			continue
		}
		evidence[i], err = latency.Decode(data)
		if err != nil {
			fmt.Fprintf(stderr, "latencycheck: %s: %v\n", path, err)
			failed = true
		}
	}
	if failed {
		return 2
	}

	code := 0
	for i, path := range files {
		report := latency.Evaluate(evidence[i], pins)
		printReport(stdout, path, report)
		if report.Result != latency.Measured {
			code = 1
		}
	}
	return code
}

func printReport(w io.Writer, path string, report latency.Report) {
	reasons := "-"
	if len(report.Reasons) > 0 {
		reasons = strings.Join(report.Reasons, ",")
	}
	fmt.Fprintf(w, "%s %s result=%s samples=%d valid=%d rtt_p95_ms=%s latency_p95_ms=%s reasons=%s\n",
		path, report.TransmissionID, report.Result, report.Samples, report.Valid,
		formatMs(report.RTTP95Ms), formatMs(report.LatencyP95Ms), reasons)
	fmt.Fprint(w, "  kinds")
	for _, kind := range latency.Kinds {
		fmt.Fprintf(w, " %s=%d", kind, report.ValidByKind[kind])
	}
	fmt.Fprint(w, "\n  invalid")
	for _, reason := range latency.InvalidReasons {
		fmt.Fprintf(w, " %s=%d", reason, report.InvalidByReason[reason])
	}
	fmt.Fprintln(w)
}

func formatMs(value *float64) string {
	if value == nil {
		return "-"
	}
	return strconv.FormatFloat(*value, 'f', 3, 64)
}

func loadPins(manifestPath, goModPath, lockPath string) (latency.Pins, error) {
	return latency.LoadPins(manifestPath, goModPath, lockPath)
}

func lockPackages(data []byte) ([]latency.Component, error) {
	return latency.LockPackages(data)
}
