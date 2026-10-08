package mediaexec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/media"
)

type runnerFunc func(context.Context, Command, io.Writer, io.Writer) error

func (run runnerFunc) Begin(ctx context.Context, _ media.Limits) (Execution, Result) {
	return &runnerExecution{ctx: ctx, run: run}, SuccessfulResult()
}

type runnerExecution struct {
	ctx context.Context
	run runnerFunc
}

func (execution *runnerExecution) Run(command Command, stdout, stderr io.Writer) Result {
	if execution.run(execution.ctx, command, stdout, stderr) != nil {
		return FailedResult(FailureToolExit)
	}
	return SuccessfulResult()
}

func (*runnerExecution) Close() Result { return SuccessfulResult() }

type pointerRunner struct{}

func (*pointerRunner) Begin(context.Context, media.Limits) (Execution, Result) {
	return nil, SuccessfulResult()
}

type mapRunner map[string]string

func (mapRunner) Begin(context.Context, media.Limits) (Execution, Result) {
	return nil, SuccessfulResult()
}

type sliceRunner []string

func (sliceRunner) Begin(context.Context, media.Limits) (Execution, Result) {
	return nil, SuccessfulResult()
}

func TestNewAdapterFailsClosedForInvalidConfigurationOrMissingRunner(t *testing.T) {
	t.Parallel()

	valid := testConfig(t)
	tests := []struct {
		name   string
		mutate func(*Config)
		runner Runner
	}{
		{name: "missing runner", runner: nil},
		{name: "typed nil pointer runner", runner: (*pointerRunner)(nil)},
		{name: "typed nil function runner", runner: runnerFunc(nil)},
		{name: "typed nil map runner", runner: mapRunner(nil)},
		{name: "typed nil slice runner", runner: sliceRunner(nil)},
		{name: "relative ffprobe", mutate: func(c *Config) { c.FFprobePath = "ffprobe" }, runner: runnerFunc(noopRun)},
		{name: "relative ffmpeg", mutate: func(c *Config) { c.FFmpegPath = "ffmpeg" }, runner: runnerFunc(noopRun)},
		{name: "NUL ffprobe", mutate: func(c *Config) { c.FFprobePath += "\x00suffix" }, runner: runnerFunc(noopRun)},
		{name: "missing ffprobe", mutate: func(c *Config) { c.FFprobePath = filepath.Join(c.TempRoot, "missing") }, runner: runnerFunc(noopRun)},
		{name: "directory ffprobe", mutate: func(c *Config) { c.FFprobePath = c.TempRoot }, runner: runnerFunc(noopRun)},
		{name: "non-executable ffprobe", mutate: func(c *Config) { c.FFprobePath = createStub(t, c.TempRoot, "not-executable", 0o600) }, runner: runnerFunc(noopRun)},
		{name: "missing ffmpeg", mutate: func(c *Config) { c.FFmpegPath = filepath.Join(c.TempRoot, "missing") }, runner: runnerFunc(noopRun)},
		{name: "directory ffmpeg", mutate: func(c *Config) { c.FFmpegPath = c.TempRoot }, runner: runnerFunc(noopRun)},
		{name: "non-executable ffmpeg", mutate: func(c *Config) { c.FFmpegPath = createStub(t, c.TempRoot, "not-executable-ffmpeg", 0o600) }, runner: runnerFunc(noopRun)},
		{name: "relative temp root", mutate: func(c *Config) { c.TempRoot = "tmp" }, runner: runnerFunc(noopRun)},
		{name: "NUL temp root", mutate: func(c *Config) { c.TempRoot += "\x00suffix" }, runner: runnerFunc(noopRun)},
		{name: "missing temp root", mutate: func(c *Config) { c.TempRoot = filepath.Join(c.TempRoot, "missing") }, runner: runnerFunc(noopRun)},
		{name: "file temp root", mutate: func(c *Config) { c.TempRoot = c.FFprobePath }, runner: runnerFunc(noopRun)},
		{name: "root temp directory", mutate: func(c *Config) { c.TempRoot = string(filepath.Separator) }, runner: runnerFunc(noopRun)},
		{name: "zero output budget", mutate: func(c *Config) { c.Limits.MaxCapturedOutputBytes = 0 }, runner: runnerFunc(noopRun)},
		{name: "excessive output budget", mutate: func(c *Config) { c.Limits.MaxCapturedOutputBytes = maxCapturedOutputBytes + 1 }, runner: runnerFunc(noopRun)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := valid
			if tt.mutate != nil {
				tt.mutate(&config)
			}
			_, err := New(config, tt.runner)
			assertAdapterError(t, err, ErrorConfiguration)
		})
	}
}

