package evidence_test

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/evidence"
)

// S6, S10: a durable write that fails leaves the run visibly incomplete; it can
// never be completed afterwards.
func TestFailedAddKeepsTheRunIncomplete(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	w, err := evidence.Begin(t.TempDir(), testRun(otherScenario))
	if err != nil {
		t.Fatal(err)
	}
	chmod(t, w.Dir(), 0o500)
	if err := w.Add("spans.jsonl", "trace-spans", []byte("x\n")); err == nil {
		t.Fatal("Add() on a read-only run dir succeeded")
	}
	chmod(t, w.Dir(), 0o700)
	if err := w.Add("other.jsonl", "trace-spans", []byte("y\n")); !errors.Is(err, evidence.ErrClosed) {
		t.Fatalf("Add() after a failed write error = %v, want ErrClosed", err)
	}
	if err := w.Complete(evidence.ResultPass, completed); !errors.Is(err, evidence.ErrClosed) {
		t.Fatalf("Complete() after a failed write error = %v, want ErrClosed", err)
	}
	assertReport(t, evidence.Check(w.Dir(), testPins(), now), evidence.Incomplete, "incomplete_marker")
}

// S6: a run with no artifact holds no evidence, so it is never complete.
func TestCompleteRefusesARunWithoutArtifacts(t *testing.T) {
	w, err := evidence.Begin(t.TempDir(), testRun(otherScenario))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Complete(evidence.ResultPass, completed); !errors.Is(err, evidence.ErrInvalidRun) {
		t.Fatalf("Complete() without artifacts error = %v, want ErrInvalidRun", err)
	}
	assertReport(t, evidence.Check(w.Dir(), testPins(), now), evidence.Incomplete, "incomplete_marker")
	if err := w.Add("data.txt", "log", []byte("ok\n")); err != nil {
		t.Fatalf("Add() after the refusal error = %v", err)
	}
	if err := w.Complete(evidence.ResultPass, completed); err != nil {
		t.Fatalf("Complete() with an artifact error = %v", err)
	}
	assertReport(t, evidence.Check(w.Dir(), testPins(), now), evidence.Pass)
}

var artifactList = regexp.MustCompile(`(?s)"artifacts": \[.*?\n  \]`)

// S6: a manifest that lists no artifact, written by hand, is never PASS.
func TestCheckRunWithoutArtifactsIsInconclusive(t *testing.T) {
	dir := writeRun(t, t.TempDir(), testRun(otherScenario), evidence.ResultPass,
		artifact{"data.txt", "log", []byte("ok\n")})
	rewriteManifest(t, dir, func(s string) string { return artifactList.ReplaceAllString(s, `"artifacts": []`) })
	if err := os.Remove(filepath.Join(dir, "data.txt")); err != nil {
		t.Fatal(err)
	}
	assertReport(t, evidence.Check(dir, testPins(), now), evidence.Inconclusive, "no_artifacts")
}

// A2: a run that claims to complete after the check cannot be evaluated, and
// would otherwise push its retention arbitrarily far.
func TestCheckRejectsARunCompletedInTheFuture(t *testing.T) {
	dir := smokeRun(t, evidence.ResultPass)
	assertReport(t, evidence.Check(dir, testPins(), completed), evidence.Pass)
	assertReport(t, evidence.Check(dir, testPins(), completed.Add(-time.Second)), evidence.Fail, "completed_in_future")
}
