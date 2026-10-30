package latency_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/enzoruiz/pepicanvas/backend/internal/latency"
)

const pinsManifest = `{
  "release": {"version": "0.1.0"},
  "clients": [{"family": "chrome", "slot": "current", "version": "155.0.8059.79"}],
  "toolchain": {
    "go": {"version": "1.27.2"},
    "node": {"version": "24.16.0"},
    "packageManager": {"name": "npm", "version": "11.13.0"},
    "postgresql": {"version": "17.4"},
    "s3CompatibleStorage": {"version": "1.0.0-contract"},
    "ffmpeg": {"version": "7.1.1"},
    "ffprobe": {"version": "7.1.1"}
  }
}
`

func writePinFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadPinsReadsEveryPinnedVersion(t *testing.T) {
	dir := t.TempDir()
	manifest := writePinFile(t, dir, "release.json", pinsManifest)
	goMod := writePinFile(t, dir, "go.mod", "module x\n\nrequire (\n\tgithub.com/b/mod v0.2.0\n\tgithub.com/a/mod v1.0.0 // indirect\n)\n")
	lock := writePinFile(t, dir, "package-lock.json", `{"packages":{"":{},"node_modules/react":{"version":"19.3.0"},"node_modules/linked":{"link":true}}}`)

	got, err := latency.LoadPins(manifest, goMod, lock)
	if err != nil {
		t.Fatalf("LoadPins() error = %v", err)
	}
	sum := sha256.Sum256([]byte(pinsManifest))
	want := latency.Pins{
		ReleaseManifestSHA256: hex.EncodeToString(sum[:]),
		Release:               "0.1.0",
		Go:                    "1.27.2",
		Node:                  "24.16.0",
		PackageManager:        latency.Component{Name: "npm", Version: "11.13.0"},
		GoModules:             []latency.Component{{Name: "github.com/a/mod", Version: "v1.0.0"}, {Name: "github.com/b/mod", Version: "v0.2.0"}},
		FrontendDependencies:  []latency.Component{{Name: "react", Version: "19.3.0"}},
		PostgreSQL:            "17.4",
		S3CompatibleStorage:   "1.0.0-contract",
		FFmpeg:                "7.1.1",
		FFprobe:               "7.1.1",
		Clients:               []latency.Client{{Family: "chrome", Slot: "current", Version: "155.0.8059.79"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LoadPins() =\n%+v\nwant\n%+v", got, want)
	}
}

func TestLoadPinsErrorsNameTheFile(t *testing.T) {
	dir := t.TempDir()
	manifest := writePinFile(t, dir, "release.json", pinsManifest)
	goMod := writePinFile(t, dir, "go.mod", "module x\n")
	missing := filepath.Join(dir, "absent.json")

	_, err := latency.LoadPins(manifest, goMod, missing)
	if err == nil || !strings.HasPrefix(err.Error(), "read "+missing) {
		t.Fatalf("LoadPins() error = %v, want read %s", err, missing)
	}
	_, err = latency.LoadPins(manifest, goMod, writePinFile(t, dir, "lock.json", `{"packages":{}}`))
	if err == nil || err.Error() != "package-lock: packages must not be empty" {
		t.Fatalf("LoadPins() error = %v", err)
	}
}

func TestMismatchesListsFieldsInSchemaOrder(t *testing.T) {
	pins := testPins()
	got := pins
	got.Go = "0"
	got.Clients = nil

	if fields := latency.Mismatches(pins, pins); fields != nil {
		t.Fatalf("Mismatches(equal) = %v, want none", fields)
	}
	if fields := latency.Mismatches(got, pins); !reflect.DeepEqual(fields, []string{"go", "clients"}) {
		t.Fatalf("Mismatches() = %v, want [go clients]", fields)
	}
}
