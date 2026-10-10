package mediaexec

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/media"
)

const (
	realFFmpegPath     = "/usr/bin/ffmpeg"
	realFFprobePath    = "/usr/bin/ffprobe"
	realFFmpegVersion  = "ffmpeg version 6.1.1-3ubuntu5 Copyright (c) 2000-2023 the FFmpeg developers"
	realFFprobeVersion = "ffprobe version 6.1.1-3ubuntu5 Copyright (c) 2007-2023 the FFmpeg developers"
)

type fixtureManifest struct {
	SchemaVersion int                    `json:"schema_version"`
	Intent        string                 `json:"deterministic_generation_intent"`
	Tools         fixtureTools           `json:"tools"`
	Fixtures      []fixtureManifestEntry `json:"fixtures"`
}

type fixtureTools struct {
	FFmpegPath       string `json:"ffmpeg_path"`
	FFmpegFirstLine  string `json:"ffmpeg_first_line"`
	FFprobePath      string `json:"ffprobe_path"`
	FFprobeFirstLine string `json:"ffprobe_first_line"`
}

type fixtureManifestEntry struct {
	ID             string     `json:"id"`
	Filename       string     `json:"filename"`
	GenerationArgs []string   `json:"generation_args"`
	SHA256         string     `json:"sha256"`
	Kind           media.Kind `json:"kind"`
	Container      container  `json:"container"`
	VideoCodec     string     `json:"video_codec,omitempty"`
	AudioCodec     string     `json:"audio_codec,omitempty"`
	Width          int        `json:"width,omitempty"`
	Height         int        `json:"height,omitempty"`
	DurationNS     int64      `json:"duration_ns,omitempty"`
	FrameCount     int        `json:"frame_count,omitempty"`
	StreamCount    int        `json:"stream_count"`
}

