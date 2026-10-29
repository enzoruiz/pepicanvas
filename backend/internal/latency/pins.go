package latency

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// LoadPins reads the current pins from the release manifest, the backend
// go.mod and the frontend package-lock.json. Errors name files and fields but
// never echo file content.
func LoadPins(manifestPath, goModPath, lockPath string) (Pins, error) {
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return Pins{}, fmt.Errorf("read %s: %w", manifestPath, errors.Unwrap(err))
	}
	pins, err := manifestPins(manifestData)
	if err != nil {
		return Pins{}, fmt.Errorf("release manifest: %w", err)
	}

	goModData, err := os.ReadFile(goModPath)
	if err != nil {
		return Pins{}, fmt.Errorf("read %s: %w", goModPath, errors.Unwrap(err))
	}
	if pins.GoModules, err = goModRequires(string(goModData)); err != nil {
		return Pins{}, fmt.Errorf("go.mod: %w", err)
	}

	lockData, err := os.ReadFile(lockPath)
	if err != nil {
		return Pins{}, fmt.Errorf("read %s: %w", lockPath, errors.Unwrap(err))
	}
	if pins.FrontendDependencies, err = LockPackages(lockData); err != nil {
		return Pins{}, fmt.Errorf("package-lock: %w", err)
	}
	return pins, nil
}

// Mismatches lists the versions fields that differ from the pins, in schema
// field order, or nil when every field matches.
func Mismatches(got Versions, want Pins) []string {
	return versionMismatches(got, want)
}

type component struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// manifestPins decodes the subset of the release manifest that TE-010 pins;
// other fields are ignored because releasecheck validates the full manifest.
func manifestPins(data []byte) (Pins, error) {
	var manifest struct {
		Release struct {
			Version string `json:"version"`
		} `json:"release"`
		Clients   []Client `json:"clients"`
		Toolchain struct {
			Go                  component `json:"go"`
			Node                component `json:"node"`
			PackageManager      component `json:"packageManager"`
			PostgreSQL          component `json:"postgresql"`
			S3CompatibleStorage component `json:"s3CompatibleStorage"`
			FFmpeg              component `json:"ffmpeg"`
			FFprobe             component `json:"ffprobe"`
		} `json:"toolchain"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Pins{}, errors.New("invalid JSON")
	}
	toolchain := manifest.Toolchain
	required := []struct {
		field, value string
	}{
		{"toolchain.go.version", toolchain.Go.Version},
		{"toolchain.node.version", toolchain.Node.Version},
		{"toolchain.packageManager.name", toolchain.PackageManager.Name},
		{"toolchain.packageManager.version", toolchain.PackageManager.Version},
		{"toolchain.postgresql.version", toolchain.PostgreSQL.Version},
		{"toolchain.s3CompatibleStorage.version", toolchain.S3CompatibleStorage.Version},
		{"toolchain.ffmpeg.version", toolchain.FFmpeg.Version},
		{"toolchain.ffprobe.version", toolchain.FFprobe.Version},
	}
	if manifest.Release.Version == "" {
		return Pins{}, errors.New("release.version must not be empty")
	}
	if len(manifest.Clients) == 0 {
		return Pins{}, errors.New("clients must not be empty")
	}
	for _, item := range required {
		if item.value == "" {
			return Pins{}, fmt.Errorf("%s must not be empty", item.field)
		}
	}
	for i, client := range manifest.Clients {
		if client.Family == "" || client.Slot == "" || client.Version == "" {
			return Pins{}, fmt.Errorf("clients[%d] must have family, slot and version", i)
		}
	}
	sum := sha256.Sum256(data)
	return Pins{
		ReleaseManifestSHA256: hex.EncodeToString(sum[:]),
		Release:               manifest.Release.Version,
		Go:                    toolchain.Go.Version,
		Node:                  toolchain.Node.Version,
		PackageManager:        Component{Name: toolchain.PackageManager.Name, Version: toolchain.PackageManager.Version},
		PostgreSQL:            toolchain.PostgreSQL.Version,
		S3CompatibleStorage:   toolchain.S3CompatibleStorage.Version,
		FFmpeg:                toolchain.FFmpeg.Version,
		FFprobe:               toolchain.FFprobe.Version,
		Clients:               manifest.Clients,
	}, nil
}

// goModRequires returns every require entry, direct and indirect, in single-line
// and block form, sorted by name then version.
func goModRequires(text string) ([]Component, error) {
	var modules []Component
	inBlock := false
	for number, line := range strings.Split(text, "\n") {
		if comment := strings.Index(line, "//"); comment >= 0 {
			line = line[:comment]
		}
		fields := strings.Fields(line)
		switch {
		case len(fields) == 0:
			continue
		case inBlock && len(fields) == 1 && fields[0] == ")":
			inBlock = false
			continue
		case inBlock:
		case fields[0] != "require":
			continue
		case len(fields) == 2 && fields[1] == "(":
			inBlock = true
			continue
		default:
			fields = fields[1:]
		}
		if len(fields) != 2 {
			return nil, fmt.Errorf("line %d: malformed require entry", number+1)
		}
		name := fields[0]
		if unquoted, err := strconv.Unquote(name); err == nil {
			name = unquoted
		}
		modules = append(modules, Component{Name: name, Version: fields[1]})
	}
	if inBlock {
		return nil, errors.New("unterminated require block")
	}
	sortComponents(modules)
	return modules, nil
}

// LockPackages returns every non-root packages entry of a package-lock.json,
// named by the path after the last node_modules/, sorted by name then version
// with duplicates once. A "link": true entry has no version of its own and is
// skipped; the package it links to has its own entry.
func LockPackages(data []byte) ([]Component, error) {
	var lock struct {
		Packages map[string]struct {
			Version string `json:"version"`
			Link    bool   `json:"link"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(data, &lock); err != nil {
		return nil, errors.New("invalid JSON")
	}
	if len(lock.Packages) == 0 {
		return nil, errors.New("packages must not be empty")
	}
	seen := make(map[Component]bool)
	var packages []Component
	for path, entry := range lock.Packages {
		if path == "" || entry.Link {
			continue
		}
		if entry.Version == "" {
			return nil, fmt.Errorf("%s has no version", path)
		}
		name := path
		if index := strings.LastIndex(path, "node_modules/"); index >= 0 {
			name = path[index+len("node_modules/"):]
		}
		value := Component{Name: name, Version: entry.Version}
		if !seen[value] {
			seen[value] = true
			packages = append(packages, value)
		}
	}
	sortComponents(packages)
	return packages, nil
}

func sortComponents(values []Component) {
	sort.Slice(values, func(i, j int) bool {
		if values[i].Name != values[j].Name {
			return values[i].Name < values[j].Name
		}
		return values[i].Version < values[j].Version
	})
}
