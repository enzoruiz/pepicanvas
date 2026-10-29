package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Exporter sends a batch of ended spans to a collector or sink.
type Exporter interface {
	Export(ctx context.Context, spans []SpanData) error
}

// Config tunes a Tracer. Zero sizes and durations select the defaults.
type Config struct {
	Exporter      Exporter
	QueueSize     int           // default 2048
	BatchSize     int           // default 256
	Interval      time.Duration // default 1s
	ExportTimeout time.Duration // default 5s
	Logger        *slog.Logger  // default slog.Default()
}

// ErrInvalidConfig reports a configuration without an exporter or with a
// negative size or duration.
var ErrInvalidConfig = errors.New("telemetry: invalid config")

// Stats counts what happened to ended spans.
type Stats struct {
	Exported  uint64 // accepted by the exporter
	Dropped   uint64 // rejected because the queue was full or the tracer closed
	Failed    uint64 // in a batch the exporter failed or abandoned
	Discarded uint64 // still queued when Close gave up waiting
}

// Tracer queues ended spans without blocking and exports them in batches from
// one worker, so an unavailable exporter costs bounded memory and no latency.
type Tracer struct {
	config Config
	queue  chan SpanData

	mu     sync.RWMutex
	closed bool

	abort       context.Context
	cancelAbort context.CancelFunc
	done        chan struct{}

	exported, dropped, failed, discarded atomic.Uint64
}

// New starts a tracer and its export worker.
func New(config Config) (*Tracer, error) {
	if config.Exporter == nil || config.QueueSize < 0 || config.BatchSize < 0 ||
		config.Interval < 0 || config.ExportTimeout < 0 {
		return nil, ErrInvalidConfig
	}
	if config.QueueSize == 0 {
		config.QueueSize = 2048
	}
	if config.BatchSize == 0 {
		config.BatchSize = 256
	}
	if config.Interval == 0 {
		config.Interval = time.Second
	}
	if config.ExportTimeout == 0 {
		config.ExportTimeout = 5 * time.Second
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	abort, cancel := context.WithCancel(context.Background())
	t := &Tracer{
		config:      config,
		queue:       make(chan SpanData, config.QueueSize),
		abort:       abort,
		cancelAbort: cancel,
		done:        make(chan struct{}),
	}
	go t.run()
	return t, nil
}

// Stats returns the current counters.
func (t *Tracer) Stats() Stats {
	return Stats{
		Exported:  t.exported.Load(),
		Dropped:   t.dropped.Load(),
		Failed:    t.failed.Load(),
		Discarded: t.discarded.Load(),
	}
}

// enqueue never blocks: a full queue or a closed tracer counts the span as
// dropped.
func (t *Tracer) enqueue(data SpanData) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.closed {
		t.dropped.Add(1)
		return
	}
	select {
	case t.queue <- data:
	default:
		t.dropped.Add(1)
	}
}

// Close stops accepting spans, exports every queued span, and waits for the
// worker to exit. If ctx ends first, the in-flight export is cancelled, the
// spans still queued are discarded without export, and ctx's error is
// returned once the worker has exited. Close is idempotent.
func (t *Tracer) Close(ctx context.Context) error {
	t.mu.Lock()
	if !t.closed {
		t.closed = true
		close(t.queue)
	}
	t.mu.Unlock()

	select {
	case <-t.done:
		return nil
	case <-ctx.Done():
		t.cancelAbort()
		<-t.done
		return ctx.Err()
	}
}

func (t *Tracer) run() {
	defer close(t.done)
	defer t.cancelAbort()
	ticker := time.NewTicker(t.config.Interval)
	defer ticker.Stop()
	// Each flushed batch is handed off, never reused, so an exporter may keep it.
	var batch []SpanData
	for {
		select {
		case data, ok := <-t.queue:
			if !ok {
				t.flush(batch)
				return
			}
			batch = append(batch, data)
			if len(batch) >= t.config.BatchSize {
				t.flush(batch)
				batch = nil
			}
		case <-ticker.C:
			t.flush(batch)
			batch = nil
		}
	}
}

// flush exports batch, or discards it once Close has given up waiting.
func (t *Tracer) flush(batch []SpanData) {
	if len(batch) == 0 {
		return
	}
	n := uint64(len(batch))
	if t.abort.Err() != nil {
		t.discarded.Add(n)
		return
	}
	ctx, cancel := context.WithTimeout(t.abort, t.config.ExportTimeout)
	err := t.config.Exporter.Export(ctx, batch)
	cancel()
	if err != nil {
		t.failed.Add(n)
		// Counts only: the error text may echo span content.
		t.config.Logger.Warn("telemetry export failed", "spans", n, "failed_total", t.failed.Load())
		return
	}
	t.exported.Add(n)
}
