package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/evidence"
	"github.com/enzoruiz/pepicanvas/backend/internal/latency"
)

const testManifest = `{
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

const (
	testGoMod = "module x\n\nrequire github.com/a/mod v1.0.0\n"
	testLock  = `{"packages":{"":{},"node_modules/react":{"version":"19.3.0"}}}`
)

type repo struct {
	manifest, goMod, lock string
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeRepo(t *testing.T) repo {
	t.Helper()
	dir := t.TempDir()
	return repo{
		manifest: writeFile(t, dir, "release.json", testManifest),
		goMod:    writeFile(t, dir, "go.mod", testGoMod),
		lock:     writeFile(t, dir, "package-lock.json", testLock),
	}
}

func (r repo) args(dirs ...string) []string {
	return append([]string{"-manifest", r.manifest, "-gomod", r.goMod, "-lock", r.lock}, dirs...)
}

// begin starts a run pinned to the repository r.
func begin(t *testing.T, r repo) *evidence.Writer {
	t.Helper()
	pins, err := latency.LoadPins(r.manifest, r.goMod, r.lock)
	if err != nil {
		t.Fatal(err)
	}
	w, err := evidence.Begin(t.TempDir(), evidence.Run{
		Scenario:      "release-smoke",
		StartedAt:     time.Now().Add(-time.Hour),
		CorrelationID: "0123456789abcdef0123456789abcdef",
		Versions:      pins,
		Role:          "release-verifier",
		RetentionDays: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Add("data.txt", "log", []byte("smoke passed\n")); err != nil {
		t.Fatal(err)
	}
	return w
}

func completeRun(t *testing.T, r repo, result string) string {
	t.Helper()
	w := begin(t, r)
	if err := w.Complete(result, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	return w.Dir()
}

func TestRunClassifiesEachRun(t *testing.T) {
	r := writeRepo(t)
	pass := completeRun(t, r, evidence.ResultPass)
	passLine := pass + " run=" + filepath.Base(pass) + " scenario=release-smoke recorded=PASS classification=PASS problems=-\n"
	incomplete := begin(t, r).Dir()
	incompleteLine := incomplete + " run=- scenario=- recorded=- classification=INCOMPLETE problems=incomplete_marker\n"
	tampered := completeRun(t, r, evidence.ResultInconclusive)
	writeFile(t, tampered, "data.txt", "smoke failed\n")
	tamperedLine := tampered + " run=" + filepath.Base(tampered) +
		" scenario=release-smoke recorded=INCONCLUSIVE classification=FAIL problems=hash_mismatch:data.txt,recorded_inconclusive\n"

	tests := []struct {
		name     string
		dirs     []string
		wantCode int
		wantOut  string
	}{
		{name: "pass", dirs: []string{pass}, wantCode: 0, wantOut: passLine},
		{name: "every pass", dirs: []string{pass, pass}, wantCode: 0, wantOut: passLine + passLine},
		{name: "incomplete", dirs: []string{incomplete}, wantCode: 1, wantOut: incompleteLine},
		{name: "any non-pass fails, in argument order", dirs: []string{tampered, pass, incomplete}, wantCode: 1,
			wantOut: tamperedLine + passLine + incompleteLine},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(r.args(tt.dirs...), &stdout, &stderr)
			if code != tt.wantCode || stdout.String() != tt.wantOut || stderr.Len() != 0 {
				t.Fatalf("run() = %d\nstdout:\n%s\nstderr:\n%s\nwant %d\n%s", code, stdout.String(), stderr.String(), tt.wantCode, tt.wantOut)
			}
		})
	}
}

func TestRunDetectsRepositoryDrift(t *testing.T) {
	r := writeRepo(t)
	dir := completeRun(t, r, evidence.ResultPass)
	r.goMod = writeFile(t, t.TempDir(), "go.mod", strings.Replace(testGoMod, "v1.0.0", "v1.0.1", 1))

	var stdout, stderr bytes.Buffer
	code := run(r.args(dir), &stdout, &stderr)
	want := " recorded=PASS classification=INCONCLUSIVE problems=version_mismatch:goModules\n"
	if code != 1 || !strings.HasSuffix(stdout.String(), want) || stderr.Len() != 0 {
		t.Fatalf("run() = %d stdout=%q stderr=%q, want suffix %q", code, stdout.String(), stderr.String(), want)
	}
}

func TestRunErrors(t *testing.T) {
	r := writeRepo(t)
	dir := completeRun(t, r, evidence.ResultPass)
	missing := filepath.Join(t.TempDir(), "absent.json")
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "no arguments", wantErr: "usage: evidencecheck -manifest"},
		{name: "no run dirs", args: r.args(), wantErr: "usage: evidencecheck -manifest"},
		{name: "missing lock flag", args: []string{"-manifest", r.manifest, "-gomod", r.goMod, dir}, wantErr: "usage: evidencecheck -manifest"},
		{name: "unknown flag", args: append([]string{"-x"}, r.args(dir)...), wantErr: "usage: evidencecheck -manifest"},
		{name: "unreadable manifest", args: []string{"-manifest", missing, "-gomod", r.goMod, "-lock", r.lock, dir},
			wantErr: "evidencecheck: read " + missing},
		{name: "invalid lock", args: []string{"-manifest", r.manifest, "-gomod", r.goMod,
			"-lock", writeFile(t, t.TempDir(), "lock.json", `{"packages":{}}`), dir},
			wantErr: "evidencecheck: package-lock: packages must not be empty\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tt.args, &stdout, &stderr)
			if code != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), tt.wantErr) {
				t.Fatalf("run() = %d stdout=%q stderr=%q, want 2, no stdout and %q", code, stdout.String(), stderr.String(), tt.wantErr)
			}
		})
	}
}

// S4: a run dir given as "." or with a trailing slash names the same run.
func TestRunAcceptsDotAndTrailingSlashRunDirs(t *testing.T) {
	r := writeRepo(t)
	dir := completeRun(t, r, evidence.ResultPass)
	t.Chdir(dir)

	var stdout, stderr bytes.Buffer
	code := run(r.args(".", dir+"/"), &stdout, &stderr)
	suffix := " run=" + filepath.Base(dir) + " scenario=release-smoke recorded=PASS classification=PASS problems=-\n"
	want := "." + suffix + dir + "/" + suffix
	if code != 0 || stdout.String() != want || stderr.Len() != 0 {
		t.Fatalf("run() = %d stdout=%q stderr=%q, want 0 and %q", code, stdout.String(), stderr.String(), want)
	}
}

// S6: a symlinked run dir is not trusted evidence and fails explicitly.
func TestRunFailsASymlinkedRunDir(t *testing.T) {
	r := writeRepo(t)
	dir := completeRun(t, r, evidence.ResultPass)
	link := filepath.Join(t.TempDir(), filepath.Base(dir))
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run(r.args(link, link+"/"), &stdout, &stderr)
	suffix := " run=- scenario=- recorded=- classification=FAIL problems=irregular_file:run\n"
	want := link + suffix + link + "/" + suffix
	if code != 1 || stdout.String() != want || stderr.Len() != 0 {
		t.Fatalf("run() = %d stdout=%q stderr=%q, want 1 and %q", code, stdout.String(), stderr.String(), want)
	}
}
