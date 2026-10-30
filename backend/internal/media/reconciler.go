package media

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Defaults applied when a ReconcilerConfig field is zero.
const (
	// DefaultOrphanGrace is how old an object without a row must be before
	// its owner evidence allows deleting it. It is twice the default
	// reservation TTL, so it covers an upload whose row and object are still
	// being written or released.
	DefaultOrphanGrace = time.Hour
	// DefaultReconcileBatch bounds the items one store or storage call reads.
	DefaultReconcileBatch = 100
	// DefaultReconcileInterval is the wait between periodic passes.
	DefaultReconcileInterval = 5 * time.Minute
)

// ErrReconcilerClosed reports a reconciler that was closed.
var ErrReconcilerClosed = errors.New("media: reconciler closed")

// ReconcilerConfig configures a Reconciler. Store and Storage are required;
// a zero field takes its default.
type ReconcilerConfig struct {
	Store   Store
	Storage ObjectStore
	// Now returns the current time; it defaults to time.Now.
	Now func() time.Time
	// Grace is the minimum age of an orphan object before it is deleted.
	Grace time.Duration
	// Batch bounds each store and storage page, at most MaxScanKeys.
	Batch int
	// Interval is the wait between periodic passes.
	Interval time.Duration
	// NewCorrelation returns the correlation ID of a pass that has none; it
	// defaults to 32 random lowercase hex characters.
	NewCorrelation func() string
	// Correlate returns ctx carrying the pass's correlation ID, so every
	// store and storage call of the pass relates to it. It defaults to
	// returning ctx unchanged.
	Correlate func(ctx context.Context, correlationID string) context.Context
}

// Report is the outcome of one reconciliation pass. It holds counts only and
// never names an asset, an object, a reference, or a tenant.
type Report struct {
	CorrelationID string
	StartedAt     time.Time
	FinishedAt    time.Time
	// Purged counts quarantined and deleting assets whose bytes were deleted
	// and whose rows were purged.
	Purged int
	// OrphansDeleted counts objects without a row whose owner metadata names
	// an asset that does not exist, older than the grace period.
	OrphansDeleted int
	// Unowned counts objects without a row or valid owner metadata; they are
	// kept.
	Unowned int
	// Young counts objects without a row inside the grace period; they are
	// kept.
	Young int
	// Claimed counts objects without a row whose owner asset exists; they are
	// kept.
	Claimed int
	// MissingReady counts ready assets whose bytes are missing; they are
	// reported and never changed.
	MissingReady int
	// Failures counts items a store or storage call failed on; they are
	// retried by the next pass.
	Failures int
	// Complete reports whether every step of the pass finished.
	Complete bool
}

// Reconciler converges committed assets and stored objects: it deletes the
// bytes of quarantined, deleting, and expired assets, deletes orphan objects
// only with owner evidence, and reports ready assets whose bytes are missing.
// Every state stays in the store, so an interrupted pass is resumed by the
// next one. Passes of one Reconciler never overlap.
type Reconciler struct {
	config ReconcilerConfig
	// turn is held by the pass in flight.
	turn chan struct{}
	wake chan struct{}
	stop chan struct{}
	done chan struct{}
	// abort cancels the pass in flight when Close runs out of time.
	abort       context.Context
	cancelAbort context.CancelFunc
	// passes counts the Run calls admitted before Close.
	passes sync.WaitGroup

	mu      sync.Mutex
	started bool
	closed  bool
	last    Report
	hasLast bool
}

