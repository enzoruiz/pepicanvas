package mediaexec

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"math/rand/v2"
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
	ffmpegPackage      = "ffmpeg"
	ffmpegPackageVer   = "7:6.1.1-3ubuntu5"
)

type fixtureManifest struct {
	SchemaVersion int                    `json:"schema_version"`
	Intent        string                 `json:"deterministic_generation_intent"`
	Tools         fixtureTools           `json:"tools"`
	Fixtures      []fixtureManifestEntry `json:"fixtures"`
}

type fixtureTools struct {
	FFmpegPath          string `json:"ffmpeg_path"`
	FFmpegFirstLine     string `json:"ffmpeg_first_line"`
	FFmpegDebianPackage string `json:"ffmpeg_debian_package"`
	FFmpegDebianVersion string `json:"ffmpeg_debian_version"`
	FFprobePath         string `json:"ffprobe_path"`
	FFprobeFirstLine    string `json:"ffprobe_first_line"`
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
	validateFixtureManifest(t, manifest)
	validatePinnedToolManifest(t, manifest.Tools)
	validatePinnedTools(t, manifest.Tools)

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
				t.Errorf("fixture manifest hash mismatch: %s generated %s, manifest %s; pinned package: %s=%s", fixture.ID, firstHash, fixture.SHA256, manifest.Tools.FFmpegDebianPackage, manifest.Tools.FFmpegDebianVersion)
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
		assertRejectionCode(t, err, media.RejectionDecodeFailed)
		assertSafeRealToolError(t, err, execution, root, truncated)
		assertStagingRemoved(t, stagingRoot, execution.commandDirs)
	})
}

func TestPinnedRealMediaFixtureManifest(t *testing.T) {
	manifest := loadFixtureManifest(t)
	validateFixtureManifest(t, manifest)
	validatePinnedToolManifest(t, manifest.Tools)
}