func TestNewAdapterCanonicalizesTrustedPaths(t *testing.T) {
	t.Parallel()

	config := testConfig(t)
	links := t.TempDir()
	probeLink := filepath.Join(links, "ffprobe")
	ffmpegLink := filepath.Join(links, "ffmpeg")
	rootLink := filepath.Join(links, "root")
	for link, target := range map[string]string{
		probeLink:  config.FFprobePath,
		ffmpegLink: config.FFmpegPath,
		rootLink:   config.TempRoot,
	} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	config.FFprobePath = probeLink
	config.FFmpegPath = ffmpegLink
	config.TempRoot = rootLink

	adapter, err := New(config, runnerFunc(noopRun))
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	if adapter.config.FFprobePath == probeLink || adapter.config.FFmpegPath == ffmpegLink || adapter.config.TempRoot == rootLink {
		t.Fatalf("canonical config = %#v, still contains symlink paths", adapter.config)
	}
	want := testConfigPaths(t, config.FFprobePath, config.FFmpegPath, config.TempRoot)
	if adapter.config.FFprobePath != want.FFprobePath || adapter.config.FFmpegPath != want.FFmpegPath || adapter.config.TempRoot != want.TempRoot {
		t.Fatalf("canonical paths = %q, %q, %q; want %q, %q, %q", adapter.config.FFprobePath, adapter.config.FFmpegPath, adapter.config.TempRoot, want.FFprobePath, want.FFmpegPath, want.TempRoot)
	}
}

func TestBoundedReadCapacityAvoidsLimitOverflow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		limit    int64
		written  int64
		capacity int
		want     int
	}{
		{name: "maximum limit starts with full buffer", limit: math.MaxInt64, capacity: 32 * 1024, want: 32 * 1024},
		{name: "maximum limit near boundary includes probe byte", limit: math.MaxInt64, written: math.MaxInt64 - 7, capacity: 32 * 1024, want: 8},
		{name: "maximum limit at boundary probes one byte", limit: math.MaxInt64, written: math.MaxInt64, capacity: 32 * 1024, want: 1},
		{name: "small limit includes probe byte", limit: 7, capacity: 32 * 1024, want: 8},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := boundedReadCapacity(tt.limit, tt.written, tt.capacity); got != tt.want {
				t.Fatalf("boundedReadCapacity(%d, %d, %d) = %d, want %d", tt.limit, tt.written, tt.capacity, got, tt.want)
			}
		})
	}
}