// NewReconciler validates the configuration. Run reconciles once; Start runs
// periodic passes.
func NewReconciler(config ReconcilerConfig) (*Reconciler, error) {
	if config.Store == nil || config.Storage == nil {
		return nil, fmt.Errorf("%w: reconciler store and storage are required", ErrInvalid)
	}
	if config.Grace < 0 || config.Batch < 0 || config.Interval < 0 {
		return nil, fmt.Errorf("%w: reconciler durations and sizes must not be negative", ErrInvalid)
	}
	if config.Batch > MaxScanKeys {
		return nil, fmt.Errorf("%w: reconciler batch must be at most %d", ErrInvalid, MaxScanKeys)
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Grace == 0 {
		config.Grace = DefaultOrphanGrace
	}
	if config.Batch == 0 {
		config.Batch = DefaultReconcileBatch
	}
	if config.Interval == 0 {
		config.Interval = DefaultReconcileInterval
	}
	if config.NewCorrelation == nil {
		config.NewCorrelation = newCorrelation
	}
	if config.Correlate == nil {
		config.Correlate = func(ctx context.Context, _ string) context.Context { return ctx }
	}
	abort, cancelAbort := context.WithCancel(context.Background())
	return &Reconciler{
		config: config,
		turn:   make(chan struct{}, 1),
		wake:   make(chan struct{}, 1),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
		abort:  abort, cancelAbort: cancelAbort,
	}, nil
}

// Run reconciles once, after the pass in flight, if any. An empty
// correlationID takes a generated one. Steps continue past failed items,
// which are counted in Failures, and the first error is returned. When ctx
// ends the partial report is returned with ctx's error, and the next pass
// resumes from the persisted states. After Close, Run returns
// ErrReconcilerClosed.
func (r *Reconciler) Run(ctx context.Context, correlationID string) (Report, error) {
	if correlationID == "" {
		correlationID = r.config.NewCorrelation()
	}
	if err := validateIdentifier("correlation", correlationID); err != nil {
		return Report{}, err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return Report{}, ErrReconcilerClosed
	}
	r.passes.Add(1)
	r.mu.Unlock()
	defer r.passes.Done()

	select {
	case r.turn <- struct{}{}:
	case <-ctx.Done():
		return Report{}, ctx.Err()
	}
	defer func() { <-r.turn }()
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return Report{}, ErrReconcilerClosed
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopAbort := context.AfterFunc(r.abort, cancel)
	defer stopAbort()
	return r.pass(ctx, correlationID)
}

// Start runs a pass at once and then one every Interval, until Close.
func (r *Reconciler) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrReconcilerClosed
	}
	if r.started {
		return errors.New("media: reconciler already started")
	}
	r.started = true
	go r.run()
	return nil
}

// Trigger requests a pass as soon as possible. Requests made while one is
// pending coalesce. It never blocks and does nothing unless running.
func (r *Reconciler) Trigger() {
	if !r.Running() {
		return
	}
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Running reports whether the periodic loop is started and not closed.
func (r *Reconciler) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.started && !r.closed
}

// Last returns the report of the latest finished pass, periodic or Run.
func (r *Reconciler) Last() (Report, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last, r.hasLast
}

// Close stops the reconciler with the contract of realtime.Hub.Close: no new
// pass or Run starts, the pass in flight finishes, and Close waits for it and
// for the loop to exit. If ctx ends first, the pass in flight is cancelled and
// ctx's error is returned once it returned. Pending work stays in its
// persisted states for the next reconciler; nothing is deleted on close.
// Close is idempotent.
func (r *Reconciler) Close(ctx context.Context) error {
	r.mu.Lock()
	first := !r.closed
	r.closed = true
	started := r.started
	r.mu.Unlock()
	if first && started {
		close(r.stop)
	}
	finished := make(chan struct{})
	go func() {
		if started {
			<-r.done
		}
		r.passes.Wait()
		close(finished)
	}()
	select {
	case <-finished:
		r.cancelAbort()
		return nil
	case <-ctx.Done():
		r.cancelAbort()
		<-finished
		return ctx.Err()
	}
}

func (r *Reconciler) run() {
	defer close(r.done)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-r.wake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
		}
		select {
		case r.turn <- struct{}{}:
		case <-r.stop:
			return
		}
		// A select with a fired timer and a closed stop picks either case, so
		// check again: no pass starts once Close has begun.
		select {
		case <-r.stop:
			<-r.turn
			return
		default:
		}
		_, _ = r.pass(r.abort, r.config.NewCorrelation())
		<-r.turn
		timer.Reset(r.config.Interval)
	}
}

// pass runs the garbage, orphan, and ready steps and records the report.
func (r *Reconciler) pass(ctx context.Context, correlationID string) (Report, error) {
	ctx = r.config.Correlate(ctx, correlationID)
	report := Report{CorrelationID: correlationID, StartedAt: r.config.Now()}
	var first error
	complete := true
	for _, step := range []func(context.Context, *Report, *error) error{r.collectGarbage, r.deleteOrphans, r.checkReady} {
		if ctx.Err() != nil {
			break
		}
		if err := step(ctx, &report, &first); err != nil {
			complete = false
			if ctx.Err() == nil && first == nil {
				first = err
			}
		}
	}
	report.Complete = complete && ctx.Err() == nil
	report.FinishedAt = r.config.Now()
	r.mu.Lock()
	r.last, r.hasLast = report, true
	r.mu.Unlock()
	return report, errors.Join(first, ctx.Err())
}

