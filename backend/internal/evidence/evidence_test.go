package evidence_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/evidence"
	"github.com/enzoruiz/pepicanvas/backend/internal/latency"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
	"github.com/enzoruiz/pepicanvas/backend/internal/secret"
)

const (
	testCorrelation correlation.ID = "0123456789abcdef0123456789abcdef"
	otherScenario                  = "release-smoke"
	// sentinel marks content that must never appear in a report.
	sentinel = "SENTINEL-CONTENT-9c1d"
)

var (
	now       = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	started   = now.Add(-time.Hour)
	completed = now.Add(-30 * time.Minute)
)

func testPins() latency.Pins {
	return latency.Pins{
		ReleaseManifestSHA256: strings.Repeat("a", 64),
		Release:               "0.1.0",
		Go:                    "1.27.2",
		Node:                  "24.16.0",
		PackageManager:        latency.Component{Name: "npm", Version: "11.13.0"},
		GoModules:             []latency.Component{{Name: "github.com/jackc/pgx/v5", Version: "v5.11.0"}},
		FrontendDependencies:  []latency.Component{{Name: "react", Version: "19.3.0"}},
		PostgreSQL:            "17.4",
		S3CompatibleStorage:   "1.0.0-contract",
		FFmpeg:                "7.1.1",
		FFprobe:               "7.1.1",
		Clients: []latency.Client{
			{Family: "chrome", Slot: "current", Version: "155.0.8059.79"},
			{Family: "obs", Slot: "current", Version: "32.2.2"},
		},
	}
}