func TestPinnedRealMediaFixtureManifestUsesNativeOpusEncoder(t *testing.T) {
	manifest := loadFixtureManifest(t)
	for _, id := range []string{"ogg-opus", "webm-vp9-opus"} {
		fixture := fixtureByID(t, manifest, id)
		t.Run(id, func(t *testing.T) {
			args := fixture.GenerationArgs
			hasNativeOpusEncoder := false
			for index, arg := range args {
				if strings.Contains(arg, "libopus") {
					t.Fatalf("generation args mention external libopus encoder: %v", args)
				}
				if arg == "-c:a" && index+1 < len(args) && args[index+1] == "opus" {
					hasNativeOpusEncoder = true
				}
			}
			if !hasNativeOpusEncoder {
				t.Fatalf("generation args do not select FFmpeg native opus encoder: %v", args)
			}
		})
	}
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

func validatePinnedToolManifest(t *testing.T, tools fixtureTools) {
	t.Helper()
	if tools.FFmpegPath != realFFmpegPath || tools.FFprobePath != realFFprobePath || tools.FFmpegFirstLine != realFFmpegVersion || tools.FFprobeFirstLine != realFFprobeVersion ||
		tools.FFmpegDebianPackage != ffmpegPackage || tools.FFmpegDebianVersion != ffmpegPackageVer {
		t.Fatalf("test-only pinned tool manifest mismatch: got %#v", tools)
	}
}

func validatePinnedTools(t *testing.T, tools fixtureTools) {
	t.Helper()
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	output, err := exec.CommandContext(ctx, "/usr/bin/dpkg-query", "-W", "-f=${Version}", tools.FFmpegDebianPackage).Output()
	cancel()
	if err != nil {
		t.Fatalf("test-only pinned package identity failure for %s=%s: %v", tools.FFmpegDebianPackage, tools.FFmpegDebianVersion, err)
	}
	if string(output) != tools.FFmpegDebianVersion {
		t.Fatalf("test-only pinned package version mismatch for %s: got %q, want %q", tools.FFmpegDebianPackage, string(output), tools.FFmpegDebianVersion)
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
		Kind: fixture.Kind, MediaType: expectedMediaType(fixture), InputBytes: got.InputBytes, StreamCount: fixture.StreamCount,
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
	var rejection *media.Rejection
	switch {
	case errors.As(err, &rejection):
		if err.Error() != media.Reject(rejection.Code()).Error() {
			t.Fatalf("public error is not a fixed rejection: %q", err)
		}
	case !errors.As(err, &adapterError) || err.Error() != errorMessages[adapterError.Code()]:
		t.Fatalf("public error is not a fixed adapter error: %T %q", err, err)
	}
	unsafe := []string{root, strings.Join(execution.lastArgs, " "), execution.stderr, string(execution.metadata), string(fixture)}
	for _, value := range unsafe {
		if value != "" && strings.Contains(err.Error(), value) {
			t.Fatalf("public error %q contains private fixture/tool detail", err)
		}
	}
}

func adapterErrorCode(err error) string {
	var adapterError *Error
	if errors.As(err, &adapterError) {
		return string(adapterError.Code())
	}
	var rejection *media.Rejection
	if errors.As(err, &rejection) {
		return string(rejection.Code())
	}
	return ""
}

func expectedMediaType(fixture fixtureManifestEntry) string {
	switch fixture.Container {
	case containerJPEG:
		return "image/jpeg"
	case containerPNG:
		return "image/png"
	case containerWebP:
		return "image/webp"
	case containerGIF:
		return "image/gif"
	case containerMP3:
		return "audio/mpeg"
	case containerWAV:
		return "audio/wav"
	case containerFLAC:
		return "audio/flac"
	case containerOgg:
		return "audio/ogg"
	case containerISOBaseMedia:
		if fixture.Kind == media.KindAudio {
			return "audio/mp4"
		}
		return "video/mp4"
	case containerWebM:
		return "video/webm"
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

// TestRealToolsImageLimits inspects generated PNG, JPEG, WebP and GIF samples
// at the 10 000 000-byte and 4096-pixel boundaries, an SVG, truncated samples
// and a GIF with one oversized frame, with the real tools inside a delegated
// cgroup.
func TestRealToolsImageLimits(t *testing.T) {
	root := os.Getenv("PEPICANVAS_TEST_CGROUP_ROOT")
	if root == "" {
		t.Skip("PEPICANVAS_TEST_CGROUP_ROOT is not set to a disposable delegated cgroup")
	}
	runner, result := NewCgroupRunner(root)
	if !result.succeeded() {
		t.Fatalf("NewCgroupRunner() = %#v", result)
	}
	samples := t.TempDir()
	staging := t.TempDir()
	adapter := newRealToolAdapter(t, staging, runner)

	type accepted struct {
		mediaType string
		size      media.Dimensions
		frames    int
	}
	tests := []struct {
		name   string
		sample func(*testing.T) []byte
		want   accepted
		reject media.RejectionCode
	}{
		{name: "PNG 4096x4096", sample: pngSample(4096, 4096), want: accepted{mediaType: "image/png", size: media.Dimensions{Width: 4096, Height: 4096}}},
		{name: "PNG width 4097", sample: pngSample(4097, 4096), reject: media.RejectionDimensionsExceeded},
		{name: "PNG height 4097", sample: pngSample(4096, 4097), reject: media.RejectionDimensionsExceeded},
		{name: "JPEG 4096x4096", sample: jpegSample(4096, 4096), want: accepted{mediaType: "image/jpeg", size: media.Dimensions{Width: 4096, Height: 4096}}},
		{name: "JPEG width 4097", sample: jpegSample(4097, 4096), reject: media.RejectionDimensionsExceeded},
		{name: "JPEG height 4097", sample: jpegSample(4096, 4097), reject: media.RejectionDimensionsExceeded},
		{name: "WebP 4096x4096", sample: webpSample(samples, 4096, 4096), want: accepted{mediaType: "image/webp", size: media.Dimensions{Width: 4096, Height: 4096}}},
		{name: "WebP width 4097", sample: webpSample(samples, 4097, 4096), reject: media.RejectionDimensionsExceeded},
		{name: "WebP height 4097", sample: webpSample(samples, 4096, 4097), reject: media.RejectionDimensionsExceeded},
		{name: "GIF 4096x4096", sample: gifSample(4096, 4096, 2), want: accepted{mediaType: "image/gif", size: media.Dimensions{Width: 4096, Height: 4096}, frames: 2}},
		{name: "GIF width 4097", sample: gifSample(4097, 4096, 2), reject: media.RejectionDimensionsExceeded},
		{name: "GIF height 4097", sample: gifSample(16, 4097, 2), reject: media.RejectionDimensionsExceeded},
		{name: "GIF with one frame wider than 4096", sample: gifOneWideFrame(4097), reject: media.RejectionDimensionsExceeded},
		// ffmpeg clips the frame to the 16x16 screen and only warns.
		{name: "GIF frame declared wider than its screen", sample: gifPatchedFrame(1, frameDescriptor{width: 5000, height: 16}), reject: media.RejectionInvalidMetadata},
		{name: "GIF of exactly 10000000 bytes", sample: paddedGIF(10_000_000), want: accepted{mediaType: "image/gif", size: media.Dimensions{Width: 16, Height: 16}, frames: 2}},
		{name: "GIF of 10000001 bytes", sample: paddedGIF(10_000_001), reject: media.RejectionInputTooLarge},
		{name: "PNG of exactly 10000000 bytes", sample: paddedPNG(10_000_000), want: accepted{mediaType: "image/png", size: media.Dimensions{Width: 64, Height: 48}}},
		{name: "PNG of 10000001 bytes", sample: paddedPNG(10_000_001), reject: media.RejectionInputTooLarge},
		{name: "SVG", sample: func(*testing.T) []byte { return []byte(svgSample) }, reject: media.RejectionUnsupportedKind},
		// The PNG header and IHDR survive, so ffprobe reports a valid image and
		// only the complete decode finds the missing image data.
		{name: "truncated PNG", sample: truncated(noisePNG(512, 512)), reject: media.RejectionDecodeFailed},
		// ffprobe counts the frames present, the partial one included, so the
		// counts agree; ffmpeg logs "LZW decode failed" yet exits 0, and only
		// that error output reveals the failed decode.
		{name: "truncated GIF", sample: truncated(noiseGIF(256, 256, 3)), reject: media.RejectionDecodeFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			contents := tt.sample(t)
			inspection, err := adapter.Inspect(context.Background(), bytes.NewReader(contents))
			assertStagingRemoved(t, staging, nil)
			if tt.reject != "" {
				assertRejectionCode(t, err, tt.reject)
				if err.Error() != media.Reject(tt.reject).Error() {
					t.Fatalf("public error = %q, want the fixed rejection message", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Inspect() error = %v (%s), want nil", err, adapterErrorCode(err))
			}
			if inspection.MediaType != tt.want.mediaType || inspection.InputBytes != int64(len(contents)) {
				t.Fatalf("inspection = %s/%d bytes, want %s/%d bytes", inspection.MediaType, inspection.InputBytes, tt.want.mediaType, len(contents))
			}
			if tt.want.frames == 0 {
				if inspection.Kind != media.KindImage || inspection.Dimensions != tt.want.size {
					t.Fatalf("inspection = %s %+v, want image %+v", inspection.Kind, inspection.Dimensions, tt.want.size)
				}
				return
			}
			if inspection.Kind != media.KindGIF || inspection.GIFFrameCount != tt.want.frames || len(inspection.GIFFrameDimensions) != tt.want.frames {
				t.Fatalf("inspection = %s with %d frames, want GIF with %d frames", inspection.Kind, inspection.GIFFrameCount, tt.want.frames)
			}
			for _, frame := range inspection.GIFFrameDimensions {
				if frame != tt.want.size {
					t.Fatalf("GIF frame = %+v, want %+v", frame, tt.want.size)
				}
			}
		})
	}
}

const svgSample = `<svg xmlns="http://www.w3.org/2000/svg" width="64" height="48"><rect width="64" height="48" fill="red"/></svg>`

func pngSample(width, height int) func(*testing.T) []byte {
	return func(t *testing.T) []byte {
		return encodePNG(t, image.NewGray(image.Rect(0, 0, width, height)))
	}
}

func noisePNG(width, height int) func(*testing.T) []byte {
	return func(t *testing.T) []byte {
		img := image.NewGray(image.Rect(0, 0, width, height))
		random := rand.New(rand.NewPCG(1, 2))
		for index := range img.Pix {
			img.Pix[index] = uint8(random.Uint32())
		}
		return encodePNG(t, img)
	}
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatalf("encode PNG sample: %v", err)
	}
	return encoded.Bytes()
}

// paddedPNG grows a valid 64x48 PNG to exactly size bytes with a private
// ancillary chunk before IEND, which decoders skip.
func paddedPNG(size int) func(*testing.T) []byte {
	return func(t *testing.T) []byte {
		valid := encodePNG(t, image.NewGray(image.Rect(0, 0, 64, 48)))
		const iendLength = 12
		const chunkOverhead = 12
		padding := size - len(valid) - chunkOverhead
		if padding < 0 {
			t.Fatalf("PNG sample of %d bytes exceeds %d", len(valid), size)
		}
		chunk := make([]byte, chunkOverhead+padding)
		binary.BigEndian.PutUint32(chunk, uint32(padding))
		copy(chunk[4:], "prIv")
		binary.BigEndian.PutUint32(chunk[8+padding:], crc32.ChecksumIEEE(chunk[4:8+padding]))
		end := len(valid) - iendLength
		padded := append(append(append([]byte(nil), valid[:end]...), chunk...), valid[end:]...)
		if len(padded) != size {
			t.Fatalf("padded PNG = %d bytes, want %d", len(padded), size)
		}
		return padded
	}
}

// paddedGIF grows a valid 16x16 two-frame GIF to exactly size bytes with a
// comment extension before the trailer, which decoders skip.
func paddedGIF(size int) func(*testing.T) []byte {
	return func(t *testing.T) []byte {
		valid := gifSample(16, 16, 2)(t)
		need := size - len(valid) - 3
		extension := []byte{0x21, 0xFE}
		for need > 0 {
			length := min(255, need-1)
			if length <= 0 {
				t.Fatalf("cannot pad a GIF to %d bytes", size)
			}
			extension = append(extension, byte(length))
			extension = append(extension, bytes.Repeat([]byte{'x'}, length)...)
			need -= length + 1
		}
		extension = append(extension, 0x00)
		padded := append(append(append([]byte(nil), valid[:len(valid)-1]...), extension...), 0x3B)
		if len(padded) != size {
			t.Fatalf("padded GIF = %d bytes, want %d", len(padded), size)
		}
		return padded
	}
}

func jpegSample(width, height int) func(*testing.T) []byte {
	return func(t *testing.T) []byte {
		var encoded bytes.Buffer
		if err := jpeg.Encode(&encoded, image.NewGray(image.Rect(0, 0, width, height)), nil); err != nil {
			t.Fatalf("encode JPEG sample: %v", err)
		}
		return encoded.Bytes()
	}
}

// webpSample encodes a Go PNG with ffmpeg: its lavfi color source renders a
// 4097-pixel axis as 4096.
func webpSample(dir string, width, height int) func(*testing.T) []byte {
	return func(t *testing.T) []byte {
		input := filepath.Join(dir, fmt.Sprintf("sample-%dx%d.png", width, height))
		if err := os.WriteFile(input, pngSample(width, height)(t), 0o600); err != nil {
			t.Fatal(err)
		}
		output := filepath.Join(dir, fmt.Sprintf("sample-%dx%d.webp", width, height))
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		command := exec.CommandContext(ctx, realFFmpegPath, "-hide_banner", "-v", "error", "-nostdin", "-y",
			"-i", input, "-frames:v", "1", "-c:v", "libwebp", output)
		command.Env = []string{"HOME=" + dir, "LANG=C", "LC_ALL=C", "TMPDIR=" + dir}
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatalf("generate WebP sample: %v: %s", err, out)
		}
		contents, err := os.ReadFile(output)
		if err != nil {
			t.Fatal(err)
		}
		return contents
	}
}

var gifPalette = color.Palette{color.Black, color.White}

func gifSample(width, height, frames int) func(*testing.T) []byte {
	return func(t *testing.T) []byte {
		animation := &gif.GIF{}
		for range frames {
			animation.Image = append(animation.Image, image.NewPaletted(image.Rect(0, 0, width, height), gifPalette))
			animation.Delay = append(animation.Delay, 10)
		}
		return encodeGIF(t, animation)
	}
}

// gifOneWideFrame is a two-frame GIF whose second frame is width pixels wide
// while the first one is 16x16.
func gifOneWideFrame(width int) func(*testing.T) []byte {
	return func(t *testing.T) []byte {
		return encodeGIF(t, &gif.GIF{
			Image: []*image.Paletted{
				image.NewPaletted(image.Rect(0, 0, 16, 16), gifPalette),
				image.NewPaletted(image.Rect(0, 0, width, 16), gifPalette),
			},
			Delay:  []int{10, 10},
			Config: image.Config{ColorModel: gifPalette, Width: width, Height: 16},
		})
	}
}

func noiseGIF(width, height, frames int) func(*testing.T) []byte {
	return func(t *testing.T) []byte {
		random := rand.New(rand.NewPCG(3, 4))
		animation := &gif.GIF{}
		for range frames {
			frame := image.NewPaletted(image.Rect(0, 0, width, height), gifPalette)
			for index := range frame.Pix {
				frame.Pix[index] = uint8(random.IntN(2))
			}
			animation.Image = append(animation.Image, frame)
			animation.Delay = append(animation.Delay, 10)
		}
		return encodeGIF(t, animation)
	}
}

func encodeGIF(t *testing.T, animation *gif.GIF) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := gif.EncodeAll(&encoded, animation); err != nil {
		t.Fatalf("encode GIF sample: %v", err)
	}
	return encoded.Bytes()
}

func truncated(sample func(*testing.T) []byte) func(*testing.T) []byte {
	return func(t *testing.T) []byte {
		valid := sample(t)
		return valid[:len(valid)/2]
	}
}
