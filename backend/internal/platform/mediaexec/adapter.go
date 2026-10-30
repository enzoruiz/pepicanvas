package mediaexec

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/enzoruiz/pepicanvas/backend/internal/media"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/telemetry"
)

const maxCapturedOutputBytes = int64(16 * 1024 * 1024)

// Config contains only deployment-trusted paths and fixed inspection limits.
type Config struct {
	FFprobePath string
	FFmpegPath  string
	TempRoot    string
	Limits      media.Limits
}

// Adapter stages and inspects media through a narrow injected Runner.
type Adapter struct {
	config Config
	runner Runner
}

var _ media.Inspector = (*Adapter)(nil)

// New validates trusted configuration and fails closed without a Runner.
func New(config Config, runner Runner) (*Adapter, error) {
	if nilInterface(runner) || config.Limits.Validate() != nil ||
		config.Limits.MaxCapturedOutputBytes > maxCapturedOutputBytes {
		return nil, newError(ErrorConfiguration)
	}
	ffprobe, ok := canonicalExecutable(config.FFprobePath)
	if !ok {
		return nil, newError(ErrorConfiguration)
	}
	ffmpeg, ok := canonicalExecutable(config.FFmpegPath)
	if !ok {
		return nil, newError(ErrorConfiguration)
	}
	tempRoot, ok := canonicalTempRoot(config.TempRoot)
	if !ok {
		return nil, newError(ErrorConfiguration)
	}
	config.FFprobePath = ffprobe
	config.FFmpegPath = ffmpeg
	config.TempRoot = tempRoot
	return &Adapter{config: config, runner: runner}, nil
}

// Inspect consumes one untrusted byte stream. Context is checked between source
// reads; a Reader that can block indefinitely must be made cancelable by its
// owner. One deadline and one output budget cover staging and every command.
// A failure the content causes is a *media.Rejection naming the broken rule;
// timeout, cancellation, containment, staging I/O, output overflow, cleanup
// and execution failures are an *Error.
// The inspection runs in a media.inspect span and each command in a
// subprocess span of the caller's trace; the commands' environment is left
// unchanged, since the tools cannot consume a trace context.
func (adapter *Adapter) Inspect(parent context.Context, source io.Reader) (inspection media.Inspection, resultErr error) {
	parent, span := telemetry.Start(parent, telemetry.SpanMediaInspect)
	// Deferred first, so it ends the span with the final error of the
	// cleanup below.
	defer func() { endSpan(span, resultErr) }()
	ctx, cancel := context.WithTimeout(parent, adapter.config.Limits.InspectionTimeout)
	defer cancel()
	rawExecution, beginResult := adapter.runner.Begin(ctx, adapter.config.Limits)
	execution, err := beginExecution(rawExecution, beginResult)
	if err != nil {
		return media.Inspection{}, err
	}
	defer func() {
		if closeErr := resultError(execution.Close()); closeErr != nil {
			inspection = media.Inspection{}
			resultErr = closeErr
		}
	}()
	if source == nil {
		return media.Inspection{}, newError(ErrorStagingFailed)
	}

	dir, input, inputBytes, err := adapter.stage(ctx, source)
	if err != nil {
		return media.Inspection{}, err
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			inspection = media.Inspection{}
			resultErr = newError(ErrorCleanupFailed)
		}
	}()

	budget := newCaptureBudget(adapter.config.Limits.MaxCapturedOutputBytes)
	// ffprobe exits with an error only when no demuxer identifies the content.
	metadata, _, err := adapter.run(ctx, execution, budget, metadataCommand(adapter.config, dir, input))
	if err != nil {
		return media.Inspection{}, rejectContent(err, media.RejectionUnsupportedKind)
	}
	inspection, format, err := NormalizeProbe(metadata, inputBytes, adapter.config.Limits.MaxCapturedOutputBytes)
	if err != nil {
		var adapterError *Error
		if !errors.As(err, &adapterError) || adapterError.Code() != ErrorFrameMetadataRequired {
			return media.Inspection{}, rejectContent(safeNormalizationError(err), "")
		}
		// The frame probe decodes every GIF frame.
		frames, _, runErr := adapter.run(ctx, execution, budget, frameCommand(adapter.config, dir, input))
		if runErr != nil {
			return media.Inspection{}, rejectContent(runErr, media.RejectionDecodeFailed)
		}
		inspection, err = mergeGIFFrames(inspection, format.declared, frames)
		if err != nil {
			return media.Inspection{}, rejectContent(err, "")
		}
	}
	inspection.MediaType = format.mediaType()
	if err := media.Validate(inspection, adapter.config.Limits); err != nil {
		return media.Inspection{}, err
	}
	_, diagnosed, err := adapter.run(ctx, execution, budget, decodeCommand(adapter.config, dir, input))
	if err != nil {
		return media.Inspection{}, rejectContent(err, media.RejectionDecodeFailed)
	}
	// The decode logs only errors; ffmpeg logs some of them, such as a
	// truncated GIF, and still exits 0.
	if diagnosed {
		return media.Inspection{}, media.Reject(media.RejectionDecodeFailed)
	}
	if inspection.Kind == media.KindGIF {
		if err := checkStagedGIF(input); err != nil {
			return media.Inspection{}, rejectContent(err, "")
		}
	}
	return inspection, nil
}