func TestInspectCommandsAreIsolatedFromRunnerMutation(t *testing.T) {
	t.Parallel()

	config := testConfig(t)
	inspection := 0
	var secondInspection []Command
	adapter, err := New(config, runnerFunc(func(_ context.Context, command Command, stdout, _ io.Writer) error {
		if inspection == 0 {
			command.Args[0] = "mutated-argument"
			command.Env[0] = "MUTATED=1"
		} else {
			secondInspection = append(secondInspection, command)
		}
		if command.Path == config.FFprobePath {
			_, _ = io.WriteString(stdout, imageProbe())
		}
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Inspect(context.Background(), strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	inspection++
	if _, err := adapter.Inspect(context.Background(), strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if len(secondInspection) != 2 {
		t.Fatalf("second inspection commands = %d, want 2", len(secondInspection))
	}
	for _, command := range secondInspection {
		if command.Args[0] != "-hide_banner" || command.Env[0] != "HOME="+command.Dir {
			t.Fatalf("later command retained mutation: %#v", command)
		}
	}
}

func TestInspectStagesPrivatelyAndAlwaysCleansUp(t *testing.T) {
	t.Parallel()

	config := testConfig(t)
	var stagedDir string
	runner := runnerFunc(func(_ context.Context, command Command, stdout, _ io.Writer) error {
		if stagedDir == "" {
			stagedDir = command.Dir
			assertMode(t, command.Dir, 0o700)
			input := command.Args[len(command.Args)-1]
			assertMode(t, input, 0o600)
			contents, err := os.ReadFile(input)
			if err != nil || string(contents) != "trusted bytes" {
				t.Fatalf("staged input = %q, %v", contents, err)
			}
			_, _ = io.WriteString(stdout, imageProbe())
		}
		return nil
	})
	adapter, err := New(config, runner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Inspect(context.Background(), strings.NewReader("trusted bytes")); err != nil {
		t.Fatalf("Inspect() error = %v, want nil", err)
	}
	if _, err := os.Stat(stagedDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging directory remains: %v", err)
	}
}

func TestInspectRejectsStagingOverflowBeforeRunningCommands(t *testing.T) {
	t.Parallel()

	config := testConfig(t)
	config.Limits.MaxImageBytes = 3
	config.Limits.MaxGIFBytes = 4
	config.Limits.MaxAudioBytes = 5
	config.Limits.MaxVideoBytes = 6
	calls := 0
	adapter, err := New(config, runnerFunc(func(context.Context, Command, io.Writer, io.Writer) error {
		calls++
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = adapter.Inspect(context.Background(), strings.NewReader("1234567"))
	assertAdapterError(t, err, ErrorInputTooLarge)
	if calls != 0 {
		t.Fatalf("runner calls = %d, want 0", calls)
	}
	entries, readErr := os.ReadDir(config.TempRoot)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("temp root entries = %v, %v, want empty after overflow", entries, readErr)
	}
}

func TestInspectRejectsReaderFailureAndCleansStaging(t *testing.T) {
	t.Parallel()

	config := testConfig(t)
	adapter, err := New(config, runnerFunc(noopRun))
	if err != nil {
		t.Fatal(err)
	}
	_, err = adapter.Inspect(context.Background(), failingReader{})
	assertAdapterError(t, err, ErrorStagingFailed)
	entries, readErr := os.ReadDir(config.TempRoot)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("temp root entries = %v, %v, want empty after reader failure", entries, readErr)
	}
}

func TestInspectOrchestratesApprovedKindsAndCompleteDecode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		metadata  string
		frames    string
		want      media.Inspection
		wantCalls int
	}{
		{name: "image", metadata: imageProbe(), want: media.Inspection{Kind: media.KindImage, InputBytes: 1, StreamCount: 1, Dimensions: media.Dimensions{Width: 10, Height: 20}}, wantCalls: 2},
		{name: "audio", metadata: audioProbe(), want: media.Inspection{Kind: media.KindAudio, InputBytes: 1, StreamCount: 1, Duration: time.Second}, wantCalls: 2},
		{name: "video", metadata: videoProbeJSON(), want: media.Inspection{Kind: media.KindVideo, InputBytes: 1, StreamCount: 1, Dimensions: media.Dimensions{Width: 10, Height: 20}, Duration: time.Second}, wantCalls: 2},
		{name: "GIF all frames", metadata: gifProbe(), frames: `{"frames":[{"width":10,"height":20},{"width":30,"height":40}]}`, want: media.Inspection{Kind: media.KindGIF, InputBytes: 1, StreamCount: 1, Duration: time.Second, GIFFrameCount: 2, GIFFrameDimensions: []media.Dimensions{{Width: 10, Height: 20}, {Width: 30, Height: 40}}}, wantCalls: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			config := testConfig(t)
			calls := 0
			runner := runnerFunc(func(_ context.Context, command Command, stdout, _ io.Writer) error {
				calls++
				switch {
				case command.Path == config.FFprobePath && contains(command.Args, "-show_frames"):
					_, _ = io.WriteString(stdout, tt.frames)
				case command.Path == config.FFprobePath:
					_, _ = io.WriteString(stdout, tt.metadata)
				case command.Path != config.FFmpegPath:
					t.Fatalf("unexpected command: %#v", command)
				}
				return nil
			})
			adapter, err := New(config, runner)
			if err != nil {
				t.Fatal(err)
			}
			got, err := adapter.Inspect(context.Background(), strings.NewReader("x"))
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Inspect() = %#v, %v, want %#v, nil", got, err, tt.want)
			}
			if calls != tt.wantCalls {
				t.Fatalf("runner calls = %d, want %d", calls, tt.wantCalls)
			}
		})
	}
}

func TestInspectValidatesBeforeDecodeAndRejectsGIFCountMismatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		metadata string
		frames   string
		code     ErrorCode
		calls    int
	}{
		{name: "domain validation precedes decode", metadata: strings.Replace(imageProbe(), `"width":10`, `"width":11`, 1), code: ErrorValidation, calls: 1},
		{name: "GIF frame count mismatch", metadata: gifProbe(), frames: `{"frames":[{"width":10,"height":20}]}`, code: ErrorInvalidFrames, calls: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := testConfig(t)
			config.Limits.MaxWidth = 10
			calls := 0
			adapter, err := New(config, runnerFunc(func(_ context.Context, command Command, stdout, _ io.Writer) error {
				calls++
				if contains(command.Args, "-show_frames") {
					_, _ = io.WriteString(stdout, tt.frames)
				} else if command.Path == config.FFprobePath {
					_, _ = io.WriteString(stdout, tt.metadata)
				} else {
					t.Fatal("decode must not run")
				}
				return nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			_, err = adapter.Inspect(context.Background(), strings.NewReader("x"))
			assertAdapterError(t, err, tt.code)
			if calls != tt.calls {
				t.Fatalf("runner calls = %d, want %d", calls, tt.calls)
			}
		})
	}
}

func TestInspectUsesOneDeadlineAcrossEveryCommand(t *testing.T) {
	t.Parallel()

	config := testConfig(t)
	var deadlines []time.Time
	adapter, err := New(config, runnerFunc(func(ctx context.Context, command Command, stdout, _ io.Writer) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("runner context has no deadline")
		}
		deadlines = append(deadlines, deadline)
		if command.Path == config.FFprobePath {
			_, _ = io.WriteString(stdout, imageProbe())
		}
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Inspect(context.Background(), strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if len(deadlines) != 2 || !deadlines[0].Equal(deadlines[1]) {
		t.Fatalf("deadlines = %#v, want two equal inspection-wide deadlines", deadlines)
	}
}

func TestInspectRejectsFailedCompleteDecodeWithoutPartialAcceptance(t *testing.T) {
	t.Parallel()

	config := testConfig(t)
	var stagedDir string
	adapter, err := New(config, runnerFunc(func(_ context.Context, command Command, stdout, _ io.Writer) error {
		stagedDir = command.Dir
		if command.Path == config.FFprobePath {
			_, _ = io.WriteString(stdout, imageProbe())
			return nil
		}
		return errors.New("private decode detail")
	}))
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := adapter.Inspect(context.Background(), strings.NewReader("x"))
	assertAdapterError(t, err, ErrorCommandFailed)
	if !reflect.DeepEqual(inspection, media.Inspection{}) {
		t.Fatalf("inspection = %#v, want zero value after failed decode", inspection)
	}
	if _, statErr := os.Stat(stagedDir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("staging directory remains after failed decode: %v", statErr)
	}
}

func TestInspectHelperProcessTimeoutAndFloodingOutput(t *testing.T) {
	if os.Getenv("GO_WANT_MEDIAEXEC_HELPER") == "1" {
		helperProcess()
		return
	}

	tests := []struct {
		name    string
		mode    string
		budget  int64
		timeout time.Duration
		code    ErrorCode
	}{
		{name: "timeout", mode: "sleep", budget: 1024, timeout: 20 * time.Millisecond, code: ErrorTimeout},
		{name: "aggregate flood", mode: "flood", budget: 16, timeout: time.Second, code: ErrorOutputOverflow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := testConfig(t)
			config.Limits.MaxCapturedOutputBytes = tt.budget
			config.Limits.InspectionTimeout = tt.timeout
			adapter, err := New(config, processRunner{mode: tt.mode})
			if err != nil {
				t.Fatal(err)
			}
			_, err = adapter.Inspect(context.Background(), strings.NewReader("x"))
			assertAdapterError(t, err, tt.code)
		})
	}
}

func TestInspectReturnsFixedSafeErrors(t *testing.T) {
	t.Parallel()

	config := testConfig(t)
	unsafe := "private-path secret stderr bytes"
	adapter, err := New(config, runnerFunc(func(context.Context, Command, io.Writer, io.Writer) error {
		return fmt.Errorf("%s", unsafe)
	}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = adapter.Inspect(context.Background(), strings.NewReader("x"))
	assertAdapterError(t, err, ErrorCommandFailed)
	if strings.Contains(err.Error(), unsafe) || len(err.Error()) > 64 {
		t.Fatalf("public error = %q, want bounded fixed safe text", err)
	}
}

type processRunner struct{ mode string }

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("private reader detail") }

func (runner processRunner) Begin(ctx context.Context, _ media.Limits) (Execution, Result) {
	return &processExecution{ctx: ctx, mode: runner.mode}, SuccessfulResult()
}

type processExecution struct {
	ctx  context.Context
	mode string
}

func (execution *processExecution) Run(_ Command, stdout, stderr io.Writer) Result {
	command := exec.CommandContext(execution.ctx, os.Args[0], "-test.run=TestInspectHelperProcessTimeoutAndFloodingOutput")
	command.Env = []string{"GO_WANT_MEDIAEXEC_HELPER=1", "MEDIAEXEC_HELPER_MODE=" + execution.mode}
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		if execution.ctx.Err() != nil {
			return FailedResult(FailureTimeout)
		}
		return FailedResult(FailureToolExit)
	}
	return SuccessfulResult()
}

func (*processExecution) Close() Result { return SuccessfulResult() }

func helperProcess() {
	switch os.Getenv("MEDIAEXEC_HELPER_MODE") {
	case "sleep":
		time.Sleep(time.Second)
	case "flood":
		_, _ = os.Stdout.Write([]byte(strings.Repeat("o", 64)))
		_, _ = os.Stderr.Write([]byte(strings.Repeat("e", 64)))
	}
	os.Exit(0)
}

func testConfig(t *testing.T) Config {
	t.Helper()
	limits := media.DefaultLimits()
	root := t.TempDir()
	staging := filepath.Join(root, "staging")
	if err := os.Mkdir(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	return Config{
		FFprobePath: createStub(t, root, "ffprobe", 0o700),
		FFmpegPath:  createStub(t, root, "ffmpeg", 0o700),
		TempRoot:    staging,
		Limits:      limits,
	}
}

func createStub(t *testing.T, root, name string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte("controlled test stub"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func testConfigPaths(t *testing.T, paths ...string) Config {
	t.Helper()
	canonical := make([]string, len(paths))
	for index, path := range paths {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatal(err)
		}
		canonical[index] = resolved
	}
	return Config{FFprobePath: canonical[0], FFmpegPath: canonical[1], TempRoot: canonical[2]}
}

func noopRun(context.Context, Command, io.Writer, io.Writer) error { return nil }

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	if info.Mode().Perm() != want {
		t.Fatalf("mode for %q = %v, want %v", path, info.Mode().Perm(), want)
	}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func imageProbe() string {
	return `{"format":{"format_name":"png_pipe"},"streams":[{"codec_type":"video","codec_name":"png","width":10,"height":20}]}`
}

func audioProbe() string {
	return `{"format":{"format_name":"mp3","duration":"1"},"streams":[{"codec_type":"audio","codec_name":"mp3","duration":"1"}]}`
}

func videoProbeJSON() string {
	return `{"format":{"format_name":"matroska,webm","duration":"1"},"streams":[{"codec_type":"video","codec_name":"vp9","width":10,"height":20,"duration":"1"}]}`
}

func gifProbe() string {
	return `{"format":{"format_name":"gif","duration":"1"},"streams":[{"codec_type":"video","codec_name":"gif","width":10,"height":20,"duration":"1","nb_frames":"2"}]}`
}