func TestPinnedRealMediaFixtures(t *testing.T) {
	if os.Getenv("PEPICANVAS_TEST_MEDIA_TOOLS") != "1" {
		t.Skip("set PEPICANVAS_TEST_MEDIA_TOOLS=1 to run pinned real-media integration evidence")
	}
	if testing.Short() {
		t.Skip("pinned real-media integration evidence is disabled in short mode")
	}

	manifest := loadFixtureManifest(t)
	validatePinnedTools(t, manifest.Tools)
	validateFixtureManifest(t, manifest)

	root := t.TempDir()
	generatedRoot := filepath.Join(root, "generated")
	stagingRoot := filepath.Join(root, "staging")
	for _, path := range []string{generatedRoot, stagingRoot} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatalf("fixture setup failure: mkdir: %v", err)
		}
	}

	generated := make(map[string]string, len(manifest.Fixtures))
	for _, fixture := range manifest.Fixtures {
		fixture := fixture
		t.Run("generate/"+fixture.ID, func(t *testing.T) {
			first := generateFixture(t, generatedRoot, "first", fixture)
			second := generateFixture(t, generatedRoot, "second", fixture)
			firstHash := fileSHA256(t, first)
			secondHash := fileSHA256(t, second)
			if firstHash != secondHash {
				t.Fatalf("fixture reproducibility failure: %s hashes differ: %s != %s", fixture.ID, firstHash, secondHash)
			}
			if firstHash != fixture.SHA256 {
				t.Errorf("fixture manifest hash mismatch: %s generated %s, manifest %s", fixture.ID, firstHash, fixture.SHA256)
			}
			generated[fixture.ID] = first
		})
	}
	if t.Failed() {
		t.FailNow()
	}

	for _, fixture := range manifest.Fixtures {
		fixture := fixture
		t.Run("inspect/"+fixture.ID, func(t *testing.T) {
			contents, err := os.ReadFile(generated[fixture.ID])
			if err != nil {
				t.Fatalf("fixture read failure: %s: %v", fixture.ID, err)
			}
			execution := &directProcessExecution{}
			adapter := newRealToolAdapter(t, stagingRoot, directProcessRunner{execution: execution})
			inspection, err := adapter.Inspect(context.Background(), bytes.NewReader(contents))
			if err != nil {
				t.Fatalf("%s while inspecting %s: code=%s public=%q private=%s", execution.failureStage(), fixture.ID, adapterErrorCode(err), err.Error(), execution.privateDiagnostic())
			}
			assertManifestInspection(t, fixture, inspection, execution.metadata)
			assertStagingRemoved(t, stagingRoot, execution.commandDirs)
		})
	}

	t.Run("content-derived classification ignores misleading names", func(t *testing.T) {
		for _, id := range []string{"jpeg-misnamed-mp3", "mp3-misnamed-png"} {
			fixture := fixtureByID(t, manifest, id)
			if filepath.Ext(fixture.Filename) == expectedExtension(fixture.Kind, fixture.Container) {
				t.Fatalf("fixture %s does not have a misleading extension", id)
			}
			contents, err := os.ReadFile(generated[id])
			if err != nil {
				t.Fatal(err)
			}
			execution := &directProcessExecution{}
			inspection, err := newRealToolAdapter(t, stagingRoot, directProcessRunner{execution: execution}).Inspect(context.Background(), bytes.NewReader(contents))
			if err != nil || inspection.Kind != fixture.Kind {
				t.Fatalf("content-derived classification for %s = %q, %v; want %q, nil", id, inspection.Kind, err, fixture.Kind)
			}
			assertStagingRemoved(t, stagingRoot, execution.commandDirs)
		}
	})

	t.Run("truncated and malformed media fail safely", func(t *testing.T) {
		cases := []struct {
			name    string
			fixture string
			bytes   func([]byte) []byte
		}{
			{name: "image", fixture: "jpeg-misnamed-mp3", bytes: prefix(12)},
			{name: "GIF", fixture: "animated-gif", bytes: prefix(20)},
			{name: "audio", fixture: "mp3-misnamed-png", bytes: prefix(16)},
			{name: "video", fixture: "mp4-h264-aac", bytes: prefix(32)},
		}
		for _, tt := range cases {
			t.Run(tt.name, func(t *testing.T) {
				valid, err := os.ReadFile(generated[tt.fixture])
				if err != nil {
					t.Fatal(err)
				}
				malformed := tt.bytes(valid)
				execution := &directProcessExecution{}
				_, err = newRealToolAdapter(t, stagingRoot, directProcessRunner{execution: execution}).Inspect(context.Background(), bytes.NewReader(malformed))
				if err == nil {
					t.Fatalf("probe/normalization/validation failure missing for malformed %s", tt.name)
				}
				assertSafeRealToolError(t, err, execution, root, malformed)
				assertStagingRemoved(t, stagingRoot, execution.commandDirs)
			})
		}
	})

	t.Run("truncated MP4 reaches and fails complete decode", func(t *testing.T) {
		valid, err := os.ReadFile(generated["mp4-h264-aac"])
		if err != nil {
			t.Fatal(err)
		}
		truncated := valid[:len(valid)*3/4]
		execution := &directProcessExecution{}
		_, err = newRealToolAdapter(t, stagingRoot, directProcessRunner{execution: execution}).Inspect(context.Background(), bytes.NewReader(truncated))
		if err == nil || execution.lastStage != "complete-decode" {
			t.Fatalf("complete-decode failure not observed: stage=%q code=%q private=%s", execution.lastStage, adapterErrorCode(err), execution.privateDiagnostic())
		}
		assertAdapterError(t, err, ErrorCommandFailed)
		assertSafeRealToolError(t, err, execution, root, truncated)
		assertStagingRemoved(t, stagingRoot, execution.commandDirs)
	})
}

