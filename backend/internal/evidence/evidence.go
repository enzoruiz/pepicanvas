// Package evidence persists one durable manifest per verification run and reads
// it back independently (CTRL-EVID-01, CTRL-VERS-01).
//
// A run lives in <root>/<runId>/ (mode 0700). Begin writes an INCOMPLETE
// marker first; Add writes each artifact atomically with mode 0600 after
// verifying that redaction would not change it; Complete writes manifest.json
// atomically and only then removes the marker. Any failure before Complete
// leaves the marker, so a partial run never looks valid.
//
// Check reads a run directory without the writer's memory and classifies it:
// INCOMPLETE (marker present or no manifest), FAIL (malformed manifest, hash or
// size mismatch, irregular or unlisted file, group/other permission bits, a
// recalculated result different from the recorded one, recorded FAIL),
// INCONCLUSIVE (missing artifact, version mismatch with the current pins,
// expired retention, recorded INCONCLUSIVE) or PASS. Problems name manifest
// fields and artifact names only and never echo artifact content.
package evidence

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/latency"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
	"github.com/enzoruiz/pepicanvas/backend/internal/secret"
)

const (
	// SchemaVersion is the only manifest version this package writes and reads.
	SchemaVersion = 1
	// ManifestName and MarkerName are reserved file names inside a run dir.
	ManifestName = "manifest.json"
	MarkerName   = "INCOMPLETE"
	// KindLatencyEvidence is the artifact kind recalculated for latency.Scenario.
	// A run holds at most one, of at most MaxLatencyEvidenceBytes, because the
	// reader holds it in memory to recalculate it.
	KindLatencyEvidence     = "latency-evidence"
	MaxLatencyEvidenceBytes = 8 << 20
	// AccessPolicy is the only access policy: the run belongs to its file owner.
	AccessPolicy = "owner-only"
	// MaxRetentionDays bounds retention; evidence is never kept indefinitely.
	MaxRetentionDays = 365
	// IncompleteGrace is how long Purge keeps an incomplete run.
	IncompleteGrace = 24 * time.Hour
)

// Recorded results.
const (
	ResultPass         = "PASS"
	ResultFail         = "FAIL"
	ResultInconclusive = "INCONCLUSIVE"
	ResultMeasured     = "MEASURED"
)

// Classification is the reader's verdict on one run.
type Classification string

const (
	Pass         Classification = "PASS"
	Fail         Classification = "FAIL"
	Inconclusive Classification = "INCONCLUSIVE"
	Incomplete   Classification = "INCOMPLETE"
)

// Versions are the exact versions a run was measured with.
type Versions = latency.Versions

// Pins are the current versions a run must match.
type Pins = latency.Pins

var (
	ErrInvalidRun      = errors.New("evidence: invalid run")
	ErrInvalidArtifact = errors.New("evidence: invalid artifact")
	ErrSecret          = errors.New("evidence: content would be redacted")
	ErrClosed          = errors.New("evidence: run already completed")
)

var (
	runIDPattern        = regexp.MustCompile(`^[0-9a-f]{32}$`)
	identifierPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	artifactNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	kindPattern         = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
	sha256Pattern       = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Run describes a run when it begins. The release is Versions.Release.
type Run struct {
	Scenario      string
	StartedAt     time.Time
	CorrelationID correlation.ID
	Versions      Versions
	Role          string
	RetentionDays int
}

// Manifest is manifest.json, schema version 1.
type Manifest struct {
	SchemaVersion int        `json:"schemaVersion"`
	RunID         string     `json:"runId"`
	Release       string     `json:"release"`
	Scenario      string     `json:"scenario"`
	StartedAt     string     `json:"startedAt"`
	CompletedAt   string     `json:"completedAt"`
	Result        string     `json:"result"`
	CorrelationID string     `json:"correlationId"`
	Versions      Versions   `json:"versions"`
	Artifacts     []Artifact `json:"artifacts"`
	Access        Access     `json:"access"`
	Retention     Retention  `json:"retention"`
}

// Artifact is one file of a run.
type Artifact struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// Access names who may read the run.
type Access struct {
	Policy string `json:"policy"`
	Role   string `json:"role"`
}

// Retention bounds how long the run is kept.
type Retention struct {
	Days      int    `json:"days"`
	ExpiresAt string `json:"expiresAt"`
}

func (r Run) valid() bool {
	_, err := correlation.Parse(string(r.CorrelationID))
	return err == nil && identifierPattern.MatchString(r.Scenario) &&
		identifierPattern.MatchString(r.Versions.Release) && identifierPattern.MatchString(r.Role) &&
		!r.StartedAt.IsZero() && r.RetentionDays > 0 && r.RetentionDays <= MaxRetentionDays
}

func validResult(result string) bool {
	switch result {
	case ResultPass, ResultFail, ResultInconclusive, ResultMeasured:
		return true
	default:
		return false
	}
}

func validArtifactName(name string) bool {
	return artifactNamePattern.MatchString(name) && name != ManifestName && name != MarkerName
}

// formatTime renders the canonical manifest instant: RFC 3339, UTC, seconds.
func formatTime(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format(time.RFC3339)
}

// Writer records one run. It is safe for concurrent use.
type Writer struct {
	mu        sync.Mutex
	dir       string
	runID     string
	run       Run
	artifacts []Artifact
	closed    bool
}

// Begin creates <root>/<runId>/ with an INCOMPLETE marker. root is created
// with mode 0700 when missing.
func Begin(root string, run Run) (*Writer, error) {
	if !run.valid() {
		return nil, ErrInvalidRun
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("evidence: create root: %w", err)
	}
	id, err := correlation.New()
	if err != nil {
		return nil, fmt.Errorf("evidence: run id: %w", err)
	}
	dir := filepath.Join(root, id.String())
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, fmt.Errorf("evidence: create run dir: %w", err)
	}
	if err := writeFileAtomic(dir, MarkerName, []byte("incomplete\n")); err != nil {
		return nil, err
	}
	if err := syncDir(root); err != nil {
		return nil, err
	}
	return &Writer{dir: dir, runID: id.String(), run: run}, nil
}

