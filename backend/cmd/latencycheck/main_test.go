package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/enzoruiz/pepicanvas/backend/internal/latency"
)

const testManifest = `{
  "schemaVersion": "1",
  "release": {"version": "0.1.0", "releaseDate": "2026-10-17"},
  "policy": {"mobile": "blocked"},
  "clients": [
    {"family": "chrome", "slot": "current", "version": "155.0.8059.79", "releaseDate": "2026-10-13"},
    {"family": "chrome", "slot": "previous", "version": "154.0.8037.92", "releaseDate": "2026-09-29"},
    {"family": "edge", "slot": "current", "version": "155.0.4283.45", "releaseDate": "2026-10-08"},
    {"family": "edge", "slot": "previous", "version": "154.0.4258.62", "releaseDate": "2026-10-05"},
    {"family": "obs", "slot": "current", "version": "32.2.2", "releaseDate": "2026-08-14"},
    {"family": "obs", "slot": "previous", "version": "32.2.1", "releaseDate": "2026-07-24"}
  ],
  "toolchain": {
    "go": {"name": "go", "version": "1.27.2"},
    "goModules": [],
    "node": {"name": "node", "version": "24.16.0"},
    "packageManager": {"name": "npm", "version": "11.13.0"},
    "frontendDependencies": [{"name": "react", "version": "19.3.0"}],
    "postgresql": {"name": "postgresql", "version": "17.4"},
    "s3CompatibleStorage": {"name": "s3-compatible", "version": "1.0.0-contract"},
    "ffmpeg": {"name": "ffmpeg", "version": "7.1.1"},
    "ffprobe": {"name": "ffprobe", "version": "7.1.1"}
  },
  "scenarios": [],
  "evidence": []
}
`

const testGoMod = `module example.com/backend

go 1.26.0

require example.com/single v1.0.0 // a comment

require (
	github.com/zeta/mod v0.2.0
	github.com/alpha/mod v1.2.3 // indirect

	// a full-line comment
	"example.com/quoted" v0.0.1
)

replace example.com/single => ../single
`

const testLock = `{
  "name": "frontend",
  "lockfileVersion": 3,
  "packages": {
    "": {"name": "frontend", "version": "0.0.0", "dependencies": {"react": "19.3.0"}},
    "node_modules/react": {"version": "19.3.0", "resolved": "https://example.invalid/react.tgz"},
    "node_modules/@scope/pkg": {"version": "2.0.0"},
    "node_modules/a/node_modules/b": {"version": "1.0.0"},
    "node_modules/b": {"version": "1.0.0"},
    "node_modules/c/node_modules/b": {"version": "0.9.0"}
  }
}
`

func wantPins() latency.Pins {
	sum := sha256.Sum256([]byte(testManifest))
	return latency.Pins{
		ReleaseManifestSHA256: hex.EncodeToString(sum[:]),
		Release:               "0.1.0",
		Go:                    "1.27.2",
		Node:                  "24.16.0",
		PackageManager:        latency.Component{Name: "npm", Version: "11.13.0"},
		GoModules: []latency.Component{
			{Name: "example.com/quoted", Version: "v0.0.1"},
			{Name: "example.com/single", Version: "v1.0.0"},
			{Name: "github.com/alpha/mod", Version: "v1.2.3"},
			{Name: "github.com/zeta/mod", Version: "v0.2.0"},
		},
		FrontendDependencies: []latency.Component{
			{Name: "@scope/pkg", Version: "2.0.0"},
			{Name: "b", Version: "0.9.0"},
			{Name: "b", Version: "1.0.0"},
			{Name: "react", Version: "19.3.0"},
		},
		PostgreSQL:          "17.4",
		S3CompatibleStorage: "1.0.0-contract",
		FFmpeg:              "7.1.1",
		FFprobe:             "7.1.1",
		Clients: []latency.Client{
			{Family: "chrome", Slot: "current", Version: "155.0.8059.79"},
			{Family: "chrome", Slot: "previous", Version: "154.0.8037.92"},
			{Family: "edge", Slot: "current", Version: "155.0.4283.45"},
			{Family: "edge", Slot: "previous", Version: "154.0.4258.62"},
			{Family: "obs", Slot: "current", Version: "32.2.2"},
			{Family: "obs", Slot: "previous", Version: "32.2.1"},
		},
	}
}

