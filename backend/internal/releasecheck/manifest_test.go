package releasecheck

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEvaluateManifest(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(map[string]any)
		want        Classification
		wantProblem string
	}{
		{name: "complete passing evidence", want: Pass},
		{
			name: "unknown field",
			mutate: func(manifest map[string]any) {
				manifest["unexpected"] = true
			},
			want: Fail, wantProblem: "unknown field",
		},
		{
			name: "missing required client slot",
			mutate: func(manifest map[string]any) {
				manifest["clients"] = manifest["clients"].([]any)[:5]
			},
			want: Fail, wantProblem: "missing client",
		},
		{
			name: "duplicate client slot",
			mutate: func(manifest map[string]any) {
				clients := manifest["clients"].([]any)
				clients[1].(map[string]any)["slot"] = "current"
			},
			want: Fail, wantProblem: "duplicate client",
		},
		{
			name: "extra client family",
			mutate: func(manifest map[string]any) {
				clients := manifest["clients"].([]any)
				manifest["clients"] = append(clients, map[string]any{
					"family": "firefox", "slot": "current", "version": "130.0.1", "releaseDate": "2026-10-01",
				})
			},
			want: Fail, wantProblem: "unsupported client",
		},
		{
			name: "mutable client version",
			mutate: func(manifest map[string]any) {
				manifest["clients"].([]any)[0].(map[string]any)["version"] = "latest"
			},
			want: Fail, wantProblem: "immutable version",
		},
		{
			name: "invalid client release date",
			mutate: func(manifest map[string]any) {
				manifest["clients"].([]any)[0].(map[string]any)["releaseDate"] = "2026-02-30"
			},
			want: Fail, wantProblem: "releaseDate",
		},
		{
			name: "current client must differ from previous",
			mutate: func(manifest map[string]any) {
				clients := manifest["clients"].([]any)
				clients[0].(map[string]any)["version"] = clients[1].(map[string]any)["version"]
			},
			want: Fail, wantProblem: "distinct versions",
		},
		{
			name: "current client must have later release date",
			mutate: func(manifest map[string]any) {
				manifest["clients"].([]any)[0].(map[string]any)["releaseDate"] = "1999-12-01"
			},
			want: Fail, wantProblem: "later releaseDate",
		},
		{
			name: "client cannot postdate release",
			mutate: func(manifest map[string]any) {
				manifest["clients"].([]any)[0].(map[string]any)["releaseDate"] = "2000-04-01"
			},
			want: Fail, wantProblem: "after release.releaseDate",
		},
		{
			name: "viewport below minimum",
			mutate: func(manifest map[string]any) {
				manifest["policy"].(map[string]any)["minimumViewport"].(map[string]any)["width"] = float64(1279)
			},
			want: Fail, wantProblem: "1280x720",
		},
		{
			name: "mobile not blocked",
			mutate: func(manifest map[string]any) {
				manifest["policy"].(map[string]any)["mobile"] = "supported"
			},
			want: Fail, wantProblem: "mobile",
		},
		{
			name: "Safari not excluded",
			mutate: func(manifest map[string]any) {
				manifest["policy"].(map[string]any)["safari"] = "supported"
			},
			want: Fail, wantProblem: "safari",
		},
		{
			name: "missing exact tool version",
			mutate: func(manifest map[string]any) {
				manifest["toolchain"].(map[string]any)["ffprobe"].(map[string]any)["version"] = ""
			},
			want: Fail, wantProblem: "toolchain.ffprobe",
		},
		{
			name: "mutable tool version",
			mutate: func(manifest map[string]any) {
				manifest["toolchain"].(map[string]any)["node"].(map[string]any)["version"] = "lts"
			},
			want: Fail, wantProblem: "immutable version",
		},
		{
			name: "exact ecosystem version formats",
			mutate: func(manifest map[string]any) {
				toolchain := manifest["toolchain"].(map[string]any)
				toolchain["goModules"] = []any{map[string]any{"name": "example.com/module", "version": "v1.2.3"}}
				toolchain["ffmpeg"].(map[string]any)["version"] = "7:6.1.1-3ubuntu5"
				toolchain["s3CompatibleStorage"].(map[string]any)["version"] = "RELEASE.2026-01-02T03-04-05Z"
			},
			want: Pass,
		},
		{
			name: "wildcard dependency version",
			mutate: func(manifest map[string]any) {
				manifest["toolchain"].(map[string]any)["goModules"] = []any{
					map[string]any{"name": "example.com/module", "version": "v1.x"},
				}
			},
			want: Fail, wantProblem: "immutable version",
		},
		{
			name: "missing explicit module list",
			mutate: func(manifest map[string]any) {
				delete(manifest["toolchain"].(map[string]any), "goModules")
			},
			want: Fail, wantProblem: "goModules",
		},
		{
			name: "duplicate frontend dependency",
			mutate: func(manifest map[string]any) {
				dependency := map[string]any{"name": "react", "version": "19.1.1"}
				manifest["toolchain"].(map[string]any)["frontendDependencies"] = []any{dependency, dependency}
			},
			want: Fail, wantProblem: "duplicate dependency",
		},
		{
			name: "missing scenario",
			mutate: func(manifest map[string]any) {
				manifest["scenarios"] = manifest["scenarios"].([]any)[:5]
			},
			want: Inconclusive, wantProblem: "missing scenario",
		},
		{
			name: "duplicate scenario identity",
			mutate: func(manifest map[string]any) {
				scenarios := manifest["scenarios"].([]any)
				scenarios[1].(map[string]any)["id"] = scenarios[0].(map[string]any)["id"]
			},
			want: Fail, wantProblem: "duplicate scenario",
		},
		{
			name: "scenario viewport below minimum",
			mutate: func(manifest map[string]any) {
				manifest["scenarios"].([]any)[0].(map[string]any)["viewport"].(map[string]any)["height"] = float64(719)
			},
			want: Fail, wantProblem: "scenario viewport",
		},
		{
			name: "scenario client version mismatch",
			mutate: func(manifest map[string]any) {
				manifest["clients"].([]any)[0].(map[string]any)["version"] = "3.0.0-fixture"
			},
			want: Fail, wantProblem: "does not match client",
		},
		{
			name: "missing evidence reference",
			mutate: func(manifest map[string]any) {
				manifest["scenarios"].([]any)[0].(map[string]any)["evidence"] = []any{}
			},
			want: Inconclusive, wantProblem: "evidence reference",
		},
		{
			name: "unresolved evidence reference",
			mutate: func(manifest map[string]any) {
				manifest["scenarios"].([]any)[0].(map[string]any)["evidence"] = []any{"missing-evidence"}
			},
			want: Inconclusive, wantProblem: "missing-evidence",
		},
		{
			name: "invalid evidence SHA-256",
			mutate: func(manifest map[string]any) {
				manifest["evidence"].([]any)[0].(map[string]any)["sha256"] = "abc"
			},
			want: Inconclusive, wantProblem: "SHA-256",
		},
		{
			name: "duplicate evidence identity",
			mutate: func(manifest map[string]any) {
				evidence := manifest["evidence"].([]any)
				evidence[1].(map[string]any)["id"] = evidence[0].(map[string]any)["id"]
			},
			want: Fail, wantProblem: "duplicate evidence",
		},
		{
			name: "missing evidence access policy",
			mutate: func(manifest map[string]any) {
				manifest["evidence"].([]any)[0].(map[string]any)["accessPolicy"] = ""
			},
			want: Inconclusive, wantProblem: "accessPolicy",
		},
		{
			name: "evidence release mismatch",
			mutate: func(manifest map[string]any) {
				manifest["evidence"].([]any)[0].(map[string]any)["releaseVersion"] = "8.0.0-fixture"
			},
			want: Inconclusive, wantProblem: "releaseVersion",
		},
		{
			name: "evidence time order invalid",
			mutate: func(manifest map[string]any) {
				manifest["evidence"].([]any)[0].(map[string]any)["completedAt"] = "2000-03-01T09:59:59Z"
			},
			want: Inconclusive, wantProblem: "completedAt",
		},
		{
			name: "failing evidence blocks release",
			mutate: func(manifest map[string]any) {
				manifest["evidence"].([]any)[0].(map[string]any)["result"] = "FAIL"
			},
			want: Fail, wantProblem: "reported FAIL",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest := validManifest(t)
			if tt.mutate != nil {
				tt.mutate(manifest)
			}
			data, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}

			report := Evaluate(data)
			if report.Classification != tt.want {
				t.Fatalf("classification = %q, want %q; problems: %v", report.Classification, tt.want, report.Problems)
			}
			if tt.wantProblem != "" && !containsProblem(report.Problems, tt.wantProblem) {
				t.Fatalf("problems %v do not contain %q", report.Problems, tt.wantProblem)
			}
		})
	}
}

