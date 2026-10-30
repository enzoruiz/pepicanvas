package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/platform/telemetry"
)

// traceFileVariable names the owner-only JSON-lines file of exported spans.
const traceFileVariable = "PEPICANVAS_TRACE_FILE"

// tracerFromEnv returns the tracer of exported spans and the function that
// stops it. Without PEPICANVAS_TRACE_FILE export is disabled: the tracer is
// nil, spans only keep local logs correlated, and stop does nothing. With it
// the file is opened owner-only; an unusable file is refused naming only the
// variable. stop closes the tracer within shutdownTimeout, then the file, and
// logs the tracer's counters; it is idempotent.
func tracerFromEnv(getenv func(string) string, logger *slog.Logger) (*telemetry.Tracer, func(), error) {
	path := getenv(traceFileVariable)
	if path == "" {
		return nil, func() {}, nil
	}
	file, err := telemetry.OpenFile(path)
	if errors.Is(err, telemetry.ErrInsecureFile) {
		return nil, nil, errors.New(traceFileVariable + " must name a regular owner-only file")
	}
	if err != nil {
		// The cause names the path, so it is not wrapped.
		return nil, nil, errors.New(traceFileVariable + " cannot be opened")
	}
	tracer, err := telemetry.New(telemetry.Config{Exporter: file, Logger: logger})
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	return tracer, stopTracer(tracer, file, logger, shutdownTimeout), nil
}

// stopTracer returns the idempotent function that closes tracer within
// timeout, then file, and logs the tracer's counters: at info level, or at
// warn level with a fixed reason when either close fails.
func stopTracer(tracer *telemetry.Tracer, file io.Closer, logger *slog.Logger, timeout time.Duration) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			// Close fails only with ctx's error; a file error names the
			// path, so it is reported by a fixed reason.
			var reason string
			if err := tracer.Close(ctx); err != nil {
				reason = err.Error()
			}
			if err := file.Close(); err != nil && reason == "" {
				reason = "span file close failed"
			}
			stats := tracer.Stats()
			attrs := []any{
				slog.Uint64("exported", stats.Exported),
				slog.Uint64("dropped", stats.Dropped),
				slog.Uint64("failed", stats.Failed),
				slog.Uint64("discarded", stats.Discarded),
			}
			if reason != "" {
				logger.Warn("telemetry stopped", append(attrs, slog.String("error", reason))...)
				return
			}
			logger.Info("telemetry stopped", attrs...)
		})
	}
}
