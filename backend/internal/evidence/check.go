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
	"reflect"
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
var inconclusiveProblems = []string{"no_artifacts", "missing_artifact:", "version_mismatch:", "retention_expired", "recorded_inconclusive"}

// Check classifies runDir against the current pins at now. It reads only the
// run directory, never the writer's memory. Problems are reported in this
// order: run dir permissions, manifest, run id, each artifact in manifest
// order, manifest permissions, unlisted files, versions, retention, the
// recalculated result and the recorded result.
func Check(runDir string, pins Pins, now time.Time) Report {
	// The run id is the directory's own name, so "." and relative forms are
	// resolved first; this also drops a trailing slash, through which Lstat
	// would follow a symlinked run directory.
	runDir, err := filepath.Abs(runDir)
	if err != nil {
		return Report{Classification: Incomplete, Problems: []string{"missing_manifest"}}
	}
	info, err := os.Lstat(runDir)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		// A symlink is not trusted evidence, like a symlinked artifact.
		return Report{Classification: Fail, Problems: []string{"irregular_file:run"}}
	}
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
	if manifest.RunID != filepath.Base(runDir) {
		report.Problems = append(report.Problems, "run_id_mismatch")
	}

	if len(manifest.Artifacts) == 0 {
		report.Problems = append(report.Problems, "no_artifacts")
	}
	// Only the latency evidence is kept in memory, for its recalculation.
	intact := make(map[string][]byte, 1)
	for _, artifact := range manifest.Artifacts {
		if data, problems := checkArtifact(runDir, artifact); len(problems) > 0 {
			report.Problems = append(report.Problems, problems...)
		} else if artifact.Kind == KindLatencyEvidence {
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
	// A run cannot have completed after it is checked; trusting such an
	// instant would also push its retention arbitrarily far.
	if completedAt, _ := time.Parse(time.RFC3339, manifest.CompletedAt); completedAt.After(now) {
		report.Problems = append(report.Problems, "completed_in_future")
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

// checkArtifact streams an artifact through SHA-256 and returns its problems.
// It returns the content of a latency evidence artifact only, whose size is
// bounded by MaxLatencyEvidenceBytes, so a run's other artifacts are never
// held in memory.
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
	keep := artifact.Kind == KindLatencyEvidence
	if keep && artifact.Bytes > MaxLatencyEvidenceBytes {
		problems = append(problems, "artifact_too_large:"+artifact.Name)
		keep = false
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, append(problems, "irregular_file:"+artifact.Name)
	}
	defer file.Close()
	hash := sha256.New()
	var content bytes.Buffer
	var sink io.Writer = hash
	if keep {
		sink = io.MultiWriter(hash, &content)
	}
	// One byte past the recorded size is enough to detect a size mismatch.
	n, err := io.Copy(sink, io.LimitReader(file, artifact.Bytes+1))
	switch {
	case err != nil:
		problems = append(problems, "irregular_file:"+artifact.Name)
	case n != artifact.Bytes:
		problems = append(problems, "size_mismatch:"+artifact.Name)
	case hex.EncodeToString(hash.Sum(nil)) != artifact.SHA256:
		problems = append(problems, "hash_mismatch:"+artifact.Name)
	}
	return content.Bytes(), problems
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
// compares the result with the recorded one. A valid manifest lists at most
// one latency evidence artifact (a second one is manifest_malformed rather
// than recalculated too, the stricter choice), so the first is the only one.
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
	if err != nil || checkKeys(data) != nil {
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
	latencyEvidence := 0
	for _, artifact := range m.Artifacts {
		if artifact.Kind == KindLatencyEvidence {
			latencyEvidence++
		}
		if !validArtifactName(artifact.Name) || seen[artifact.Name] || !kindPattern.MatchString(artifact.Kind) ||
			!sha256Pattern.MatchString(artifact.SHA256) || artifact.Bytes < 0 || latencyEvidence > 1 {
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

// checkKeys rejects, at any depth, an object key that is not exactly the JSON
// name of a Manifest field or that repeats, and any data after the single
// top-level value. encoding/json would match a key such as "Result"
// case-insensitively, but the manifest has one spelling.
func checkKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := walkJSON(decoder, reflect.TypeOf(Manifest{})); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data")
	}
	return nil
}

// walkJSON checks the next JSON value against t, the type it decodes into.
func walkJSON(decoder *json.Decoder, t reflect.Type) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	switch token {
	case json.Delim('{'):
		fields := jsonFields(t)
		seen := make(map[string]bool)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key := keyToken.(string)
			field, ok := fields[key]
			if !ok {
				return errors.New("unknown key")
			}
			if seen[key] {
				return errors.New("duplicate key")
			}
			seen[key] = true
			if err := walkJSON(decoder, field); err != nil {
				return err
			}
		}
	case json.Delim('['):
		var elem reflect.Type
		if t != nil && t.Kind() == reflect.Slice {
			elem = t.Elem()
		}
		for decoder.More() {
			if err := walkJSON(decoder, elem); err != nil {
				return err
			}
		}
	default:
		return nil
	}
	_, err = decoder.Token()
	return err
}

// jsonFields maps the exact JSON names of struct type t to their field types;
// it is empty for any other type, so an object there has no valid key.
func jsonFields(t reflect.Type) map[string]reflect.Type {
	fields := make(map[string]reflect.Type)
	if t == nil || t.Kind() != reflect.Struct {
		return fields
	}
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if !field.IsExported() || name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		fields[name] = field.Type
	}
	return fields
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
