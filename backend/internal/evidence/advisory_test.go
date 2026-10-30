package evidence_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/evidence"
	"github.com/enzoruiz/pepicanvas/backend/internal/latency"
)

// S4: the run id is the run directory's own name, however the path is spelled.
func TestCheckAcceptsEquivalentRunDirForms(t *testing.T) {
	dir := smokeRun(t, evidence.ResultPass)
	runID := filepath.Base(dir)

	t.Chdir(dir)
	for _, form := range []string{".", "./", dir + "/", dir + "/."} {
		report := evidence.Check(form, testPins(), now)
		if report.RunID != runID {
			t.Fatalf("Check(%q).RunID = %q, want %q", form, report.RunID, runID)
		}
		assertReport(t, report, evidence.Pass)
	}
	t.Chdir(filepath.Dir(dir))
	for _, form := range []string{runID, runID + "/", "./" + runID, "../" + filepath.Base(filepath.Dir(dir)) + "/" + runID} {
		assertReport(t, evidence.Check(form, testPins(), now), evidence.Pass)
	}
}

// S6: a symlink is not trusted evidence, so a symlinked run directory is
// FAIL with an explicit problem, whether or not the path ends in a slash.
func TestCheckFailsASymlinkedRunDir(t *testing.T) {
	dir := smokeRun(t, evidence.ResultPass)
	link := filepath.Join(t.TempDir(), filepath.Base(dir))
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	for _, form := range []string{link, link + "/"} {
		report := evidence.Check(form, testPins(), now)
		if report.RunID != "" || report.Scenario != "" || report.Recorded != "" {
			t.Fatalf("Check(%q) = %+v, want no manifest fields", form, report)
		}
		assertReport(t, report, evidence.Fail, "irregular_file:run")
	}
	assertReport(t, evidence.Check(dir, testPins(), now), evidence.Pass)
}

// S3, A3: the manifest is strict; a key that differs from the schema only in
// case is malformed, alone or beside the canonical key.
func TestCheckRejectsKeysWithAnotherCase(t *testing.T) {
	edits := map[string]func(string) string{
		"lone top-level key": func(s string) string { return strings.Replace(s, `"result":`, `"Result":`, 1) },
		"duplicate by case": func(s string) string {
			return strings.Replace(s, `"result": "PASS"`, `"result": "PASS", "Result": "PASS"`, 1)
		},
		"nested access key":   func(s string) string { return strings.Replace(s, `"policy":`, `"Policy":`, 1) },
		"nested versions key": func(s string) string { return strings.Replace(s, `"go":`, `"GO":`, 1) },
		"version client key":  func(s string) string { return strings.Replace(s, `"family":`, `"Family":`, 1) },
		"artifact key":        func(s string) string { return strings.Replace(s, `"sha256":`, `"SHA256":`, 1) },
		"retention key":       func(s string) string { return strings.Replace(s, `"expiresAt":`, `"expiresat":`, 1) },
	}
	for name, edit := range edits {
		t.Run(name, func(t *testing.T) {
			dir := smokeRun(t, evidence.ResultPass)
			rewriteManifest(t, dir, edit)
			report := evidence.Check(dir, testPins(), now)
			if report.Recorded != "" {
				t.Fatalf("Recorded = %q, want none", report.Recorded)
			}
			assertReport(t, report, evidence.Fail, "manifest_malformed")
		})
	}
}

