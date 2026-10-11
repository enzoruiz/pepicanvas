// Package releasecheck validates reproducible client support release manifests.
package releasecheck

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Classification is the release-gate result of evaluating a manifest.
type Classification string

const (
	Pass         Classification = "PASS"
	Fail         Classification = "FAIL"
	Inconclusive Classification = "INCONCLUSIVE"
)

// Report describes a release classification and the reasons that block it.
type Report struct {
	Classification Classification
	Problems       []string
}

type manifest struct {
	SchemaVersion string     `json:"schemaVersion"`
	Release       release    `json:"release"`
	Policy        policy     `json:"policy"`
	Clients       []client   `json:"clients"`
	Toolchain     toolchain  `json:"toolchain"`
	Scenarios     []scenario `json:"scenarios"`
	Evidence      []evidence `json:"evidence"`
}

type release struct {
	Version     string `json:"version"`
	ReleaseDate string `json:"releaseDate"`
}

type policy struct {
	MinimumViewport viewport `json:"minimumViewport"`
	Mobile          string   `json:"mobile"`
	Tablet          string   `json:"tablet"`
	Firefox         string   `json:"firefox"`
	Safari          string   `json:"safari"`
}

type viewport struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

type client struct {
	Family      string `json:"family"`
	Slot        string `json:"slot"`
	Version     string `json:"version"`
	ReleaseDate string `json:"releaseDate"`
}

type component struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type toolchain struct {
	Go                   component   `json:"go"`
	GoModules            []component `json:"goModules"`
	Node                 component   `json:"node"`
	PackageManager       component   `json:"packageManager"`
	FrontendDependencies []component `json:"frontendDependencies"`
	PostgreSQL           component   `json:"postgresql"`
	S3CompatibleStorage  component   `json:"s3CompatibleStorage"`
	FFmpeg               component   `json:"ffmpeg"`
	FFprobe              component   `json:"ffprobe"`
}

type scenario struct {
	ID           string   `json:"id"`
	ClientFamily string   `json:"clientFamily"`
	ClientSlot   string   `json:"clientSlot"`
	Viewport     viewport `json:"viewport"`
	Evidence     []string `json:"evidence"`
}

type evidence struct {
	ID              string `json:"id"`
	ReleaseVersion  string `json:"releaseVersion"`
	ScenarioID      string `json:"scenarioId"`
	Location        string `json:"location"`
	SHA256          string `json:"sha256"`
	CorrelationID   string `json:"correlationId"`
	StartedAt       string `json:"startedAt"`
	CompletedAt     string `json:"completedAt"`
	Result          string `json:"result"`
	AccessPolicy    string `json:"accessPolicy"`
	RetentionPolicy string `json:"retentionPolicy"`
}

var (
	immutableVersion = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:+~/@-]*$`)
	identifier       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*$`)
	sha256Pattern    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

var requiredClients = []string{
	"chrome/current", "chrome/previous",
	"edge/current", "edge/previous",
	"obs/current", "obs/previous",
}

var mutableVersionTokens = map[string]bool{
	"current": true, "head": true, "latest": true, "lts": true,
	"main": true, "master": true, "next": true, "previous": true,
	"stable": true, "x": true,
}

// Evaluate strictly decodes and classifies a release manifest.
func Evaluate(data []byte) Report {
	if err := rejectDuplicateFields(data); err != nil {
		return Report{Classification: Fail, Problems: []string{fmt.Sprintf("invalid manifest JSON: %v", err)}}
	}
	var value manifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return Report{Classification: Fail, Problems: []string{fmt.Sprintf("invalid manifest JSON: %v", err)}}
	}
	if err := requireJSONEnd(decoder); err != nil {
		return Report{Classification: Fail, Problems: []string{err.Error()}}
	}

	structural, incomplete, observedFailure := validate(value)
	sort.Strings(structural)
	sort.Strings(incomplete)
	switch {
	case len(structural) > 0 || observedFailure:
		return Report{Classification: Fail, Problems: append(structural, incomplete...)}
	case len(incomplete) > 0:
		return Report{Classification: Inconclusive, Problems: incomplete}
	default:
		return Report{Classification: Pass}
	}
}

func rejectDuplicateFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var walk func(string) error
	walk = func(path string) error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seen := make(map[string]bool)
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("object field name must be a string")
				}
				if seen[key] {
					return fmt.Errorf("duplicate JSON field %q at %s", key, path)
				}
				seen[key] = true
				if err := walk(path + "." + key); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := walk(path + "[]"); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		default:
			return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
		}
	}
	return walk("$")
}

// EvaluateFile reads and evaluates a manifest without changing it.
func EvaluateFile(path string) (Report, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Report{}, fmt.Errorf("read manifest: %w", err)
	}
	return Evaluate(data), nil
}

func requireJSONEnd(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("manifest must contain a single JSON value: %w", err)
	}
	return errors.New("manifest must contain a single JSON value")
}

func validate(value manifest) (structural, incomplete []string, observedFailure bool) {
	if value.SchemaVersion != "1" {
		structural = append(structural, `schemaVersion must be "1"`)
	}
	if !isImmutableVersion(value.Release.Version) {
		structural = append(structural, "release.version must be an exact immutable version")
	}
	if !isDate(value.Release.ReleaseDate) {
		structural = append(structural, "release.releaseDate must be a real YYYY-MM-DD date")
	}
	structural = append(structural, validatePolicy(value.Policy)...)

	clients, clientProblems := validateClients(value.Clients, value.Release.ReleaseDate)
	structural = append(structural, clientProblems...)
	structural = append(structural, validateToolchain(value.Toolchain)...)

	scenarios, scenarioStructural, scenarioIncomplete := validateScenarios(value.Scenarios, clients, value.Policy.MinimumViewport)
	structural = append(structural, scenarioStructural...)
	incomplete = append(incomplete, scenarioIncomplete...)

	evidenceByID, evidenceStructural := indexEvidence(value.Evidence)
	structural = append(structural, evidenceStructural...)
	referenced := make(map[string]bool)
	for _, scenarioValue := range value.Scenarios {
		for _, evidenceID := range scenarioValue.Evidence {
			referenced[evidenceID] = true
			evidenceValue, ok := evidenceByID[evidenceID]
			if !ok {
				incomplete = append(incomplete, fmt.Sprintf("scenario %q references missing evidence %q", scenarioValue.ID, evidenceID))
				continue
			}
			problems, failed := validateEvidence(evidenceValue, value.Release.Version, scenarioValue.ID)
			incomplete = append(incomplete, problems...)
			observedFailure = observedFailure || failed
		}
	}
	for id := range evidenceByID {
		if !referenced[id] {
			incomplete = append(incomplete, fmt.Sprintf("evidence %q is not referenced by a scenario", id))
		}
	}
	for _, key := range requiredClients {
		if scenarios[key] == 0 {
			incomplete = append(incomplete, fmt.Sprintf("missing scenario for client %s", key))
		}
	}
	return structural, incomplete, observedFailure
}

func validatePolicy(value policy) []string {
	var problems []string
	if value.MinimumViewport.Width < 1280 || value.MinimumViewport.Height < 720 {
		problems = append(problems, "policy.minimumViewport must be at least 1280x720")
	}
	for name, actual := range map[string]string{
		"mobile": value.Mobile, "tablet": value.Tablet,
	} {
		if actual != "blocked" {
			problems = append(problems, fmt.Sprintf("policy.%s must be blocked", name))
		}
	}
	for name, actual := range map[string]string{
		"firefox": value.Firefox, "safari": value.Safari,
	} {
		if actual != "excluded" {
			problems = append(problems, fmt.Sprintf("policy.%s must be excluded", name))
		}
	}
	return problems
}