type repo struct {
	manifest, goMod, lock string
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeRepo(t *testing.T) repo {
	t.Helper()
	dir := t.TempDir()
	return repo{
		manifest: writeFile(t, dir, "client-support-release.json", testManifest),
		goMod:    writeFile(t, dir, "go.mod", testGoMod),
		lock:     writeFile(t, dir, "package-lock.json", testLock),
	}
}

func (r repo) args(files ...string) []string {
	return append([]string{"-manifest", r.manifest, "-gomod", r.goMod, "-lock", r.lock}, files...)
}

const correlation = "0123456789abcdef0123456789abcdef"

// testEvidence builds n valid samples over every kind: RTT 10 ms, clock offset
// 1000 ms and latency i+1 ms for sample i.
func testEvidence(id string, n int) latency.Evidence {
	samples := make([]latency.Sample, 0, n)
	for i := 0; i < n; i++ {
		var pings []latency.Ping
		for p := 0; p < 5; p++ {
			sent := float64(100 + p*20)
			pings = append(pings, latency.Ping{ClientSentMs: sent, ServerAtNs: int64((sent + 5 + 1000) * 1e6), ClientReceivedMs: sent + 10})
		}
		samples = append(samples, latency.Sample{
			ID:                   fmt.Sprintf("s-%03d", i),
			Kind:                 latency.Kinds[i%len(latency.Kinds)],
			OperationID:          fmt.Sprintf("op-%03d", i),
			Sequence:             int64(i + 1),
			CommandCorrelationID: correlation,
			RenderCorrelationID:  correlation,
			AcceptedAtNs:         5_000_000_000,
			RenderedAtMs:         4000 + float64(i+1),
			Pings:                pings,
		})
	}
	return latency.Evidence{
		SchemaVersion: 1,
		Transmission: latency.Transmission{
			ID:                 id,
			ScenarioID:         "remote-render-latency",
			CorrelationID:      correlation,
			StartedAt:          "2026-10-24T00:00:00Z",
			CompletedAt:        "2026-10-24T00:05:00Z",
			MeasurementStartNs: 5_000_000_000,
			Client:             latency.Client{Family: "chrome", Slot: "current", Version: "155.0.8059.79"},
		},
		Versions: wantPins(),
		Samples:  samples,
	}
}

func writeEvidence(t *testing.T, value latency.Evidence) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return writeFile(t, t.TempDir(), value.Transmission.ID+".json", string(data))
}

const zeroInvalidLine = "  invalid too_few_pings=0 negative_rtt=0 rtt_p95_exceeded=0 disconnected=0 before_measurement_start=0 correlation_mismatch=0 duplicate_sample=0\n"

func TestLoadPins(t *testing.T) {
	r := writeRepo(t)

	got, err := loadPins(r.manifest, r.goMod, r.lock)
	if err != nil {
		t.Fatalf("loadPins() error = %v", err)
	}
	if want := wantPins(); !reflect.DeepEqual(got, want) {
		t.Fatalf("loadPins() =\n%+v\nwant\n%+v", got, want)
	}
}