// collectGarbage deletes the bytes of quarantined, deleting, and expired
// assets and purges each one whose bytes are gone. It repeats while a full
// batch made progress; an item that failed once in this pass is left for the
// next pass, so failing items never loop.
func (r *Reconciler) collectGarbage(ctx context.Context, report *Report, first *error) error {
	failed := map[string]bool{}
	for {
		garbage, err := r.config.Store.Collect(ctx, r.config.Batch)
		if err != nil {
			return err
		}
		purged := 0
		for _, item := range garbage {
			if failed[item.StorageKey] {
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			err := r.config.Storage.Delete(ctx, item.StorageKey)
			if err == nil {
				err = r.config.Store.Purge(ctx, item.Key, item.AssetID)
			}
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				failed[item.StorageKey] = true
				r.fail(report, first, err)
				continue
			}
			purged++
			report.Purged++
		}
		if purged == 0 || len(garbage) < r.config.Batch {
			return nil
		}
	}
}

// deleteOrphans scans the whole bucket. An object no asset holds is deleted
// only when its owner metadata validates, it is older than the grace period,
// and the owner's asset does not exist; otherwise it is counted and kept.
func (r *Reconciler) deleteOrphans(ctx context.Context, report *Report, first *error) error {
	cutoff := report.StartedAt.Add(-r.config.Grace)
	after := ""
	for {
		page, err := r.config.Storage.List(ctx, after, r.config.Batch)
		if err != nil || len(page) == 0 {
			return err
		}
		keys := make([]string, len(page))
		for i, object := range page {
			keys[i] = object.Key
		}
		held, err := r.config.Store.HeldKeys(ctx, keys)
		if err != nil {
			return err
		}
		for _, key := range keys {
			if held[key] {
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := r.inspectOrphan(ctx, report, key, cutoff); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				r.fail(report, first, err)
			}
		}
		if len(page) < r.config.Batch {
			return nil
		}
		after = page[len(page)-1].Key
	}
}

func (r *Reconciler) inspectOrphan(ctx context.Context, report *Report, key string, cutoff time.Time) error {
	object, err := r.config.Storage.Stat(ctx, key)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil
	case err != nil:
		return err
	case !object.Owned:
		report.Unowned++
		return nil
	case object.ModifiedAt.After(cutoff):
		report.Young++
		return nil
	}
	exists, err := r.config.Store.AssetExists(ctx, object.Owner)
	if err != nil {
		return err
	}
	if exists {
		report.Claimed++
		return nil
	}
	if err := r.config.Storage.Delete(ctx, key); err != nil {
		return err
	}
	report.OrphansDeleted++
	return nil
}

// checkReady reports ready assets whose bytes are missing. It never changes
// them: Uploads.Open already answers not found for them.
func (r *Reconciler) checkReady(ctx context.Context, report *Report, first *error) error {
	after := Owner{}
	for {
		page, err := r.config.Store.ReadyPage(ctx, after, r.config.Batch)
		if err != nil || len(page) == 0 {
			return err
		}
		for _, asset := range page {
			if err := ctx.Err(); err != nil {
				return err
			}
			_, err := r.config.Storage.Stat(ctx, asset.StorageKey)
			switch {
			case err == nil:
			case errors.Is(err, ErrNotFound):
				report.MissingReady++
			case ctx.Err() != nil:
				return ctx.Err()
			default:
				r.fail(report, first, err)
			}
		}
		if len(page) < r.config.Batch {
			return nil
		}
		after = page[len(page)-1].Owner
	}
}

func (r *Reconciler) fail(report *Report, first *error, err error) {
	report.Failures++
	if *first == nil {
		*first = err
	}
}

// newCorrelation returns 32 random lowercase hex characters.
func newCorrelation() string {
	var id [16]byte
	_, _ = rand.Read(id[:])
	return hex.EncodeToString(id[:])
}
