// Command tracequery prints the exported spans of one correlation from a
// JSON-lines span file written by telemetry.FileExporter (CTRL-TRACE-01).
//
//	tracequery -file <spans.jsonl> <correlation_id>
//
// It prints the matching spans as JSON lines in start order and reports on
// stderr how many malformed lines it skipped. Exit status is 0 when at least
// one span matches, 1 when none does, and 2 on a usage error, a malformed
// correlation ID, an unreadable file, or a file that is not regular or grants
// group or other permissions: the evidence reader is the file owner. On exit
// 2 nothing is printed on stdout, and stderr never echoes file content.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/telemetry"
)

const usage = "usage: tracequery -file <spans.jsonl> <correlation_id>"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("tracequery", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("file", "", "JSON-lines span file")
	if err := flags.Parse(args); err != nil || *path == "" || flags.NArg() != 1 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	id, err := correlation.Parse(flags.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, "tracequery: invalid correlation ID")
		return 2
	}

	f, err := os.Open(*path)
	if err != nil {
		fmt.Fprintf(stderr, "tracequery: %s: cannot open\n", *path)
		return 2
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		fmt.Fprintf(stderr, "tracequery: %s: cannot stat\n", *path)
		return 2
	}
	if !info.Mode().IsRegular() {
		fmt.Fprintf(stderr, "tracequery: %s: not a regular file\n", *path)
		return 2
	}
	if info.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(stderr, "tracequery: %s: group or other permission bits set\n", *path)
		return 2
	}

	spans, malformed, err := telemetry.Query(f, id)
	if err != nil {
		fmt.Fprintf(stderr, "tracequery: %s: read failed\n", *path)
		return 2
	}
	if malformed > 0 {
		fmt.Fprintf(stderr, "tracequery: skipped malformed lines: %d\n", malformed)
	}
	enc := json.NewEncoder(stdout)
	for _, span := range spans {
		if err := enc.Encode(span); err != nil {
			fmt.Fprintln(stderr, "tracequery: write failed")
			return 2
		}
	}
	if len(spans) == 0 {
		return 1
	}
	return 0
}