func TestRunResults(t *testing.T) {
	r := writeRepo(t)
	measured := writeEvidence(t, testEvidence("tx-measured", 102))
	short := testEvidence("tx-short", 6)
	short.Samples[0].Disconnects = 1
	short.Versions.FFmpeg = "7.1.2"
	shortPath := writeEvidence(t, short)
	empty := writeEvidence(t, testEvidence("tx-empty", 0))

	measuredOut := measured + " tx-measured result=MEASURED samples=102 valid=102 rtt_p95_ms=10.000 latency_p95_ms=97.000 reasons=-\n" +
		"  kinds create=17 move=17 resize=17 rotate=17 delete=17 media=17\n" + zeroInvalidLine
	shortOut := shortPath + " tx-short result=INCONCLUSIVE samples=6 valid=5 rtt_p95_ms=10.000 latency_p95_ms=6.000" +
		" reasons=version_mismatch:ffmpeg,insufficient_samples,missing_kind:create\n" +
		"  kinds create=0 move=1 resize=1 rotate=1 delete=1 media=1\n" +
		"  invalid too_few_pings=0 negative_rtt=0 rtt_p95_exceeded=0 disconnected=1 before_measurement_start=0 correlation_mismatch=0 duplicate_sample=0\n"
	emptyOut := empty + " tx-empty result=INCONCLUSIVE samples=0 valid=0 rtt_p95_ms=- latency_p95_ms=-" +
		" reasons=insufficient_samples,missing_kind:create,missing_kind:move,missing_kind:resize,missing_kind:rotate,missing_kind:delete,missing_kind:media\n" +
		"  kinds create=0 move=0 resize=0 rotate=0 delete=0 media=0\n" + zeroInvalidLine

	tests := []struct {
		name     string
		files    []string
		wantCode int
		wantOut  string
	}{
		{name: "measured", files: []string{measured}, wantCode: 0, wantOut: measuredOut},
		{name: "inconclusive", files: []string{shortPath}, wantCode: 1, wantOut: shortOut},
		{name: "no samples", files: []string{empty}, wantCode: 1, wantOut: emptyOut},
		{name: "any inconclusive fails the run", files: []string{measured, shortPath}, wantCode: 1, wantOut: measuredOut + shortOut},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(r.args(tt.files...), &stdout, &stderr)
			if code != tt.wantCode || stdout.String() != tt.wantOut || stderr.Len() != 0 {
				t.Fatalf("run() = %d\nstdout:\n%s\nstderr:\n%s\nwant %d\n%s", code, stdout.String(), stderr.String(), tt.wantCode, tt.wantOut)
			}
		})
	}
}

func TestRunDetectsRepositoryDrift(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*repo, string)
		want   string
	}{
		{name: "manifest bytes", mutate: func(r *repo, dir string) {
			r.manifest = writeFile(t, dir, "m.json", testManifest+"\n")
		}, want: "reasons=version_mismatch:releaseManifestSha256\n"},
		{name: "go module", mutate: func(r *repo, dir string) {
			r.goMod = writeFile(t, dir, "go.mod", strings.Replace(testGoMod, "v0.2.0", "v0.2.1", 1))
		}, want: "reasons=version_mismatch:goModules\n"},
		{name: "frontend package", mutate: func(r *repo, dir string) {
			r.lock = writeFile(t, dir, "package-lock.json", strings.Replace(testLock, `"2.0.0"`, `"2.0.1"`, 1))
		}, want: "reasons=version_mismatch:frontendDependencies\n"},
		{name: "release client", mutate: func(r *repo, dir string) {
			r.manifest = writeFile(t, dir, "m.json", strings.Replace(testManifest, "155.0.8059.79", "155.0.8059.80", 1))
		}, want: "reasons=incompatible_client,version_mismatch:releaseManifestSha256,version_mismatch:clients\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := writeRepo(t)
			tt.mutate(&r, t.TempDir())
			path := writeEvidence(t, testEvidence("tx", 102))

			var stdout, stderr bytes.Buffer
			code := run(r.args(path), &stdout, &stderr)
			first, _, _ := strings.Cut(stdout.String(), "\n")
			if code != 1 || !strings.HasSuffix(first+"\n", tt.want) || stderr.Len() != 0 {
				t.Fatalf("run() = %d stdout=%q stderr=%q, want suffix %q", code, stdout.String(), stderr.String(), tt.want)
			}
		})
	}
}

