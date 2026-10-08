package mediaexec

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/enzoruiz/pepicanvas/backend/internal/media"
)

const maxCapturedOutputBytes = int64(16 * 1024 * 1024)

// Runner executes one shell-free command under the supplied context. A runner
// must not return until both output streams reach EOF. MEDIA-02C supplies the
// production implementation with cgroup-v2 containment.
type Runner interface {
	Run(context.Context, Command, io.Writer, io.Writer) error
}

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
	if runner == nil || !validAbsolutePath(config.FFprobePath) || !validAbsolutePath(config.FFmpegPath) ||
		!validTempRoot(config.TempRoot) || config.Limits.Validate() != nil ||
		config.Limits.MaxCapturedOutputBytes > maxCapturedOutputBytes {
		return nil, newError(ErrorConfiguration)
	}
	return &Adapter{config: config, runner: runner}, nil
}

// Inspect consumes one untrusted byte stream. Context is checked between source
// reads; a Reader that can block indefinitely must be made cancelable by its
// owner. One deadline and one output budget cover staging and every command.
func (adapter *Adapter) Inspect(parent context.Context, source io.Reader) (inspection media.Inspection, resultErr error) {
	if source == nil {
		return media.Inspection{}, newError(ErrorStagingFailed)
	}
	ctx, cancel := context.WithTimeout(parent, adapter.config.Limits.InspectionTimeout)
	defer cancel()

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
	metadata, err := adapter.run(ctx, budget, metadataCommand(adapter.config, dir, input))
	if err != nil {
		return media.Inspection{}, err
	}
	inspection, _, err = NormalizeProbe(metadata, inputBytes, adapter.config.Limits.MaxCapturedOutputBytes)
	if err != nil {
		var adapterError *Error
		if !errors.As(err, &adapterError) || adapterError.Code() != ErrorFrameMetadataRequired {
			return media.Inspection{}, safeNormalizationError(err)
		}
		frames, runErr := adapter.run(ctx, budget, frameCommand(adapter.config, dir, input))
		if runErr != nil {
			return media.Inspection{}, runErr
		}
		inspection, err = mergeGIFFrames(inspection, frames)
		if err != nil {
			return media.Inspection{}, err
		}
	}
	if err := media.Validate(inspection, adapter.config.Limits); err != nil {
		return media.Inspection{}, newError(ErrorValidation)
	}
	if _, err := adapter.run(ctx, budget, decodeCommand(adapter.config, dir, input)); err != nil {
		return media.Inspection{}, err
	}
	return inspection, nil
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
		readBuffer := buffer
		remaining := limit - written
		if remaining < int64(len(buffer)) {
			readBuffer = buffer[:remaining+1]
		}
		count, readErr := source.Read(readBuffer)
		if count > 0 {
			outputCount, writeErr := destination.Write(readBuffer[:count])
			written += int64(outputCount)
			if writeErr != nil || outputCount != count {
				return 0, newError(ErrorStagingFailed)
			}
			if written > limit {
				return 0, newError(ErrorInputTooLarge)
			}
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

func (adapter *Adapter) run(ctx context.Context, budget *captureBudget, command Command) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, newError(ErrorTimeout)
	}
	capture := budget.command()
	err := adapter.runner.Run(ctx, command, capture.stdout(), capture.stderr())
	if budget.overflowed() {
		return nil, newError(ErrorOutputOverflow)
	}
	if ctx.Err() != nil {
		return nil, newError(ErrorTimeout)
	}
	if err != nil {
		return nil, newError(ErrorCommandFailed)
	}
	return capture.output(), nil
}

func safeNormalizationError(err error) error {
	var adapterError *Error
	if errors.As(err, &adapterError) {
		return newError(adapterError.Code())
	}
	return newError(ErrorInvalidProbe)
}

func validAbsolutePath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != string(filepath.Separator)
}

func validTempRoot(root string) bool {
	if !validAbsolutePath(root) {
		return false
	}
	info, err := os.Lstat(root)
	return err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0
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
