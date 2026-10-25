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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
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
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return latency.Pins{}, fmt.Errorf("read %s: %w", manifestPath, errors.Unwrap(err))
	}
	pins, err := manifestPins(manifestData)
	if err != nil {
		return latency.Pins{}, fmt.Errorf("release manifest: %w", err)
	}

	goModData, err := os.ReadFile(goModPath)
	if err != nil {
		return latency.Pins{}, fmt.Errorf("read %s: %w", goModPath, errors.Unwrap(err))
	}
	if pins.GoModules, err = goModRequires(string(goModData)); err != nil {
		return latency.Pins{}, fmt.Errorf("go.mod: %w", err)
	}

	lockData, err := os.ReadFile(lockPath)
	if err != nil {
		return latency.Pins{}, fmt.Errorf("read %s: %w", lockPath, errors.Unwrap(err))
	}
	if pins.FrontendDependencies, err = lockPackages(lockData); err != nil {
		return latency.Pins{}, fmt.Errorf("package-lock: %w", err)
	}
	return pins, nil
}

type component struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// manifestPins decodes the subset of the release manifest that TE-010 pins;
// other fields are ignored because releasecheck validates the full manifest.
func manifestPins(data []byte) (latency.Pins, error) {
	var manifest struct {
		Release struct {
			Version string `json:"version"`
		} `json:"release"`
		Clients   []latency.Client `json:"clients"`
		Toolchain struct {
			Go                  component `json:"go"`
			Node                component `json:"node"`
			PackageManager      component `json:"packageManager"`
			PostgreSQL          component `json:"postgresql"`
			S3CompatibleStorage component `json:"s3CompatibleStorage"`
			FFmpeg              component `json:"ffmpeg"`
			FFprobe             component `json:"ffprobe"`
		} `json:"toolchain"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return latency.Pins{}, errors.New("invalid JSON")
	}
	toolchain := manifest.Toolchain
	required := []struct {
		field, value string
	}{
		{"toolchain.go.version", toolchain.Go.Version},
		{"toolchain.node.version", toolchain.Node.Version},
		{"toolchain.packageManager.name", toolchain.PackageManager.Name},
		{"toolchain.packageManager.version", toolchain.PackageManager.Version},
		{"toolchain.postgresql.version", toolchain.PostgreSQL.Version},
		{"toolchain.s3CompatibleStorage.version", toolchain.S3CompatibleStorage.Version},
		{"toolchain.ffmpeg.version", toolchain.FFmpeg.Version},
		{"toolchain.ffprobe.version", toolchain.FFprobe.Version},
	}
	if manifest.Release.Version == "" {
		return latency.Pins{}, errors.New("release.version must not be empty")
	}
	if len(manifest.Clients) == 0 {
		return latency.Pins{}, errors.New("clients must not be empty")
	}
	for _, item := range required {
		if item.value == "" {
			return latency.Pins{}, fmt.Errorf("%s must not be empty", item.field)
		}
	}
	for i, client := range manifest.Clients {
		if client.Family == "" || client.Slot == "" || client.Version == "" {
			return latency.Pins{}, fmt.Errorf("clients[%d] must have family, slot and version", i)
		}
	}
	sum := sha256.Sum256(data)
	return latency.Pins{
		ReleaseManifestSHA256: hex.EncodeToString(sum[:]),
		Release:               manifest.Release.Version,
		Go:                    toolchain.Go.Version,
		Node:                  toolchain.Node.Version,
		PackageManager:        latency.Component{Name: toolchain.PackageManager.Name, Version: toolchain.PackageManager.Version},
		PostgreSQL:            toolchain.PostgreSQL.Version,
		S3CompatibleStorage:   toolchain.S3CompatibleStorage.Version,
		FFmpeg:                toolchain.FFmpeg.Version,
		FFprobe:               toolchain.FFprobe.Version,
		Clients:               manifest.Clients,
	}, nil
}

// goModRequires returns every require entry, direct and indirect, in single-line
// and block form, sorted by name then version.
func goModRequires(text string) ([]latency.Component, error) {
	var modules []latency.Component
	inBlock := false
	for number, line := range strings.Split(text, "\n") {
		if comment := strings.Index(line, "//"); comment >= 0 {
			line = line[:comment]
		}
		fields := strings.Fields(line)
		switch {
		case len(fields) == 0:
			continue
		case inBlock && len(fields) == 1 && fields[0] == ")":
			inBlock = false
			continue
		case inBlock:
		case fields[0] != "require":
			continue
		case len(fields) == 2 && fields[1] == "(":
			inBlock = true
			continue
		default:
			fields = fields[1:]
		}
		if len(fields) != 2 {
			return nil, fmt.Errorf("line %d: malformed require entry", number+1)
		}
		name := fields[0]
		if unquoted, err := strconv.Unquote(name); err == nil {
			name = unquoted
		}
		modules = append(modules, latency.Component{Name: name, Version: fields[1]})
	}
	if inBlock {
		return nil, errors.New("unterminated require block")
	}
	sortComponents(modules)
	return modules, nil
}

// lockPackages returns every non-root packages entry, named by the path after
// the last node_modules/, sorted by name then version with duplicates once.
func lockPackages(data []byte) ([]latency.Component, error) {
	var lock struct {
		Packages map[string]struct {
			Version string `json:"version"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(data, &lock); err != nil {
		return nil, errors.New("invalid JSON")
	}
	if len(lock.Packages) == 0 {
		return nil, errors.New("packages must not be empty")
	}
	seen := make(map[latency.Component]bool)
	var packages []latency.Component
	for path, entry := range lock.Packages {
		if path == "" {
			continue
		}
		if entry.Version == "" {
			return nil, fmt.Errorf("%s has no version", path)
		}
		name := path
		if index := strings.LastIndex(path, "node_modules/"); index >= 0 {
			name = path[index+len("node_modules/"):]
		}
		value := latency.Component{Name: name, Version: entry.Version}
		if !seen[value] {
			seen[value] = true
			packages = append(packages, value)
		}
	}
	sortComponents(packages)
	return packages, nil
}

func sortComponents(values []latency.Component) {
	sort.Slice(values, func(i, j int) bool {
		if values[i].Name != values[j].Name {
			return values[i].Name < values[j].Name
		}
		return values[i].Version < values[j].Version
	})
}