func TestEvaluateRejectsTrailingJSON(t *testing.T) {
	data, err := json.Marshal(validManifest(t))
	if err != nil {
		t.Fatal(err)
	}
	report := Evaluate(append(data, []byte(` {}`)...))
	if report.Classification != Fail || !containsProblem(report.Problems, "single JSON value") {
		t.Fatalf("report = %#v", report)
	}
}

func TestEvaluateRejectsDuplicateJSONFields(t *testing.T) {
	data, err := json.Marshal(validManifest(t))
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte(`{`), []byte(`{"schemaVersion":"1",`), 1)
	report := Evaluate(data)
	if report.Classification != Fail || !containsProblem(report.Problems, "duplicate JSON field") {
		t.Fatalf("report = %#v", report)
	}
}

func TestEvaluateFile(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "release.json")
	data, err := json.Marshal(validManifest(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := EvaluateFile(path)
	if err != nil {
		t.Fatalf("EvaluateFile() error = %v", err)
	}
	if report.Classification != Pass {
		t.Fatalf("classification = %q, problems = %v", report.Classification, report.Problems)
	}
}

func validManifest(t *testing.T) map[string]any {
	t.Helper()
	data := []byte(`{
  "schemaVersion": "1",
  "release": {"version": "9.0.0-fixture", "releaseDate": "2000-03-01"},
  "policy": {
    "minimumViewport": {"width": 1280, "height": 720},
    "mobile": "blocked", "tablet": "blocked",
    "firefox": "excluded", "safari": "excluded"
  },
  "clients": [
    {"family": "chrome", "slot": "current", "version": "2.0.0-fixture", "releaseDate": "2000-02-02"},
    {"family": "chrome", "slot": "previous", "version": "1.0.0-fixture", "releaseDate": "2000-01-01"},
    {"family": "edge", "slot": "current", "version": "2.0.0-fixture", "releaseDate": "2000-02-02"},
    {"family": "edge", "slot": "previous", "version": "1.0.0-fixture", "releaseDate": "2000-01-01"},
    {"family": "obs", "slot": "current", "version": "2.0.0-fixture", "releaseDate": "2000-02-02"},
    {"family": "obs", "slot": "previous", "version": "1.0.0-fixture", "releaseDate": "2000-01-01"}
  ],
  "toolchain": {
    "go": {"name": "go", "version": "1.25.1"},
    "goModules": [],
    "node": {"name": "node", "version": "24.8.0"},
    "packageManager": {"name": "npm", "version": "11.6.0"},
    "frontendDependencies": [],
    "postgresql": {"name": "postgresql", "version": "17.4"},
    "s3CompatibleStorage": {"name": "minio", "version": "2026.1.0"},
    "ffmpeg": {"name": "ffmpeg", "version": "7.1.1"},
    "ffprobe": {"name": "ffprobe", "version": "7.1.1"}
  },
  "scenarios": [],
  "evidence": []
}`)
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}

	clients := manifest["clients"].([]any)
	for index, clientValue := range clients {
		client := clientValue.(map[string]any)
		scenarioID := client["family"].(string) + "-" + client["slot"].(string)
		evidenceID := "evidence-" + scenarioID
		manifest["scenarios"] = append(manifest["scenarios"].([]any), map[string]any{
			"id": scenarioID, "clientFamily": client["family"], "clientSlot": client["slot"],
			"clientVersion": client["version"],
			"viewport":      map[string]any{"width": float64(1280), "height": float64(720)},
			"evidence":      []any{evidenceID},
		})
		manifest["evidence"] = append(manifest["evidence"].([]any), map[string]any{
			"id": evidenceID, "releaseVersion": "9.0.0-fixture", "scenarioId": scenarioID,
			"location":      "s3://release-evidence/fixture/" + scenarioID + ".json",
			"sha256":        strings.Repeat(string(rune('a'+index)), 64),
			"correlationId": "fixture-run-" + scenarioID,
			"startedAt":     "2000-03-01T10:00:00Z", "completedAt": "2000-03-01T10:01:00Z",
			"result": "PASS", "accessPolicy": "release-evidence-readers-v1",
			"retentionPolicy": "release-evidence-365d-v1",
		})
	}
	return manifest
}

func containsProblem(problems []string, fragment string) bool {
	for _, problem := range problems {
		if strings.Contains(problem, fragment) {
			return true
		}
	}
	return false
}