func loadFixtureManifest(t *testing.T) fixtureManifest {
	t.Helper()
	path := filepath.Join("testdata", "media-fixtures.json")
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("fixture manifest load failure: %v", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var manifest fixtureManifest
	if err := decoder.Decode(&manifest); err != nil {
		t.Fatalf("fixture manifest decode failure: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("fixture manifest has trailing JSON: %v", err)
	}
	return manifest
}

func validateFixtureManifest(t *testing.T, manifest fixtureManifest) {
	t.Helper()
	wantIDs := []string{"animated-gif", "flac", "jpeg-misnamed-mp3", "m4a-aac", "mp3-misnamed-png", "mp4-h264-aac", "ogg-opus", "ogg-vorbis", "png", "wav-pcm-s16le", "webm-vp9-opus", "webp"}
	if manifest.SchemaVersion != 1 || strings.TrimSpace(manifest.Intent) == "" || len(manifest.Fixtures) != len(wantIDs) {
		t.Fatalf("fixture manifest identity is incomplete: schema=%d intent=%q fixtures=%d", manifest.SchemaVersion, manifest.Intent, len(manifest.Fixtures))
	}
	seen := make([]string, 0, len(manifest.Fixtures))
	for _, fixture := range manifest.Fixtures {
		seen = append(seen, fixture.ID)
		if fixture.ID == "" || filepath.Base(fixture.Filename) != fixture.Filename || len(fixture.GenerationArgs) == 0 || fixture.StreamCount <= 0 {
			t.Fatalf("fixture manifest entry is incomplete: %#v", fixture)
		}
		decodedHash, err := hex.DecodeString(fixture.SHA256)
		if err != nil || len(decodedHash) != sha256.Size {
			t.Fatalf("fixture %s SHA-256 is not exactly 32 bytes: %q", fixture.ID, fixture.SHA256)
		}
		if !fixture.Kind.Valid() || fixture.Container == "" {
			t.Fatalf("fixture %s expected identity is invalid", fixture.ID)
		}
	}
	slices.Sort(seen)
	if !slices.Equal(seen, wantIDs) {
		t.Fatalf("fixture IDs = %v, want exact matrix %v", seen, wantIDs)
	}
}

func validatePinnedTools(t *testing.T, tools fixtureTools) {
	t.Helper()
	if tools.FFmpegPath != realFFmpegPath || tools.FFprobePath != realFFprobePath || tools.FFmpegFirstLine != realFFmpegVersion || tools.FFprobeFirstLine != realFFprobeVersion {
		t.Fatalf("test-only pinned tool manifest mismatch: got %#v", tools)
	}
	for _, tool := range []struct{ path, want string }{{tools.FFmpegPath, tools.FFmpegFirstLine}, {tools.FFprobePath, tools.FFprobeFirstLine}} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		output, err := exec.CommandContext(ctx, tool.path, "-version").Output()
		cancel()
		if err != nil {
			t.Fatalf("test-only pinned tool identity failure for %s: %v", tool.path, err)
		}
		first, _, _ := strings.Cut(string(output), "\n")
		if first != tool.want {
			t.Fatalf("test-only pinned tool version mismatch for %s: got %q, want %q", tool.path, first, tool.want)
		}
	}
}

func generateFixture(t *testing.T, root, generation string, fixture fixtureManifestEntry) string {
	t.Helper()
	dir := filepath.Join(root, generation)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("fixture generation setup failure for %s: %v", fixture.ID, err)
	}
	output := filepath.Join(dir, fixture.Filename)
	args := append(slices.Clone(fixture.GenerationArgs), output)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, realFFmpegPath, args...)
	command.Env = []string{"HOME=" + dir, "LANG=C", "LC_ALL=C", "TMPDIR=" + dir}
	command.Dir = dir
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("fixture generation failure for %s (%s): %v; stderr=%q", fixture.ID, generation, err, stderr.String())
	}
	return output
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("fixture hash read failure: %v", err)
	}
	sum := sha256.Sum256(contents)
	return hex.EncodeToString(sum[:])
}

type directProcessRunner struct{ execution *directProcessExecution }

func (runner directProcessRunner) Begin(ctx context.Context, _ media.Limits) (Execution, Result) {
	runner.execution.ctx = ctx
	return runner.execution, SuccessfulResult()
}

type directProcessExecution struct {
	ctx         context.Context
	lastStage   string
	lastArgs    []string
	stderr      string
	metadata    []byte
	commandDirs []string
}

func (execution *directProcessExecution) Run(spec Command, stdout, stderr io.Writer) Result {
	execution.lastStage = commandStage(spec)
	execution.lastArgs = slices.Clone(spec.Args)
	execution.commandDirs = append(execution.commandDirs, spec.Dir)
	command := exec.CommandContext(execution.ctx, spec.Path, spec.Args...)
	command.Env = slices.Clone(spec.Env)
	command.Dir = spec.Dir
	var privateStderr bytes.Buffer
	command.Stdout = stdout
	if execution.lastStage == "probe/normalization/validation" {
		var metadata bytes.Buffer
		command.Stdout = io.MultiWriter(stdout, &metadata)
		defer func() { execution.metadata = metadata.Bytes() }()
	}
	command.Stderr = io.MultiWriter(stderr, &privateStderr)
	err := command.Run()
	execution.stderr = privateStderr.String()
	if err == nil {
		return SuccessfulResult()
	}
	if errors.Is(execution.ctx.Err(), context.DeadlineExceeded) {
		return FailedResult(FailureTimeout)
	}
	if errors.Is(execution.ctx.Err(), context.Canceled) {
		return FailedResult(FailureCancelled)
	}
	return FailedResult(FailureToolExit)
}

func (*directProcessExecution) Close() Result { return SuccessfulResult() }

func (execution *directProcessExecution) failureStage() string {
	if execution.lastStage == "complete-decode" {
		return "complete-decode failure"
	}
	return "probe/normalization/validation failure"
}

func (execution *directProcessExecution) privateDiagnostic() string {
	return fmt.Sprintf("stage=%q stderr=%q", execution.lastStage, execution.stderr)
}