func validateClients(values []client, releaseDate string) (map[string]client, []string) {
	required := make(map[string]bool, len(requiredClients))
	for _, key := range requiredClients {
		required[key] = true
	}
	indexed := make(map[string]client, len(values))
	var problems []string
	for _, value := range values {
		key := value.Family + "/" + value.Slot
		if !required[key] {
			problems = append(problems, fmt.Sprintf("unsupported client slot %q", key))
			continue
		}
		if _, exists := indexed[key]; exists {
			problems = append(problems, fmt.Sprintf("duplicate client slot %q", key))
			continue
		}
		indexed[key] = value
		if !isImmutableVersion(value.Version) {
			problems = append(problems, fmt.Sprintf("client %s must have an exact immutable version", key))
		}
		if !isDate(value.ReleaseDate) {
			problems = append(problems, fmt.Sprintf("client %s releaseDate must be a real YYYY-MM-DD date", key))
		}
	}
	for _, key := range requiredClients {
		if _, exists := indexed[key]; !exists {
			problems = append(problems, fmt.Sprintf("missing client slot %q", key))
		}
	}
	for _, family := range []string{"chrome", "edge", "obs"} {
		current, hasCurrent := indexed[family+"/current"]
		previous, hasPrevious := indexed[family+"/previous"]
		if !hasCurrent || !hasPrevious {
			continue
		}
		if current.Version == previous.Version {
			problems = append(problems, fmt.Sprintf("client %s current and previous slots must have distinct versions", family))
		}
		currentDate, currentErr := time.Parse("2006-01-02", current.ReleaseDate)
		previousDate, previousErr := time.Parse("2006-01-02", previous.ReleaseDate)
		if currentErr == nil && previousErr == nil && !currentDate.After(previousDate) {
			problems = append(problems, fmt.Sprintf("client %s current slot must have a later releaseDate than previous", family))
		}
	}
	manifestDate, manifestDateErr := time.Parse("2006-01-02", releaseDate)
	if manifestDateErr == nil {
		for key, value := range indexed {
			clientDate, err := time.Parse("2006-01-02", value.ReleaseDate)
			if err == nil && clientDate.After(manifestDate) {
				problems = append(problems, fmt.Sprintf("client %s releaseDate cannot be after release.releaseDate", key))
			}
		}
	}
	if len(values) != len(requiredClients) {
		problems = append(problems, fmt.Sprintf("clients must contain exactly %d entries", len(requiredClients)))
	}
	return indexed, problems
}

func validateToolchain(value toolchain) []string {
	var problems []string
	tools := []struct {
		name  string
		value component
	}{
		{"go", value.Go}, {"node", value.Node}, {"packageManager", value.PackageManager},
		{"postgresql", value.PostgreSQL}, {"s3CompatibleStorage", value.S3CompatibleStorage},
		{"ffmpeg", value.FFmpeg}, {"ffprobe", value.FFprobe},
	}
	for _, tool := range tools {
		problems = append(problems, validateComponent("toolchain."+tool.name, tool.value)...)
	}
	if value.GoModules == nil {
		problems = append(problems, "toolchain.goModules must be explicitly present")
	} else {
		problems = append(problems, validateDependencies("toolchain.goModules", value.GoModules)...)
	}
	if value.FrontendDependencies == nil {
		problems = append(problems, "toolchain.frontendDependencies must be explicitly present")
	} else {
		problems = append(problems, validateDependencies("toolchain.frontendDependencies", value.FrontendDependencies)...)
	}
	return problems
}

func validateComponent(path string, value component) []string {
	var problems []string
	if !identifier.MatchString(value.Name) {
		problems = append(problems, path+".name must be an exact identifier")
	}
	if !isImmutableVersion(value.Version) {
		problems = append(problems, path+".version must be an exact immutable version")
	}
	return problems
}

func validateDependencies(path string, values []component) []string {
	seen := make(map[string]bool, len(values))
	var problems []string
	for _, value := range values {
		problems = append(problems, validateComponent(path, value)...)
		if seen[value.Name] {
			problems = append(problems, fmt.Sprintf("duplicate dependency %q in %s", value.Name, path))
		}
		seen[value.Name] = true
	}
	return problems
}