// checkStagedGIF requires every frame the staged GIF declares to fit its
// logical screen.
func checkStagedGIF(input string) error {
	file, err := os.Open(input)
	if err != nil {
		return newError(ErrorStagingFailed)
	}
	defer file.Close()
	return gifFramesFitScreen(file)
}

func (adapter *Adapter) stage(ctx context.Context, source io.Reader) (dir, input string, inputBytes int64, resultErr error) {
	dir, err := os.MkdirTemp(adapter.config.TempRoot, "media-inspection-")
	if err != nil {
		return "", "", 0, newError(ErrorStagingFailed)
	}
	cleanupDir := dir
	keep := false
	defer func() {
		if !keep {
			if err := os.RemoveAll(cleanupDir); err != nil {
				dir, input, inputBytes = "", "", 0
				resultErr = newError(ErrorCleanupFailed)
			}
		}
	}()
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", "", 0, newError(ErrorStagingFailed)
	}
	input = filepath.Join(dir, "input")
	file, err := os.OpenFile(input, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", "", 0, newError(ErrorStagingFailed)
	}

	limit := greatestInputLimit(adapter.config.Limits)
	written, copyErr := copyBounded(ctx, file, source, limit)
	closeErr := file.Close()
	if copyErr != nil {
		return "", "", 0, copyErr
	}
	if closeErr != nil {
		return "", "", 0, newError(ErrorStagingFailed)
	}
	keep = true
	return dir, input, written, nil
}

func copyBounded(ctx context.Context, destination io.Writer, source io.Reader, limit int64) (int64, error) {
	buffer := make([]byte, 32*1024)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return 0, newError(ErrorTimeout)
		}
		remaining := limit - written
		readBuffer := buffer[:boundedReadCapacity(limit, written, len(buffer))]
		count, readErr := source.Read(readBuffer)
		if count < 0 || count > len(readBuffer) {
			return 0, newError(ErrorStagingFailed)
		}
		if count > 0 {
			if int64(count) > remaining {
				return 0, media.Reject(media.RejectionInputTooLarge)
			}
			outputCount, writeErr := destination.Write(readBuffer[:count])
			if writeErr != nil || outputCount != count || outputCount < 0 {
				return 0, newError(ErrorStagingFailed)
			}
			written += int64(outputCount)
		}
		if readErr == io.EOF {
			return written, nil
		}
		if readErr != nil {
			return 0, newError(ErrorStagingFailed)
		}
		if count == 0 {
			continue
		}
	}
}

func boundedReadCapacity(limit, written int64, capacity int) int {
	remaining := limit - written
	if remaining >= int64(capacity) {
		return capacity
	}
	return int(remaining) + 1
}

// run returns the command's stdout and whether it wrote to stderr.
func (adapter *Adapter) run(ctx context.Context, execution Execution, budget *captureBudget, command Command) (_ []byte, _ bool, err error) {
	_, span := telemetry.Start(ctx, telemetry.SpanSubprocess, telemetry.Attr("operation", adapter.tool(command.Path)))
	defer func() { endSpan(span, err) }()
	if ctx.Err() != nil {
		return nil, false, newError(ErrorTimeout)
	}
	if command.Path == adapter.config.FFprobePath {
		command.Args = removeArgument(command.Args, "-nostdin")
	}
	capture := budget.command()
	result := execution.Run(command, capture.stdout(), capture.stderr())
	if budget.overflowed() {
		return nil, false, newError(ErrorOutputOverflow)
	}
	if err := resultError(result); err != nil {
		return nil, false, err
	}
	if ctx.Err() != nil {
		return nil, false, newError(ErrorTimeout)
	}
	return capture.output(), capture.diagnosed(), nil
}

