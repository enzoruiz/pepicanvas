package evidence

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/latency"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
)

// Report is the reader's verdict on one run. RunID, Scenario and Recorded are
// empty when the run is incomplete or its manifest is malformed.
type Report struct {
	RunID          string
	Scenario       string
	Recorded       string
	Classification Classification
	Problems       []string
}

// inconclusiveProblems never falsify a run; they require repeating it.
var inconclusiveProblems = []string{"missing_artifact:", "version_mismatch:", "retention_expired", "recorded_inconclusive"}

// Check classifies runDir against the current pins at now. It reads only the
// run directory, never the writer's memory. Problems are reported in this
// order: run dir permissions, manifest, run id, each artifact in manifest
// order, manifest permissions, unlisted files, versions, retention, the
// recalculated result and the recorded result.
func Check(runDir string, pins Pins, now time.Time) Report {
	info, err := os.Lstat(runDir)
	if err != nil || !info.IsDir() {
		return Report{Classification: Incomplete, Problems: []string{"missing_manifest"}}
	}
	if _, err := os.Lstat(filepath.Join(runDir, MarkerName)); !errors.Is(err, os.ErrNotExist) {
		return Report{Classification: Incomplete, Problems: []string{"incomplete_marker"}}
	}
	manifestInfo, err := os.Lstat(filepath.Join(runDir, ManifestName))
	if errors.Is(err, os.ErrNotExist) {
		return Report{Classification: Incomplete, Problems: []string{"missing_manifest"}}
	}

	var report Report
	if info.Mode().Perm()&0o077 != 0 {
		report.Problems = append(report.Problems, "loose_permissions:run")
	}
	var manifest Manifest
	ok := err == nil && manifestInfo.Mode().IsRegular()
	if ok {
		manifest, ok = readManifest(filepath.Join(runDir, ManifestName))
	}
	if !ok {
		report.Problems = append(report.Problems, "manifest_malformed")
		return report.classify()
	}
	report.RunID, report.Scenario, report.Recorded = manifest.RunID, manifest.Scenario, manifest.Result
	if manifest.RunID != filepath.Base(filepath.Clean(runDir)) {
		report.Problems = append(report.Problems, "run_id_mismatch")
	}

	intact := make(map[string][]byte, len(manifest.Artifacts))
	for _, artifact := range manifest.Artifacts {
		if data, problems := checkArtifact(runDir, artifact); len(problems) > 0 {
			report.Problems = append(report.Problems, problems...)
		} else {
			intact[artifact.Name] = data
		}
	}
	if manifestInfo.Mode().Perm()&0o077 != 0 {
		report.Problems = append(report.Problems, "loose_permissions:"+ManifestName)
	}
	if hasUnlistedFile(runDir, manifest) {
		report.Problems = append(report.Problems, "unlisted_file")
	}

	mismatches := latency.Mismatches(manifest.Versions, pins)
	for _, field := range mismatches {
		report.Problems = append(report.Problems, "version_mismatch:"+field)
	}
	expiresAt, _ := time.Parse(time.RFC3339, manifest.Retention.ExpiresAt)
	if !now.Before(expiresAt) {
		report.Problems = append(report.Problems, "retention_expired")
	}
	// A run measured with other versions must be repeated, so its result is
	// only recalculated against the pins it was recorded with.
	if manifest.Scenario == latency.Scenario && len(mismatches) == 0 {
		report.Problems = append(report.Problems, recalculateLatency(manifest, pins, intact)...)
	}

	switch manifest.Result {
	case ResultFail:
		report.Problems = append(report.Problems, "recorded_fail")
	case ResultInconclusive:
		report.Problems = append(report.Problems, "recorded_inconclusive")
	}
	return report.classify()
}

func (r Report) classify() Report {
	r.Classification = Pass
	for _, problem := range r.Problems {
		if !isInconclusive(problem) {
			r.Classification = Fail
			return r
		}
		r.Classification = Inconclusive
	}
	return r
}

func isInconclusive(problem string) bool {
	for _, prefix := range inconclusiveProblems {
		if strings.HasPrefix(problem, prefix) {
			return true
		}
	}
	return false
}

// checkArtifact returns the content of an intact artifact, or its problems.
func checkArtifact(runDir string, artifact Artifact) ([]byte, []string) {
	path := filepath.Join(runDir, artifact.Name)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, []string{"missing_artifact:" + artifact.Name}
	}
	if err != nil || !info.Mode().IsRegular() {
		return nil, []string{"irregular_file:" + artifact.Name}
	}
	var problems []string
	if info.Mode().Perm()&0o077 != 0 {
		problems = append(problems, "loose_permissions:"+artifact.Name)
	}
	data, err := os.ReadFile(path)
	switch {
	case err != nil:
		problems = append(problems, "irregular_file:"+artifact.Name)
	case int64(len(data)) != artifact.Bytes:
		problems = append(problems, "size_mismatch:"+artifact.Name)
	case sha256Hex(data) != artifact.SHA256:
		problems = append(problems, "hash_mismatch:"+artifact.Name)
	}
	return data, problems
}