func validateScenarios(values []scenario, clients map[string]client, minimum viewport) (map[string]int, []string, []string) {
	coverage := make(map[string]int)
	seenIDs := make(map[string]bool, len(values))
	var structural, incomplete []string
	for _, value := range values {
		if !identifier.MatchString(value.ID) {
			structural = append(structural, "scenario id must be an exact identifier")
		} else if seenIDs[value.ID] {
			structural = append(structural, fmt.Sprintf("duplicate scenario id %q", value.ID))
		}
		seenIDs[value.ID] = true
		key := value.ClientFamily + "/" + value.ClientSlot
		if _, exists := clients[key]; !exists {
			structural = append(structural, fmt.Sprintf("scenario %q targets unknown client %q", value.ID, key))
		} else {
			coverage[key]++
		}
		if value.Viewport.Width < minimum.Width || value.Viewport.Height < minimum.Height {
			structural = append(structural, fmt.Sprintf("scenario viewport for %q is below policy minimum", value.ID))
		}
		if len(value.Evidence) == 0 {
			incomplete = append(incomplete, fmt.Sprintf("scenario %q has no evidence reference", value.ID))
		}
		references := make(map[string]bool, len(value.Evidence))
		for _, evidenceID := range value.Evidence {
			if references[evidenceID] {
				structural = append(structural, fmt.Sprintf("scenario %q has duplicate evidence reference %q", value.ID, evidenceID))
			}
			references[evidenceID] = true
		}
	}
	return coverage, structural, incomplete
}

func indexEvidence(values []evidence) (map[string]evidence, []string) {
	indexed := make(map[string]evidence, len(values))
	var problems []string
	for _, value := range values {
		if !identifier.MatchString(value.ID) {
			problems = append(problems, "evidence id must be an exact identifier")
		}
		if _, exists := indexed[value.ID]; exists {
			problems = append(problems, fmt.Sprintf("duplicate evidence id %q", value.ID))
			continue
		}
		indexed[value.ID] = value
	}
	return indexed, problems
}

func validateEvidence(value evidence, releaseVersion, scenarioID string) ([]string, bool) {
	var problems []string
	if value.ReleaseVersion != releaseVersion {
		problems = append(problems, fmt.Sprintf("evidence %q releaseVersion does not match the release", value.ID))
	}
	if value.ScenarioID != scenarioID {
		problems = append(problems, fmt.Sprintf("evidence %q scenarioId does not match scenario %q", value.ID, scenarioID))
	}
	location, err := url.Parse(value.Location)
	if err != nil || location.Scheme == "" || location.Scheme == "file" {
		problems = append(problems, fmt.Sprintf("evidence %q location must be a durable absolute URI", value.ID))
	}
	if !sha256Pattern.MatchString(value.SHA256) {
		problems = append(problems, fmt.Sprintf("evidence %q SHA-256 must be 64 lowercase hexadecimal characters", value.ID))
	}
	if !identifier.MatchString(value.CorrelationID) {
		problems = append(problems, fmt.Sprintf("evidence %q correlationId is required", value.ID))
	}
	started, startErr := time.Parse(time.RFC3339, value.StartedAt)
	completed, completeErr := time.Parse(time.RFC3339, value.CompletedAt)
	if startErr != nil {
		problems = append(problems, fmt.Sprintf("evidence %q startedAt must be RFC3339", value.ID))
	}
	if completeErr != nil || (startErr == nil && completed.Before(started)) {
		problems = append(problems, fmt.Sprintf("evidence %q completedAt must be RFC3339 and not precede startedAt", value.ID))
	}
	if !isPolicyIdentifier(value.AccessPolicy) {
		problems = append(problems, fmt.Sprintf("evidence %q accessPolicy must be an exact policy identifier", value.ID))
	}
	if !isPolicyIdentifier(value.RetentionPolicy) {
		problems = append(problems, fmt.Sprintf("evidence %q retentionPolicy must be an exact policy identifier", value.ID))
	}
	switch value.Result {
	case "PASS":
		return problems, false
	case "FAIL":
		problems = append(problems, fmt.Sprintf("evidence %q reported FAIL", value.ID))
		return problems, true
	default:
		problems = append(problems, fmt.Sprintf("evidence %q result must be PASS or FAIL", value.ID))
		return problems, false
	}
}

func isImmutableVersion(value string) bool {
	if !immutableVersion.MatchString(value) || !strings.ContainsAny(value, "0123456789") {
		return false
	}
	for _, token := range strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	}) {
		if mutableVersionTokens[token] {
			return false
		}
	}
	return true
}

func isDate(value string) bool {
	parsed, err := time.Parse("2006-01-02", value)
	return err == nil && parsed.Format("2006-01-02") == value
}

func isPolicyIdentifier(value string) bool {
	lower := strings.ToLower(value)
	return identifier.MatchString(value) && lower != "default" && lower != "latest" && lower != "current"
}