// S6: the writer refuses a completion instant the reader would reject, and
// stays open so the run can still be completed honestly.
func TestCompleteRefusesACompletionInTheFuture(t *testing.T) {
	w, err := evidence.Begin(t.TempDir(), testRun(otherScenario))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Add("data.txt", "log", []byte("ok\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Complete(evidence.ResultPass, time.Now().Add(time.Hour)); !errors.Is(err, evidence.ErrInvalidRun) {
		t.Fatalf("Complete(future) error = %v, want ErrInvalidRun", err)
	}
	if _, err := os.Lstat(filepath.Join(w.Dir(), "manifest.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("manifest.json after a refused completion: %v", err)
	}
	assertReport(t, evidence.Check(w.Dir(), testPins(), time.Now()), evidence.Incomplete, "incomplete_marker")
	if err := w.Complete(evidence.ResultPass, time.Now()); err != nil {
		t.Fatalf("Complete(now) error = %v", err)
	}
	assertReport(t, evidence.Check(w.Dir(), testPins(), time.Now()), evidence.Pass)
}

// replaceArtifact overwrites an artifact with data and updates its manifest
// entry, as an honest writer would have recorded it.
func replaceArtifact(t *testing.T, dir, name string, old, data []byte) {
	t.Helper()
	writeOver(t, filepath.Join(dir, name), string(data))
	oldSum, newSum := sha256.Sum256(old), sha256.Sum256(data)
	rewriteManifest(t, dir, func(s string) string {
		s = strings.Replace(s, hex.EncodeToString(oldSum[:]), hex.EncodeToString(newSum[:]), 1)
		return strings.Replace(s, fmt.Sprintf(`"bytes": %d`, len(old)), fmt.Sprintf(`"bytes": %d`, len(data)), 1)
	})
}

// S4: checking a run streams its artifacts instead of holding them in memory.
func TestCheckStreamsLargeArtifacts(t *testing.T) {
	old := []byte("ok\n")
	dir := writeRun(t, t.TempDir(), testRun(otherScenario), evidence.ResultPass, artifact{"data.txt", "log", old})
	replaceArtifact(t, dir, "data.txt", old, bytes.Repeat([]byte("x"), 16<<20))

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	report := evidence.Check(dir, testPins(), now)
	runtime.ReadMemStats(&after)
	assertReport(t, report, evidence.Pass)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 4<<20 {
		t.Fatalf("Check() allocated %d bytes for a 16 MiB artifact, want at most 4 MiB", allocated)
	}
}

// S4, S10: the latency evidence recalculation reads into memory is bounded;
// the writer refuses a larger one and the reader fails it.
func TestLatencyEvidenceIsBounded(t *testing.T) {
	tooLarge := bytes.Repeat([]byte("x"), evidence.MaxLatencyEvidenceBytes+1)

	w, err := evidence.Begin(t.TempDir(), testRun(latency.Scenario))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Add("latency.json", evidence.KindLatencyEvidence, tooLarge); !errors.Is(err, evidence.ErrInvalidArtifact) {
		t.Fatalf("Add(oversized latency evidence) error = %v, want ErrInvalidArtifact", err)
	}
	if _, err := os.Lstat(filepath.Join(w.Dir(), "latency.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oversized latency evidence written: %v", err)
	}

	old := latencyEvidence(t, 102, testCorrelation)
	dir := latencyRun(t, evidence.ResultMeasured, old)
	assertReport(t, evidence.Check(dir, testPins(), now), evidence.Pass)
	replaceArtifact(t, dir, "latency.json", old, tooLarge)
	assertReport(t, evidence.Check(dir, testPins(), now), evidence.Fail, "artifact_too_large:latency.json")
}

// S4: a run holds at most one latency evidence artifact, so no second one can
// escape recalculation.
func TestOnlyOneLatencyEvidencePerRun(t *testing.T) {
	w, err := evidence.Begin(t.TempDir(), testRun(latency.Scenario))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Add("latency.json", evidence.KindLatencyEvidence, latencyEvidence(t, 102, testCorrelation)); err != nil {
		t.Fatal(err)
	}
	if err := w.Add("second.json", evidence.KindLatencyEvidence, latencyEvidence(t, 6, testCorrelation)); !errors.Is(err, evidence.ErrInvalidArtifact) {
		t.Fatalf("Add(second latency evidence) error = %v, want ErrInvalidArtifact", err)
	}
	if _, err := os.Lstat(filepath.Join(w.Dir(), "second.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("second latency evidence written: %v", err)
	}

	dir := writeRun(t, t.TempDir(), testRun(latency.Scenario), evidence.ResultMeasured,
		artifact{"latency.json", evidence.KindLatencyEvidence, latencyEvidence(t, 102, testCorrelation)},
		artifact{"second.json", "log", latencyEvidence(t, 6, testCorrelation)})
	assertReport(t, evidence.Check(dir, testPins(), now), evidence.Pass)
	rewriteManifest(t, dir, func(s string) string {
		return strings.Replace(s, `"kind": "log"`, `"kind": "`+evidence.KindLatencyEvidence+`"`, 1)
	})
	assertReport(t, evidence.Check(dir, testPins(), now), evidence.Fail, "manifest_malformed")
}

// S10: a run directory left with neither marker nor manifest is purged after
// the incomplete grace period, and kept before it.
func TestPurgeRemovesStaleEmptyRunDirs(t *testing.T) {
	root := t.TempDir()
	stale := filepath.Join(root, strings.Repeat("e", 32))
	fresh := filepath.Join(root, strings.Repeat("f", 32))
	for _, dir := range []string{stale, fresh} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	setAge(t, stale, 25*time.Hour)
	setAge(t, fresh, 23*time.Hour)

	removed, err := evidence.Purge(root, purgeAt)
	if err != nil || removed != 1 {
		t.Fatalf("Purge() = %d, %v, want 1 removed", removed, err)
	}
	if _, err := os.Lstat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale empty run dir survived Purge: %v", err)
	}
	if _, err := os.Lstat(fresh); err != nil {
		t.Fatalf("Purge removed the fresh empty run dir: %v", err)
	}
}