func TestRunErrors(t *testing.T) {
	const secret = "SECRET-CONTENT-7f3a"
	r := writeRepo(t)
	dir := t.TempDir()
	measured := writeEvidence(t, testEvidence("tx-measured", 102))
	unknownField := writeFile(t, dir, "unknown.json", strings.Replace(
		readFile(t, measured), `{"schemaVersion"`, `{"`+secret+`":1,"schemaVersion"`, 1))
	trailing := writeFile(t, dir, "trailing.json", readFile(t, measured)+secret)
	missing := filepath.Join(dir, "absent.json")

	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantErr  []string
	}{
		{name: "no arguments", args: nil, wantCode: 2, wantErr: []string{"usage: latencycheck -manifest"}},
		{name: "no evidence files", args: r.args(), wantCode: 2, wantErr: []string{"usage: latencycheck -manifest"}},
		{name: "missing lock flag", args: []string{"-manifest", r.manifest, "-gomod", r.goMod, measured}, wantCode: 2,
			wantErr: []string{"usage: latencycheck -manifest"}},
		{name: "unknown flag", args: append([]string{"-x"}, r.args(measured)...), wantCode: 2,
			wantErr: []string{"usage: latencycheck -manifest"}},
		{name: "unknown evidence field", args: r.args(measured, unknownField), wantCode: 2,
			wantErr: []string{"latencycheck: " + unknownField + ": unknown field at $\n"}},
		{name: "trailing evidence data", args: r.args(trailing), wantCode: 2,
			wantErr: []string{"latencycheck: " + trailing + ": evidence must contain a single JSON value\n"}},
		{name: "every structural error reported", args: r.args(unknownField, trailing), wantCode: 2,
			wantErr: []string{unknownField + ": unknown field", trailing + ": evidence must contain"}},
		{name: "unreadable evidence", args: r.args(missing), wantCode: 2, wantErr: []string{"latencycheck: read " + missing}},
		{name: "unreadable manifest", args: []string{"-manifest", missing, "-gomod", r.goMod, "-lock", r.lock, measured},
			wantCode: 2, wantErr: []string{"latencycheck: read " + missing}},
		{name: "manifest without release version", args: []string{
			"-manifest", writeFile(t, dir, "m.json", strings.Replace(testManifest, `"version": "0.1.0"`, `"version": ""`, 1)),
			"-gomod", r.goMod, "-lock", r.lock, measured,
		}, wantCode: 2, wantErr: []string{"release manifest: release.version must not be empty"}},
		{name: "manifest without clients", args: []string{
			"-manifest", writeFile(t, dir, "m2.json", `{"release":{"version":"1"},"toolchain":{}}`),
			"-gomod", r.goMod, "-lock", r.lock, measured,
		}, wantCode: 2, wantErr: []string{"release manifest: clients must not be empty"}},
		{name: "malformed go.mod require", args: []string{
			"-manifest", r.manifest, "-gomod", writeFile(t, dir, "bad.mod", "module x\nrequire (\n\texample.com/a\n)\n"),
			"-lock", r.lock, measured,
		}, wantCode: 2, wantErr: []string{"go.mod: line 3: malformed require entry"}},
		{name: "unterminated go.mod block", args: []string{
			"-manifest", r.manifest, "-gomod", writeFile(t, dir, "open.mod", "module x\nrequire (\n\texample.com/a v1.0.0\n"),
			"-lock", r.lock, measured,
		}, wantCode: 2, wantErr: []string{"go.mod: unterminated require block"}},
		{name: "lock without packages", args: []string{
			"-manifest", r.manifest, "-gomod", r.goMod, "-lock", writeFile(t, dir, "lock.json", `{"name":"x"}`), measured,
		}, wantCode: 2, wantErr: []string{"package-lock: packages must not be empty"}},
		{name: "lock package without version", args: []string{
			"-manifest", r.manifest, "-gomod", r.goMod,
			"-lock", writeFile(t, dir, "lock2.json", `{"packages":{"":{},"node_modules/a":{}}}`), measured,
		}, wantCode: 2, wantErr: []string{"package-lock: node_modules/a has no version"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tt.args, &stdout, &stderr)
			if code != tt.wantCode || stdout.Len() != 0 {
				t.Fatalf("run() = %d stdout=%q stderr=%q, want %d and no stdout", code, stdout.String(), stderr.String(), tt.wantCode)
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(stderr.String(), want) {
					t.Fatalf("stderr = %q, want it to contain %q", stderr.String(), want)
				}
			}
			if strings.Contains(stderr.String(), secret) {
				t.Fatalf("stderr echoes evidence content: %q", stderr.String())
			}
		})
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