func hasUnlistedFile(runDir string, manifest Manifest) bool {
	entries, err := os.ReadDir(runDir)
	if err != nil {
		return true
	}
	listed := map[string]bool{ManifestName: true}
	for _, artifact := range manifest.Artifacts {
		listed[artifact.Name] = true
	}
	for _, entry := range entries {
		if !listed[entry.Name()] {
			return true
		}
	}
	return false
}

// recalculateLatency evaluates the run's latency evidence against pins and
// compares the result with the recorded one.
func recalculateLatency(manifest Manifest, pins Pins, intact map[string][]byte) []string {
	for _, artifact := range manifest.Artifacts {
		if artifact.Kind != KindLatencyEvidence {
			continue
		}
		data, ok := intact[artifact.Name]
		if !ok {
			// Its integrity problem is already reported.
			return nil
		}
		value, err := latency.Decode(data)
		if err != nil {
			return []string{"artifact_malformed:" + artifact.Name}
		}
		var problems []string
		if value.Transmission.CorrelationID != manifest.CorrelationID {
			problems = append(problems, "correlation_mismatch")
		}
		if string(latency.Evaluate(value, pins).Result) != manifest.Result {
			problems = append(problems, "result_mismatch")
		}
		return problems
	}
	return []string{"missing_artifact:" + KindLatencyEvidence}
}

// readManifest strictly decodes and validates a manifest file.
func readManifest(path string) (Manifest, bool) {
	data, err := os.ReadFile(path)
	if err != nil || checkDuplicateKeys(data) != nil {
		return Manifest{}, false
	}
	var manifest Manifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return Manifest{}, false
	}
	return manifest, manifest.valid()
}

func (m Manifest) valid() bool {
	started, okStarted := parseTime(m.StartedAt)
	completed, okCompleted := parseTime(m.CompletedAt)
	expiresAt, okExpires := parseTime(m.Retention.ExpiresAt)
	_, err := correlation.Parse(m.CorrelationID)
	if m.SchemaVersion != SchemaVersion || !runIDPattern.MatchString(m.RunID) || err != nil ||
		!identifierPattern.MatchString(m.Release) || m.Release != m.Versions.Release ||
		!identifierPattern.MatchString(m.Scenario) || !validResult(m.Result) ||
		!okStarted || !okCompleted || completed.Before(started) ||
		m.Access.Policy != AccessPolicy || !identifierPattern.MatchString(m.Access.Role) ||
		m.Retention.Days <= 0 || m.Retention.Days > MaxRetentionDays || !okExpires ||
		!expiresAt.Equal(completed.Add(time.Duration(m.Retention.Days)*24*time.Hour)) {
		return false
	}
	seen := make(map[string]bool, len(m.Artifacts))
	for _, artifact := range m.Artifacts {
		if !validArtifactName(artifact.Name) || seen[artifact.Name] || !kindPattern.MatchString(artifact.Kind) ||
			!sha256Pattern.MatchString(artifact.SHA256) || artifact.Bytes < 0 {
			return false
		}
		seen[artifact.Name] = true
	}
	return true
}

// parseTime accepts only the canonical form written by formatTime.
func parseTime(value string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, value)
	return t, err == nil && formatTime(t) == value
}

// checkDuplicateKeys rejects a repeated object key at any depth and any data
// after the single top-level value. Keys are compared case-insensitively
// because encoding/json matches field names that way.
func checkDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := walkJSON(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data")
	}
	return nil
}

func walkJSON(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	switch token {
	case json.Delim('{'):
		seen := make(map[string]bool)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key := strings.ToLower(keyToken.(string))
			if seen[key] {
				return errors.New("duplicate key")
			}
			seen[key] = true
			if err := walkJSON(decoder); err != nil {
				return err
			}
		}
	case json.Delim('['):
		for decoder.More() {
			if err := walkJSON(decoder); err != nil {
				return err
			}
		}
	default:
		return nil
	}
	_, err = decoder.Token()
	return err
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Purge removes complete runs whose retention expired at now and incomplete
// runs older than IncompleteGrace. It leaves every other entry of root,
// including runs with a malformed manifest, untouched.
func Purge(root string, now time.Time) (int, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	removed := 0
	var firstErr error
	for _, entry := range entries {
		if !entry.IsDir() || !runIDPattern.MatchString(entry.Name()) {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		if !purgeable(dir, entry.Name(), now) {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		removed++
	}
	return removed, firstErr
}

func purgeable(dir, runID string, now time.Time) bool {
	marker, markerErr := os.Lstat(filepath.Join(dir, MarkerName))
	_, manifestErr := os.Lstat(filepath.Join(dir, ManifestName))
	switch {
	case markerErr == nil:
		return now.Sub(marker.ModTime()) > IncompleteGrace
	case errors.Is(manifestErr, os.ErrNotExist):
		info, err := os.Lstat(dir)
		return err == nil && now.Sub(info.ModTime()) > IncompleteGrace
	}
	manifest, ok := readManifest(filepath.Join(dir, ManifestName))
	if !ok || manifest.RunID != runID {
		return false
	}
	expiresAt, _ := time.Parse(time.RFC3339, manifest.Retention.ExpiresAt)
	return !now.Before(expiresAt)
}