// tool names a configured executable by its role, never by its path.
func (adapter *Adapter) tool(path string) string {
	switch path {
	case adapter.config.FFprobePath:
		return "ffprobe"
	case adapter.config.FFmpegPath:
		return "ffmpeg"
	default:
		return "unknown"
	}
}

// endSpan ends span with outcome ok or the fixed code of the adapter error;
// no message, path, or argument is exported.
func endSpan(span *telemetry.Span, err error) {
	outcome := "ok"
	if err != nil {
		outcome = "error"
		var adapterError *Error
		if errors.As(err, &adapterError) {
			outcome = string(adapterError.Code())
		}
	}
	span.SetAttr("outcome", outcome)
	span.End(err)
}

func removeArgument(arguments []string, target string) []string {
	filtered := make([]string, 0, len(arguments))
	for _, argument := range arguments {
		if argument != target {
			filtered = append(filtered, argument)
		}
	}
	return filtered
}

func beginExecution(raw Execution, result Result) (Execution, error) {
	if !result.valid() {
		return nil, newError(ErrorExecutionFailed)
	}
	if result.succeeded() {
		execution, ok := guardExecution(raw)
		if !ok {
			return nil, newError(ErrorExecutionFailed)
		}
		return execution, nil
	}
	if !nilInterface(raw) {
		return nil, newError(ErrorExecutionFailed)
	}
	return nil, resultError(result)
}

func resultError(result Result) error {
	if !result.valid() {
		return newError(ErrorExecutionFailed)
	}
	if result.succeeded() {
		return nil
	}
	switch result.failureKind() {
	case FailureContainmentUnavailable:
		return newError(ErrorContainmentUnavailable)
	case FailureContainmentSetup:
		return newError(ErrorContainmentSetup)
	case FailureContainmentCleanup:
		return newError(ErrorContainmentCleanup)
	case FailureTimeout:
		return newError(ErrorTimeout)
	case FailureCancelled:
		return newError(ErrorCancelled)
	case FailureMemoryLimit, FailureTaskLimit, FailureCPULimit:
		// Only a command inspecting the content exhausts its limits.
		return media.Reject(media.RejectionResourcesExceeded)
	case FailureToolExit:
		return newError(ErrorCommandFailed)
	case FailureInternal:
		return newError(ErrorExecutionFailed)
	default:
		return newError(ErrorExecutionFailed)
	}
}

// rejectContent turns an adapter error the content causes into the rejection
// of the rule it breaks: toolExit for a failing command, when that command
// inspects the content. Every other failure is returned unchanged.
func rejectContent(err error, toolExit media.RejectionCode) error {
	var adapterError *Error
	if !errors.As(err, &adapterError) {
		return err
	}
	switch adapterError.Code() {
	case ErrorUnsupportedMedia:
		return media.Reject(media.RejectionUnsupportedKind)
	case ErrorInvalidProbe, ErrorInvalidFrames:
		return media.Reject(media.RejectionInvalidMetadata)
	case ErrorCommandFailed:
		if toolExit != "" {
			return media.Reject(toolExit)
		}
	}
	return err
}

func safeNormalizationError(err error) error {
	var adapterError *Error
	if errors.As(err, &adapterError) {
		return newError(adapterError.Code())
	}
	return newError(ErrorInvalidProbe)
}

func canonicalExecutable(path string) (string, bool) {
	canonical, ok := canonicalPath(path)
	if !ok {
		return "", false
	}
	info, err := os.Stat(canonical)
	return canonical, err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

func canonicalTempRoot(root string) (string, bool) {
	canonical, ok := canonicalPath(root)
	if !ok || canonical == string(filepath.Separator) {
		return "", false
	}
	info, err := os.Stat(canonical)
	return canonical, err == nil && info.IsDir()
}

func canonicalPath(path string) (string, bool) {
	if strings.IndexByte(path, 0) >= 0 || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", false
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil || !filepath.IsAbs(canonical) {
		return "", false
	}
	return canonical, true
}

func greatestInputLimit(limits media.Limits) int64 {
	greatest := limits.MaxImageBytes
	for _, candidate := range []int64{limits.MaxGIFBytes, limits.MaxAudioBytes, limits.MaxVideoBytes} {
		if candidate > greatest {
			greatest = candidate
		}
	}
	return greatest
}