// latencyEvidence follows the cmd/latencycheck fixture: n valid samples over
// every kind with RTT 10 ms and latency i+1 ms. It is MEASURED from 100 samples.
func latencyEvidence(t *testing.T, n int, id correlation.ID) []byte {
	t.Helper()
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
			CommandCorrelationID: string(id),
			RenderCorrelationID:  string(id),
			AcceptedAtNs:         5_000_000_000,
			RenderedAtMs:         4000 + float64(i+1),
			Pings:                pings,
		})
	}
	data, err := json.Marshal(latency.Evidence{
		SchemaVersion: 1,
		Transmission: latency.Transmission{
			ID:                 "chrome-current",
			ScenarioID:         latency.Scenario,
			CorrelationID:      string(id),
			StartedAt:          "2026-10-28T11:00:00Z",
			CompletedAt:        "2026-10-28T11:30:00Z",
			MeasurementStartNs: 5_000_000_000,
			Client:             latency.Client{Family: "chrome", Slot: "current", Version: "155.0.8059.79"},
		},
		Versions: testPins(),
		Samples:  samples,
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func testRun(scenario string) evidence.Run {
	return evidence.Run{
		Scenario:      scenario,
		StartedAt:     started,
		CorrelationID: testCorrelation,
		Versions:      testPins(),
		Role:          "release-verifier",
		RetentionDays: 30,
	}
}

type artifact struct {
	name, kind string
	data       []byte
}

// writeRun completes one run under root and returns its directory. A run
// needs an artifact, so one is added when none is given.
func writeRun(t *testing.T, root string, run evidence.Run, result string, artifacts ...artifact) string {
	t.Helper()
	if len(artifacts) == 0 {
		artifacts = []artifact{{"data.txt", "log", []byte("ok\n")}}
	}
	w, err := evidence.Begin(root, run)
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	for _, a := range artifacts {
		if err := w.Add(a.name, a.kind, a.data); err != nil {
			t.Fatalf("Add(%s) error = %v", a.name, err)
		}
	}
	if err := w.Complete(result, completed); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	return w.Dir()
}

func smokeRun(t *testing.T, result string) string {
	return writeRun(t, t.TempDir(), testRun(otherScenario), result,
		artifact{"data.txt", "log", []byte("smoke passed\n")},
		artifact{"summary.json", "summary", []byte(`{"checks":3}`)})
}

func latencyRun(t *testing.T, result string, data []byte) string {
	return writeRun(t, t.TempDir(), testRun(latency.Scenario), result,
		artifact{"latency.json", evidence.KindLatencyEvidence, data})
}

func assertReport(t *testing.T, got evidence.Report, class evidence.Classification, problems ...string) {
	t.Helper()
	if got.Classification != class || !reflect.DeepEqual(got.Problems, problems) {
		t.Fatalf("Check() = %s %q, want %s %q", got.Classification, got.Problems, class, problems)
	}
	for _, problem := range got.Problems {
		if strings.Contains(problem, sentinel) {
			t.Fatalf("problem echoes content: %q", problem)
		}
	}
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestWriterPersistsADurableManifest(t *testing.T) {
	root := filepath.Join(t.TempDir(), "evidence")
	data := []byte("smoke passed\n")
	dir := writeRun(t, root, testRun(otherScenario), evidence.ResultPass, artifact{"data.txt", "log", data})

	runID := filepath.Base(dir)
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(runID) || filepath.Dir(dir) != root {
		t.Fatalf("run dir = %s, want <root>/<32 hex>", dir)
	}
	if mode(t, root) != 0o700 || mode(t, dir) != 0o700 {
		t.Fatalf("modes root=%v dir=%v, want 0700", mode(t, root), mode(t, dir))
	}
	for _, name := range []string{"data.txt", "manifest.json"} {
		if got := mode(t, filepath.Join(dir, name)); got != 0o600 {
			t.Fatalf("%s mode = %v, want 0600", name, got)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "INCOMPLETE")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("INCOMPLETE marker after Complete: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("run dir has %d entries, want data.txt and manifest.json", len(entries))
	}

	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	want := map[string]any{
		"schemaVersion": 1.0,
		"runId":         runID,
		"release":       "0.1.0",
		"scenario":      otherScenario,
		"startedAt":     "2026-10-28T11:00:00Z",
		"completedAt":   "2026-10-28T11:30:00Z",
		"result":        "PASS",
		"correlationId": string(testCorrelation),
		"artifacts":     []any{map[string]any{"name": "data.txt", "kind": "log", "sha256": hex.EncodeToString(sum[:]), "bytes": 13.0}},
		"access":        map[string]any{"policy": "owner-only", "role": "release-verifier"},
		"retention":     map[string]any{"days": 30.0, "expiresAt": "2026-11-27T11:30:00Z"},
	}
	versions := manifest["versions"]
	delete(manifest, "versions")
	if !reflect.DeepEqual(manifest, want) {
		t.Fatalf("manifest =\n%v\nwant\n%v", manifest, want)
	}
	var gotVersions latency.Versions
	encoded, _ := json.Marshal(versions)
	if err := json.Unmarshal(encoded, &gotVersions); err != nil || !reflect.DeepEqual(gotVersions, testPins()) {
		t.Fatalf("versions = %+v, want the pins", gotVersions)
	}

	report := evidence.Check(dir, testPins(), now)
	if report.RunID != runID || report.Scenario != otherScenario || report.Recorded != "PASS" {
		t.Fatalf("report = %+v", report)
	}
	assertReport(t, report, evidence.Pass)
}

func TestCheckClassifiesRecordedResults(t *testing.T) {
	tests := []struct {
		result   string
		class    evidence.Classification
		problems []string
	}{
		{evidence.ResultPass, evidence.Pass, nil},
		{evidence.ResultMeasured, evidence.Pass, nil},
		{evidence.ResultInconclusive, evidence.Inconclusive, []string{"recorded_inconclusive"}},
		{evidence.ResultFail, evidence.Fail, []string{"recorded_fail"}},
	}
	for _, tt := range tests {
		t.Run(tt.result, func(t *testing.T) {
			report := evidence.Check(smokeRun(t, tt.result), testPins(), now)
			if report.Recorded != tt.result {
				t.Fatalf("Recorded = %q, want %q", report.Recorded, tt.result)
			}
			assertReport(t, report, tt.class, tt.problems...)
		})
	}
}

func TestCheckRecalculatesTheLatencyResult(t *testing.T) {
	other := correlation.ID(strings.Repeat("b", 32))
	tests := []struct {
		name     string
		result   string
		data     func(*testing.T) []byte
		class    evidence.Classification
		problems []string
	}{
		{name: "measured recalculated as measured", result: evidence.ResultMeasured,
			data: func(t *testing.T) []byte { return latencyEvidence(t, 102, testCorrelation) }, class: evidence.Pass},
		{name: "recorded measured but recalculated inconclusive", result: evidence.ResultMeasured,
			data: func(t *testing.T) []byte { return latencyEvidence(t, 6, testCorrelation) }, class: evidence.Fail,
			problems: []string{"result_mismatch"}},
		{name: "recorded inconclusive but recalculated measured", result: evidence.ResultInconclusive,
			data: func(t *testing.T) []byte { return latencyEvidence(t, 102, testCorrelation) }, class: evidence.Fail,
			problems: []string{"result_mismatch", "recorded_inconclusive"}},
		{name: "recorded pass is not a latency result", result: evidence.ResultPass,
			data: func(t *testing.T) []byte { return latencyEvidence(t, 102, testCorrelation) }, class: evidence.Fail,
			problems: []string{"result_mismatch"}},
		{name: "inconclusive recalculated as inconclusive", result: evidence.ResultInconclusive,
			data: func(t *testing.T) []byte { return latencyEvidence(t, 6, testCorrelation) }, class: evidence.Inconclusive,
			problems: []string{"recorded_inconclusive"}},
		{name: "evidence of another correlation", result: evidence.ResultMeasured,
			data: func(t *testing.T) []byte { return latencyEvidence(t, 102, other) }, class: evidence.Fail,
			problems: []string{"correlation_mismatch"}},
		{name: "malformed evidence", result: evidence.ResultMeasured,
			data: func(*testing.T) []byte { return []byte(`{"` + sentinel + `":1}`) }, class: evidence.Fail,
			problems: []string{"artifact_malformed:latency.json"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := evidence.Check(latencyRun(t, tt.result, tt.data(t)), testPins(), now)
			assertReport(t, report, tt.class, tt.problems...)
		})
	}
}

func TestCheckLatencyRunWithoutEvidenceIsInconclusive(t *testing.T) {
	dir := writeRun(t, t.TempDir(), testRun(latency.Scenario), evidence.ResultMeasured,
		artifact{"notes.txt", "log", []byte("no samples\n")})

	assertReport(t, evidence.Check(dir, testPins(), now), evidence.Inconclusive, "missing_artifact:latency-evidence")
}

func TestCheckIncompleteRuns(t *testing.T) {
	t.Run("interrupted before Complete", func(t *testing.T) {
		w, err := evidence.Begin(t.TempDir(), testRun(otherScenario))
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Add("data.txt", "log", []byte("partial\n")); err != nil {
			t.Fatal(err)
		}
		if mode(t, filepath.Join(w.Dir(), "INCOMPLETE")) != 0o600 {
			t.Fatal("INCOMPLETE marker mode is not 0600")
		}
		report := evidence.Check(w.Dir(), testPins(), now)
		if report.Recorded != "" || report.RunID != "" {
			t.Fatalf("report = %+v, want no recorded result", report)
		}
		assertReport(t, report, evidence.Incomplete, "incomplete_marker")
	})
	t.Run("marker left beside a manifest", func(t *testing.T) {
		dir := smokeRun(t, evidence.ResultPass)
		if err := os.WriteFile(filepath.Join(dir, "INCOMPLETE"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		assertReport(t, evidence.Check(dir, testPins(), now), evidence.Incomplete, "incomplete_marker")
	})
	t.Run("no manifest", func(t *testing.T) {
		assertReport(t, evidence.Check(t.TempDir(), testPins(), now), evidence.Incomplete, "missing_manifest")
	})
	t.Run("missing run directory", func(t *testing.T) {
		assertReport(t, evidence.Check(filepath.Join(t.TempDir(), "absent"), testPins(), now), evidence.Incomplete, "missing_manifest")
	})
	t.Run("durable write fails", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		w, err := evidence.Begin(t.TempDir(), testRun(otherScenario))
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Add("data.txt", "log", []byte("ok\n")); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(w.Dir(), 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(w.Dir(), 0o700) })

		if err := w.Complete(evidence.ResultPass, completed); err == nil {
			t.Fatal("Complete() on a read-only run dir succeeded")
		}
		if err := os.Chmod(w.Dir(), 0o700); err != nil {
			t.Fatal(err)
		}
		assertReport(t, evidence.Check(w.Dir(), testPins(), now), evidence.Incomplete, "incomplete_marker")
		if err := w.Complete(evidence.ResultPass, completed); !errors.Is(err, evidence.ErrClosed) {
			t.Fatalf("second Complete() error = %v, want ErrClosed", err)
		}
	})
	t.Run("invalid result keeps the run incomplete", func(t *testing.T) {
		w, err := evidence.Begin(t.TempDir(), testRun(otherScenario))
		if err != nil {
			t.Fatal(err)
		}
		for _, call := range []struct {
			result string
			at     time.Time
		}{{"OK", completed}, {evidence.ResultPass, started.Add(-time.Second)}} {
			if err := w.Complete(call.result, call.at); !errors.Is(err, evidence.ErrInvalidRun) {
				t.Fatalf("Complete(%q, %v) error = %v, want ErrInvalidRun", call.result, call.at, err)
			}
		}
		assertReport(t, evidence.Check(w.Dir(), testPins(), now), evidence.Incomplete, "incomplete_marker")
	})
}

func TestWriterRejectsWritesAfterComplete(t *testing.T) {
	w, err := evidence.Begin(t.TempDir(), testRun(otherScenario))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Add("data.txt", "log", []byte("ok\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Complete(evidence.ResultPass, completed); err != nil {
		t.Fatal(err)
	}
	if err := w.Add("late.txt", "log", []byte("late")); !errors.Is(err, evidence.ErrClosed) {
		t.Fatalf("Add() after Complete error = %v, want ErrClosed", err)
	}
	if err := w.Complete(evidence.ResultFail, completed); !errors.Is(err, evidence.ErrClosed) {
		t.Fatalf("Complete() twice error = %v, want ErrClosed", err)
	}
	if _, err := os.Stat(filepath.Join(w.Dir(), "late.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("late.txt exists: %v", err)
	}
	assertReport(t, evidence.Check(w.Dir(), testPins(), now), evidence.Pass)
}

func TestAddRejectsSecretsBeforePersisting(t *testing.T) {
	token, err := secret.Generate(secret.KindSessionToken)
	if err != nil {
		t.Fatal(err)
	}
	payloads := map[string]string{
		"generated token": "session " + token.Reveal() + "\n",
		"private url":     "media https://bucket.example/object.mp4?X-Amz-Signature=" + sentinel + "\n",
		"url userinfo":    "postgres://user:" + sentinel + "@db.example/app\n",
	}
	for name, payload := range payloads {
		t.Run(name, func(t *testing.T) {
			w, err := evidence.Begin(t.TempDir(), testRun(otherScenario))
			if err != nil {
				t.Fatal(err)
			}
			if err := w.Add("leak.txt", "log", []byte(payload)); !errors.Is(err, evidence.ErrSecret) {
				t.Fatalf("Add() error = %v, want ErrSecret", err)
			}
			if err := w.Add("data.txt", "log", []byte("ok\n")); err != nil {
				t.Fatalf("Add() after a rejected secret error = %v", err)
			}
			if err := w.Complete(evidence.ResultPass, completed); err != nil {
				t.Fatal(err)
			}
			entries, _ := os.ReadDir(w.Dir())
			for _, entry := range entries {
				data, _ := os.ReadFile(filepath.Join(w.Dir(), entry.Name()))
				if (entry.Name() != "manifest.json" && entry.Name() != "data.txt") || strings.Contains(string(data), "leak.txt") || strings.Contains(string(data), sentinel) {
					t.Fatalf("rejected secret persisted in %s", entry.Name())
				}
			}
			assertReport(t, evidence.Check(w.Dir(), testPins(), now), evidence.Pass)
		})
	}
}

func TestAddRejectsUnsafeArtifacts(t *testing.T) {
	w, err := evidence.Begin(t.TempDir(), testRun(otherScenario))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Add("data.txt", "log", []byte("ok")); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ name, kind string }{
		{"", "log"}, {"../escape.txt", "log"}, {"a/b.txt", "log"}, {".hidden", "log"}, {"..", "log"},
		{"manifest.json", "log"}, {"INCOMPLETE", "log"}, {"data.txt", "log"}, {"line\nbreak", "log"},
		{"ok.txt", ""}, {"ok.txt", "Log Kind"},
	} {
		if err := w.Add(tt.name, tt.kind, []byte("x")); !errors.Is(err, evidence.ErrInvalidArtifact) {
			t.Fatalf("Add(%q, %q) error = %v, want ErrInvalidArtifact", tt.name, tt.kind, err)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(w.Dir()), "escape.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("artifact escaped the run dir")
	}
	if err := w.Complete(evidence.ResultPass, completed); err != nil {
		t.Fatal(err)
	}
	assertReport(t, evidence.Check(w.Dir(), testPins(), now), evidence.Pass)
}

func TestBeginRejectsInvalidRuns(t *testing.T) {
	tests := map[string]func(*evidence.Run){
		"malformed correlation": func(r *evidence.Run) { r.CorrelationID = "ABC" },
		"empty scenario":        func(r *evidence.Run) { r.Scenario = "" },
		"scenario with spaces":  func(r *evidence.Run) { r.Scenario = "a b" },
		"empty release":         func(r *evidence.Run) { r.Versions.Release = "" },
		"zero start":            func(r *evidence.Run) { r.StartedAt = time.Time{} },
		"empty role":            func(r *evidence.Run) { r.Role = "" },
		"no retention":          func(r *evidence.Run) { r.RetentionDays = 0 },
		"retention over a year": func(r *evidence.Run) { r.RetentionDays = evidence.MaxRetentionDays + 1 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			run := testRun(otherScenario)
			mutate(&run)
			if _, err := evidence.Begin(root, run); !errors.Is(err, evidence.ErrInvalidRun) {
				t.Fatalf("Begin() error = %v, want ErrInvalidRun", err)
			}
			if entries, _ := os.ReadDir(root); len(entries) != 0 {
				t.Fatalf("Begin() left %d entries", len(entries))
			}
		})
	}
}

func rewriteManifest(t *testing.T, dir string, edit func(string) string) {
	t.Helper()
	path := filepath.Join(dir, "manifest.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := edit(string(data))
	if edited == string(data) {
		t.Fatal("manifest edit changed nothing")
	}
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCheckDetectsTamperingAndLooseAccess(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(t *testing.T, dir string) string
		class    evidence.Classification
		problems []string
	}{
		{name: "untouched run", mutate: func(t *testing.T, dir string) string {
			return dir
		}},
		{name: "tampered artifact of the same size", mutate: func(t *testing.T, dir string) string {
			writeOver(t, filepath.Join(dir, "data.txt"), "smoke failed\n")
			return dir
		}, class: evidence.Fail, problems: []string{"hash_mismatch:data.txt"}},
		{name: "appended bytes", mutate: func(t *testing.T, dir string) string {
			writeOver(t, filepath.Join(dir, "data.txt"), "smoke passed\n"+sentinel)
			return dir
		}, class: evidence.Fail, problems: []string{"size_mismatch:data.txt"}},
		{name: "extra file", mutate: func(t *testing.T, dir string) string {
			writeOver(t, filepath.Join(dir, sentinel+".txt"), sentinel)
			return dir
		}, class: evidence.Fail, problems: []string{"unlisted_file"}},
		{name: "extra directory", mutate: func(t *testing.T, dir string) string {
			if err := os.Mkdir(filepath.Join(dir, "nested"), 0o700); err != nil {
				t.Fatal(err)
			}
			return dir
		}, class: evidence.Fail, problems: []string{"unlisted_file"}},
		{name: "missing artifact", mutate: func(t *testing.T, dir string) string {
			if err := os.Remove(filepath.Join(dir, "summary.json")); err != nil {
				t.Fatal(err)
			}
			return dir
		}, class: evidence.Inconclusive, problems: []string{"missing_artifact:summary.json"}},
		{name: "artifact replaced by a symlink", mutate: func(t *testing.T, dir string) string {
			target := filepath.Join(t.TempDir(), "elsewhere")
			writeOver(t, target, "smoke passed\n")
			if err := os.Remove(filepath.Join(dir, "data.txt")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(dir, "data.txt")); err != nil {
				t.Fatal(err)
			}
			return dir
		}, class: evidence.Fail, problems: []string{"irregular_file:data.txt"}},
		{name: "group-readable run dir", mutate: func(t *testing.T, dir string) string {
			chmod(t, dir, 0o750)
			return dir
		}, class: evidence.Fail, problems: []string{"loose_permissions:run"}},
		{name: "world-readable artifact", mutate: func(t *testing.T, dir string) string {
			chmod(t, filepath.Join(dir, "data.txt"), 0o644)
			return dir
		}, class: evidence.Fail, problems: []string{"loose_permissions:data.txt"}},
		{name: "group-writable manifest", mutate: func(t *testing.T, dir string) string {
			chmod(t, filepath.Join(dir, "manifest.json"), 0o620)
			return dir
		}, class: evidence.Fail, problems: []string{"loose_permissions:manifest.json"}},
		{name: "renamed run dir", mutate: func(t *testing.T, dir string) string {
			renamed := filepath.Join(filepath.Dir(dir), strings.Repeat("c", 32))
			if err := os.Rename(dir, renamed); err != nil {
				t.Fatal(err)
			}
			return renamed
		}, class: evidence.Fail, problems: []string{"run_id_mismatch"}},
		{name: "recorded result edited", mutate: func(t *testing.T, dir string) string {
			rewriteManifest(t, dir, func(s string) string { return strings.Replace(s, `"result": "PASS"`, `"result": "FAIL"`, 1) })
			return dir
		}, class: evidence.Fail, problems: []string{"recorded_fail"}},
	}
	malformed := map[string]func(string) string{
		"unknown field": func(s string) string { return strings.Replace(s, `{`, `{"`+sentinel+`": 1,`, 1) },
		"duplicate field": func(s string) string {
			return strings.Replace(s, `"result": "PASS"`, `"result": "PASS", "result": "PASS"`, 1)
		},
		"trailing data":   func(s string) string { return s + sentinel },
		"schema version":  func(s string) string { return strings.Replace(s, `"schemaVersion": 1`, `"schemaVersion": 2`, 1) },
		"unknown result":  func(s string) string { return strings.Replace(s, `"result": "PASS"`, `"result": "`+sentinel+`"`, 1) },
		"release differs": func(s string) string { return strings.Replace(s, `"release": "0.1.0"`, `"release": "0.2.0"`, 1) },
		"open access":     func(s string) string { return strings.Replace(s, `"owner-only"`, `"public"`, 1) },
		"retention extended": func(s string) string {
			return strings.Replace(s, `"expiresAt": "2026-11-27T11:30:00Z"`, `"expiresAt": "2027-11-27T11:30:00Z"`, 1)
		},
		"non-UTC time": func(s string) string {
			return strings.Replace(s, `"startedAt": "2026-10-28T11:00:00Z"`, `"startedAt": "2026-10-28T08:00:00-03:00"`, 1)
		},
		"unsafe artifact name": func(s string) string { return strings.Replace(s, `"name": "data.txt"`, `"name": "../data.txt"`, 1) },
		"not JSON":             func(string) string { return sentinel },
	}
	for name, edit := range malformed {
		tests = append(tests, struct {
			name     string
			mutate   func(t *testing.T, dir string) string
			class    evidence.Classification
			problems []string
		}{name: "malformed manifest: " + name, mutate: func(t *testing.T, dir string) string {
			rewriteManifest(t, dir, edit)
			return dir
		}, class: evidence.Fail, problems: []string{"manifest_malformed"}})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.class == "" {
				tt.class = evidence.Pass
			}
			dir := tt.mutate(t, smokeRun(t, evidence.ResultPass))
			assertReport(t, evidence.Check(dir, testPins(), now), tt.class, tt.problems...)
		})
	}
}

func writeOver(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func chmod(t *testing.T, path string, perm os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
}

func TestCheckVersionMismatchIsInconclusive(t *testing.T) {
	drifted := testPins()
	drifted.Go = "1.27.3"
	drifted.Clients = drifted.Clients[:1]

	assertReport(t, evidence.Check(smokeRun(t, evidence.ResultPass), drifted, now), evidence.Inconclusive,
		"version_mismatch:go", "version_mismatch:clients")
	// A measured latency run checked against drifted pins must be repeated,
	// not reported as a falsified result.
	measured := latencyRun(t, evidence.ResultMeasured, latencyEvidence(t, 102, testCorrelation))
	assertReport(t, evidence.Check(measured, drifted, now), evidence.Inconclusive,
		"version_mismatch:go", "version_mismatch:clients")
}

func TestCheckExpiredRetentionIsInconclusive(t *testing.T) {
	dir := smokeRun(t, evidence.ResultPass)
	expiresAt := completed.Add(30 * 24 * time.Hour)

	assertReport(t, evidence.Check(dir, testPins(), expiresAt.Add(-time.Second)), evidence.Pass)
	assertReport(t, evidence.Check(dir, testPins(), expiresAt), evidence.Inconclusive, "retention_expired")
}

func TestCheckReportsEveryProblem(t *testing.T) {
	dir := smokeRun(t, evidence.ResultInconclusive)
	chmod(t, dir, 0o755)
	writeOver(t, filepath.Join(dir, "data.txt"), "smoke failed\n")
	writeOver(t, filepath.Join(dir, "extra"), "")
	drifted := testPins()
	drifted.FFmpeg = "7.1.2"

	assertReport(t, evidence.Check(dir, drifted, completed.Add(31*24*time.Hour)), evidence.Fail,
		"loose_permissions:run", "hash_mismatch:data.txt", "unlisted_file",
		"version_mismatch:ffmpeg", "retention_expired", "recorded_inconclusive")
}

// purgeAt is two days after the runs completed.
var purgeAt = completed.Add(48 * time.Hour)

func setAge(t *testing.T, path string, age time.Duration) {
	t.Helper()
	at := purgeAt.Add(-age)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func TestPurgeRemovesOnlyExpiredAndStaleRuns(t *testing.T) {
	root := t.TempDir()
	expired := testRun(otherScenario)
	expired.RetentionDays = 1
	expiredDir := writeRun(t, root, expired, evidence.ResultPass)
	keptDir := writeRun(t, root, testRun(otherScenario), evidence.ResultPass)

	stale, err := evidence.Begin(root, testRun(otherScenario))
	if err != nil {
		t.Fatal(err)
	}
	setAge(t, filepath.Join(stale.Dir(), "INCOMPLETE"), 25*time.Hour)
	fresh, err := evidence.Begin(root, testRun(otherScenario))
	if err != nil {
		t.Fatal(err)
	}
	setAge(t, filepath.Join(fresh.Dir(), "INCOMPLETE"), 23*time.Hour)

	malformedDir := writeRun(t, root, expired, evidence.ResultPass)
	rewriteManifest(t, malformedDir, func(s string) string { return s + "garbage" })
	unrelatedDir := filepath.Join(root, "notes")
	if err := os.Mkdir(unrelatedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	setAge(t, unrelatedDir, 30*24*time.Hour)
	unrelatedFile := filepath.Join(root, strings.Repeat("d", 32))
	writeOver(t, unrelatedFile, "")
	setAge(t, unrelatedFile, 30*24*time.Hour)

	removed, err := evidence.Purge(root, purgeAt)
	if err != nil || removed != 2 {
		t.Fatalf("Purge() = %d, %v, want 2 removed", removed, err)
	}
	for _, gone := range []string{expiredDir, stale.Dir()} {
		if _, err := os.Lstat(gone); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s survived Purge: %v", gone, err)
		}
	}
	for _, kept := range []string{keptDir, fresh.Dir(), malformedDir, unrelatedDir, unrelatedFile} {
		if _, err := os.Lstat(kept); err != nil {
			t.Fatalf("Purge removed %s: %v", kept, err)
		}
	}
	assertReport(t, evidence.Check(keptDir, testPins(), now), evidence.Pass)
}

func TestPurgeMissingRoot(t *testing.T) {
	if removed, err := evidence.Purge(filepath.Join(t.TempDir(), "absent"), now); err != nil || removed != 0 {
		t.Fatalf("Purge(absent) = %d, %v, want 0 and no error", removed, err)
	}
}
