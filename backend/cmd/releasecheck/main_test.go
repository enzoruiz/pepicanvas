package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	valid := cliManifest(t)
	missingEvidence := cliManifest(t)
	delete(missingEvidence, "evidence")

	tests := []struct {
		name       string
		manifest   map[string]any
		args       []string
		wantCode   int
		wantOutput string
	}{
		{name: "passing file", manifest: valid, wantCode: 0, wantOutput: "PASS\n"},
		{name: "missing evidence blocks release", manifest: missingEvidence, wantCode: 1, wantOutput: "INCONCLUSIVE\n"},
		{name: "invalid arguments", args: []string{"one", "two"}, wantCode: 2, wantOutput: "usage:"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var output, errorOutput bytes.Buffer
			args := tt.args
			if tt.manifest != nil {
				path := filepath.Join(t.TempDir(), "manifest.json")
				data, err := json.Marshal(tt.manifest)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
				args = []string{path}
			}

			code := run(args, &output, &errorOutput)
			combined := output.String() + errorOutput.String()
			if code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d; output: %s", code, tt.wantCode, combined)
			}
			if !strings.Contains(combined, tt.wantOutput) {
				t.Fatalf("output %q does not contain %q", combined, tt.wantOutput)
			}
		})
	}
}

func TestReleaseTemplateBlocksBeforeHostedEvidence(t *testing.T) {
	path := filepath.Join("..", "..", "..", "releases", "client-support-release.template.json")
	var output, errorOutput bytes.Buffer

	code := run([]string{path}, &output, &errorOutput)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stdout: %s; stderr: %s", code, output.String(), errorOutput.String())
	}
	if !strings.HasPrefix(output.String(), "INCONCLUSIVE\n") {
		t.Fatalf("output = %q, want INCONCLUSIVE", output.String())
	}
	for _, scenario := range []string{
		"chrome-current", "chrome-previous", "edge-current", "edge-previous",
		"obs-browser-source-current", "obs-browser-source-previous",
	} {
		if !strings.Contains(output.String(), fmt.Sprintf("scenario %q has no evidence reference", scenario)) {
			t.Fatalf("output does not identify missing evidence for %q: %s", scenario, output.String())
		}
	}
}

func cliManifest(t *testing.T) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal([]byte(`{
  "schemaVersion":"1",
  "release":{"version":"1.0.0","releaseDate":"2026-10-11"},
  "policy":{"minimumViewport":{"width":1280,"height":720},"mobile":"blocked","tablet":"blocked","firefox":"excluded","safari":"excluded"},
  "clients":[
    {"family":"chrome","slot":"current","version":"2.0.0","releaseDate":"2026-10-01"},
    {"family":"chrome","slot":"previous","version":"1.0.0","releaseDate":"2026-09-01"},
    {"family":"edge","slot":"current","version":"2.0.0","releaseDate":"2026-10-01"},
    {"family":"edge","slot":"previous","version":"1.0.0","releaseDate":"2026-09-01"},
    {"family":"obs","slot":"current","version":"2.0.0","releaseDate":"2026-10-01"},
    {"family":"obs","slot":"previous","version":"1.0.0","releaseDate":"2026-09-01"}
  ],
  "toolchain":{
    "go":{"name":"go","version":"1.25.1"},"goModules":[],
    "node":{"name":"node","version":"24.8.0"},
    "packageManager":{"name":"npm","version":"11.6.0"},"frontendDependencies":[],
    "postgresql":{"name":"postgresql","version":"17.4"},
    "s3CompatibleStorage":{"name":"storage","version":"1.0.0"},
    "ffmpeg":{"name":"ffmpeg","version":"7.1.1"},
    "ffprobe":{"name":"ffprobe","version":"7.1.1"}
  },
  "scenarios":[],"evidence":[]
}`), &value); err != nil {
		t.Fatal(err)
	}
	for index, clientValue := range value["clients"].([]any) {
		client := clientValue.(map[string]any)
		id := client["family"].(string) + "-" + client["slot"].(string)
		evidenceID := "evidence-" + id
		value["scenarios"] = append(value["scenarios"].([]any), map[string]any{
			"id": id, "clientFamily": client["family"], "clientSlot": client["slot"],
			"clientVersion": client["version"],
			"viewport":      map[string]any{"width": float64(1280), "height": float64(720)},
			"evidence":      []any{evidenceID},
		})
		value["evidence"] = append(value["evidence"].([]any), map[string]any{
			"id": evidenceID, "releaseVersion": "1.0.0", "scenarioId": id,
			"location": "evidence://releases/1.0.0/" + id,
			"sha256":   strings.Repeat(string(rune('a'+index)), 64), "correlationId": "run-" + id,
			"startedAt": "2026-10-11T10:00:00Z", "completedAt": "2026-10-11T10:01:00Z",
			"result": "PASS", "accessPolicy": "readers-v1", "retentionPolicy": "retention-365d-v1",
		})
	}
	return value
}