// Dir returns the run directory.
func (w *Writer) Dir() string { return w.dir }

// RunID returns the run identifier.
func (w *Writer) RunID() string { return w.runID }

// Add writes one artifact. name must be a single safe path element, a run
// holds at most one latency evidence artifact of at most
// MaxLatencyEvidenceBytes, and data must be unchanged by secret.Scrub;
// otherwise nothing is written and the writer stays usable. A failed durable
// write closes the writer, so the run keeps its INCOMPLETE marker and can
// never be completed.
func (w *Writer) Add(name, kind string, data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	if !validArtifactName(name) || !kindPattern.MatchString(kind) ||
		(kind == KindLatencyEvidence && len(data) > MaxLatencyEvidenceBytes) {
		return ErrInvalidArtifact
	}
	for _, existing := range w.artifacts {
		if existing.Name == name || (kind == KindLatencyEvidence && existing.Kind == KindLatencyEvidence) {
			return ErrInvalidArtifact
		}
	}
	if secret.Scrub(string(data)) != string(data) {
		return ErrSecret
	}
	if err := writeFileAtomic(w.dir, name, data); err != nil {
		w.closed = true
		return err
	}
	w.artifacts = append(w.artifacts, Artifact{Name: name, Kind: kind, SHA256: sha256Hex(data), Bytes: int64(len(data))})
	return nil
}

// Complete writes manifest.json and then removes the INCOMPLETE marker. An
// invalid result, an instant before the start or after now (the reader would
// report completed_in_future), or a run without artifacts, is rejected without
// closing the writer; any later failure closes it and leaves the run visibly
// incomplete.
func (w *Writer) Complete(result string, completedAt time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	started := w.run.StartedAt.UTC().Truncate(time.Second)
	completed := completedAt.UTC().Truncate(time.Second)
	if !validResult(result) || completedAt.IsZero() || completed.Before(started) || completed.After(time.Now()) ||
		len(w.artifacts) == 0 {
		return ErrInvalidRun
	}
	w.closed = true

	manifest := Manifest{
		SchemaVersion: SchemaVersion,
		RunID:         w.runID,
		Release:       w.run.Versions.Release,
		Scenario:      w.run.Scenario,
		StartedAt:     formatTime(started),
		CompletedAt:   formatTime(completed),
		Result:        result,
		CorrelationID: w.run.CorrelationID.String(),
		Versions:      w.run.Versions,
		Artifacts:     append([]Artifact{}, w.artifacts...),
		Access:        Access{Policy: AccessPolicy, Role: w.run.Role},
		Retention: Retention{
			Days:      w.run.RetentionDays,
			ExpiresAt: formatTime(completed.Add(time.Duration(w.run.RetentionDays) * 24 * time.Hour)),
		},
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("evidence: encode manifest: %w", err)
	}
	data = append(data, '\n')
	if secret.Scrub(string(data)) != string(data) {
		return ErrSecret
	}
	if err := writeFileAtomic(w.dir, ManifestName, data); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(w.dir, MarkerName)); err != nil {
		return fmt.Errorf("evidence: remove marker: %w", err)
	}
	return syncDir(w.dir)
}

// writeFileAtomic writes dir/name through a synced temporary file and a rename,
// so a reader sees either no file or the whole file, with mode 0600.
func writeFileAtomic(dir, name string, data []byte) (err error) {
	file, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("evidence: create %s: %w", name, err)
	}
	temp := file.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(temp)
		}
	}()
	if err = file.Chmod(0o600); err == nil {
		if _, err = file.Write(data); err == nil {
			err = file.Sync()
		}
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("evidence: write %s: %w", name, err)
	}
	if err = os.Rename(temp, filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("evidence: rename %s: %w", name, err)
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	file, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("evidence: open dir: %w", err)
	}
	err = file.Sync()
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("evidence: sync dir: %w", err)
	}
	return nil
}