func commandStage(command Command) string {
	if command.Path == realFFmpegPath {
		return "complete-decode"
	}
	if contains(command.Args, "-show_frames") {
		return "GIF all-frame probe/normalization/validation"
	}
	return "probe/normalization/validation"
}

func newRealToolAdapter(t *testing.T, stagingRoot string, runner Runner) *Adapter {
	t.Helper()
	config := Config{FFprobePath: realFFprobePath, FFmpegPath: realFFmpegPath, TempRoot: stagingRoot, Limits: media.DefaultLimits()}
	adapter, err := New(config, runner)
	if err != nil {
		t.Fatalf("real-tool adapter configuration failure: %v", err)
	}
	if adapter.config.FFprobePath != realFFprobePath || adapter.config.FFmpegPath != realFFmpegPath {
		t.Fatalf("real-tool adapter executable paths changed: %#v", adapter.config)
	}
	return adapter
}

func assertManifestInspection(t *testing.T, fixture fixtureManifestEntry, got media.Inspection, metadata []byte) {
	t.Helper()
	want := media.Inspection{
		Kind: fixture.Kind, InputBytes: got.InputBytes, StreamCount: fixture.StreamCount,
		Dimensions: media.Dimensions{Width: fixture.Width, Height: fixture.Height},
		Duration:   time.Duration(fixture.DurationNS), GIFFrameCount: fixture.FrameCount,
	}
	if fixture.Kind == media.KindGIF {
		want.Dimensions = media.Dimensions{}
		want.GIFFrameDimensions = make([]media.Dimensions, fixture.FrameCount)
		for index := range want.GIFFrameDimensions {
			want.GIFFrameDimensions[index] = media.Dimensions{Width: fixture.Width, Height: fixture.Height}
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("inspection for %s = %#v, want %#v", fixture.ID, got, want)
	}
	_, normalized, err := NormalizeProbe(metadata, got.InputBytes, media.DefaultLimits().MaxCapturedOutputBytes)
	if fixture.Kind == media.KindGIF {
		assertAdapterError(t, err, ErrorFrameMetadataRequired)
	} else if err != nil {
		t.Fatalf("manifest normalization check for %s: %v", fixture.ID, err)
	}
	if normalized.container != fixture.Container || normalized.videoCodec != fixture.VideoCodec || normalized.audioCodec != fixture.AudioCodec {
		t.Fatalf("normalized identity for %s = %#v, want %q/%q/%q", fixture.ID, normalized, fixture.Container, fixture.VideoCodec, fixture.AudioCodec)
	}
}

func assertStagingRemoved(t *testing.T, stagingRoot string, commandDirs []string) {
	t.Helper()
	for _, dir := range commandDirs {
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("temporary staging directory remains: %v", err)
		}
	}
	entries, err := os.ReadDir(stagingRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary staging root is not empty: entries=%v error=%v", entries, err)
	}
}

func assertSafeRealToolError(t *testing.T, err error, execution *directProcessExecution, root string, fixture []byte) {
	t.Helper()
	var adapterError *Error
	if !errors.As(err, &adapterError) || err.Error() != errorMessages[adapterError.Code()] {
		t.Fatalf("public error is not a fixed adapter error: %T %q", err, err)
	}
	unsafe := []string{root, strings.Join(execution.lastArgs, " "), execution.stderr, string(execution.metadata), string(fixture)}
	for _, value := range unsafe {
		if value != "" && strings.Contains(err.Error(), value) {
			t.Fatalf("public error %q contains private fixture/tool detail", err)
		}
	}
}

func adapterErrorCode(err error) ErrorCode {
	var adapterError *Error
	if errors.As(err, &adapterError) {
		return adapterError.Code()
	}
	return ""
}

func fixtureByID(t *testing.T, manifest fixtureManifest, id string) fixtureManifestEntry {
	t.Helper()
	for _, fixture := range manifest.Fixtures {
		if fixture.ID == id {
			return fixture
		}
	}
	t.Fatalf("fixture manifest is missing %s", id)
	return fixtureManifestEntry{}
}

func expectedExtension(kind media.Kind, format container) string {
	if kind == media.KindImage && format == containerJPEG {
		return ".jpg"
	}
	if kind == media.KindAudio && format == containerMP3 {
		return ".mp3"
	}
	return ""
}

func prefix(size int) func([]byte) []byte {
	return func(input []byte) []byte {
		if len(input) < size {
			return slices.Clone(input)
		}
		return slices.Clone(input[:size])
	}
}
