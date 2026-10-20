package outbox_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/outbox"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/pgtest"
	"github.com/enzoruiz/pepicanvas/backend/internal/realtime"
	"github.com/enzoruiz/pepicanvas/backend/internal/resync"
)

var t0 = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

// clock is a settable time source.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// stored is one record of memStore with its settlement.
type stored struct {
	record    outbox.Record
	lastError string
	settledAt time.Time
	outcome   outbox.Outcome
}

// memStore is an in-memory outbox.Store with the PostgreSQL semantics.
type memStore struct {
	mu       sync.Mutex
	records  []*stored
	nextID   int64
	failRead int
	cutoffs  []time.Time
}

func (s *memStore) add(key canvas.Key, revision int64, correlationID string, at time.Time) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	s.records = append(s.records, &stored{record: outbox.Record{
		ID: s.nextID,
		Event: resync.Event{
			Key: key, Mutation: canvas.SetContent{ObjectID: "o", Content: fmt.Sprint(revision)},
			Result: canvas.Result{Revision: revision, ObjectID: "o"}, CorrelationID: correlationID,
		},
		CreatedAt: at, NextAttempt: at,
	}})
	return s.nextID
}

// addMalformed adds a record the store could not decode.
func (s *memStore) addMalformed(key canvas.Key, revision int64, at time.Time, reason error) int64 {
	id := s.add(key, revision, "", at)
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.find(id)
	r.record.Event.Mutation, r.record.Malformed = nil, reason
	return id
}

func (s *memStore) addSettled(at time.Time) int64 {
	id := s.add(canvas.Key{TenantID: "t", CanvasID: "settled"}, 1, "", at)
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.find(id)
	r.settledAt, r.outcome = at, outbox.OutcomeDelivered
	return id
}

func (s *memStore) find(id int64) *stored {
	for _, r := range s.records {
		if r.record.ID == id {
			return r
		}
	}
	return nil
}

func (s *memStore) get(id int64) stored {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.find(id); r != nil {
		return *r
	}
	return stored{}
}

func (s *memStore) Heads(_ context.Context, limit int) ([]outbox.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failRead > 0 {
		s.failRead--
		return nil, errors.New("store unavailable")
	}
	seen := map[canvas.Key]bool{}
	var heads []outbox.Record
	for _, r := range s.records {
		key := r.record.Event.Key
		if r.outcome == "" && !seen[key] {
			seen[key] = true
			heads = append(heads, r.record)
		}
	}
	slices.SortStableFunc(heads, func(a, b outbox.Record) int { return a.NextAttempt.Compare(b.NextAttempt) })
	return heads[:min(limit, len(heads))], nil
}

func (s *memStore) Settle(_ context.Context, id int64, outcome outbox.Outcome, at time.Time, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.find(id); r != nil && r.outcome == "" {
		r.outcome, r.settledAt = outcome, at
		if reason != "" {
			r.lastError = reason
		}
	}
	return nil
}

func (s *memStore) Retry(_ context.Context, id int64, attempts int, next time.Time, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.find(id); r != nil && r.outcome == "" {
		r.record.Attempts, r.record.NextAttempt, r.lastError = attempts, next, reason
	}
	return nil
}

func (s *memStore) Purge(_ context.Context, cutoff time.Time, limit int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cutoffs = append(s.cutoffs, cutoff)
	kept, purged := s.records[:0:0], 0
	for _, r := range s.records {
		if r.outcome != "" && !r.settledAt.After(cutoff) && purged < limit {
			purged++
			continue
		}
		kept = append(kept, r)
	}
	s.records = kept
	return purged, nil
}

// sink records deliveries and answers through deliver.
type sink struct {
	mu        sync.Mutex
	delivered []outbox.Record
	deliver   func(context.Context, outbox.Record) error
}

func (s *sink) Deliver(ctx context.Context, record outbox.Record) error {
	var err error
	if s.deliver != nil {
		err = s.deliver(ctx, record)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delivered = append(s.delivered, record)
	return err
}

// calls returns the delivered (key, revision) pairs in call order.
func (s *sink) calls(key canvas.Key) []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var revisions []int64
	for _, r := range s.delivered {
		if r.Event.Key == key {
			revisions = append(revisions, r.Event.Result.Revision)
		}
	}
	return revisions
}

func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func start(t *testing.T, config outbox.Config) *outbox.Publisher {
	t.Helper()
	publisher, err := outbox.NewPublisher(config)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	if err := publisher.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = publisher.Close(context.Background()) })
	return publisher
}

var (
	canvasA = canvas.Key{TenantID: "tenant-a", CanvasID: "main"}
	canvasB = canvas.Key{TenantID: "tenant-b", CanvasID: "main"}
)

func TestPublisherDeliversRecordsPendingAtStartInRevisionOrderPerCanvas(t *testing.T) {
	store, out, now := &memStore{}, &sink{}, &clock{now: t0}
	var ids []int64
	for _, step := range []struct {
		key      canvas.Key
		revision int64
	}{{canvasA, 1}, {canvasB, 1}, {canvasA, 2}, {canvasA, 3}, {canvasB, 2}} {
		ids = append(ids, store.add(step.key, step.revision, "", t0))
	}
	publisher := start(t, outbox.Config{Store: store, Sink: out, Now: now.Now, PollInterval: time.Hour})

	eventually(t, "every record settled", func() bool { return publisher.Counters().Delivered == 5 })
	if got := out.calls(canvasA); !slices.Equal(got, []int64{1, 2, 3}) {
		t.Fatalf("canvas A delivery order = %v, want [1 2 3]", got)
	}
	if got := out.calls(canvasB); !slices.Equal(got, []int64{1, 2}) {
		t.Fatalf("canvas B delivery order = %v, want [1 2]", got)
	}
	for _, id := range ids {
		if r := store.get(id); r.outcome != outbox.OutcomeDelivered || !r.settledAt.Equal(t0) || r.lastError != "" {
			t.Fatalf("record %d = %+v, want delivered at %v", id, r, t0)
		}
	}
	if c := publisher.Counters(); c != (outbox.Counters{Delivered: 5}) {
		t.Fatalf("Counters = %+v, want 5 delivered only", c)
	}
}

func TestPublisherRetriesWithBoundedBackoffAndBlocksOnlyThatCanvas(t *testing.T) {
	store, now := &memStore{}, &clock{now: t0}
	failures := 4
	var mu sync.Mutex
	out := &sink{deliver: func(_ context.Context, r outbox.Record) error {
		mu.Lock()
		defer mu.Unlock()
		if r.Event.Key == canvasA && r.Event.Result.Revision == 1 && failures > 0 {
			failures--
			return errors.New("actor unavailable")
		}
		return nil
	}}
	a1 := store.add(canvasA, 1, "", t0)
	a2 := store.add(canvasA, 2, "", t0)
	store.add(canvasB, 1, "", t0)
	publisher := start(t, outbox.Config{
		Store: store, Sink: out, Now: now.Now, PollInterval: time.Hour,
		MinBackoff: 10 * time.Millisecond, MaxBackoff: 40 * time.Millisecond,
	})

	eventually(t, "canvas B delivered", func() bool { return slices.Equal(out.calls(canvasB), []int64{1}) })
	wantBackoffs := []time.Duration{10, 20, 40, 40}
	for attempt, backoff := range wantBackoffs {
		eventually(t, fmt.Sprintf("attempt %d recorded", attempt+1), func() bool { return store.get(a1).record.Attempts == attempt+1 })
		r := store.get(a1)
		if want := now.Now().Add(backoff * time.Millisecond); !r.record.NextAttempt.Equal(want) || r.lastError != "actor unavailable" || r.outcome != "" {
			t.Fatalf("after attempt %d record = %+v, want pending, next attempt %v, last error recorded", attempt+1, r, want)
		}
		if got := out.calls(canvasA); slices.Contains(got, 2) {
			t.Fatalf("canvas A revision 2 delivered before revision 1: %v", got)
		}
		now.Advance(backoff * time.Millisecond)
		publisher.Notify()
	}
	eventually(t, "canvas A delivered", func() bool { return store.get(a2).outcome == outbox.OutcomeDelivered })
	if got := out.calls(canvasA); !slices.Equal(got, []int64{1, 1, 1, 1, 1, 2}) {
		t.Fatalf("canvas A calls = %v, want four failed attempts of 1, then 1 and 2", got)
	}
	if c := publisher.Counters(); c.Retries != 4 || c.Delivered != 3 || c.Rejected != 0 {
		t.Fatalf("Counters = %+v, want 4 retries, 3 delivered", c)
	}
}

func TestPublisherSettlesUndeliverableRecordsWithoutRetry(t *testing.T) {
	store, now := &memStore{}, &clock{now: t0}
	out := &sink{deliver: func(_ context.Context, r outbox.Record) error {
		if r.Event.Result.Revision == 1 {
			return fmt.Errorf("%w: access denied", outbox.ErrUndeliverable)
		}
		return nil
	}}
	first := store.add(canvasA, 1, "", t0)
	second := store.add(canvasA, 2, "", t0)
	publisher := start(t, outbox.Config{Store: store, Sink: out, Now: now.Now, PollInterval: time.Hour})

	eventually(t, "both records settled", func() bool { return store.get(second).outcome != "" })
	if r := store.get(first); r.outcome != outbox.OutcomeRejected || r.lastError != "outbox: undeliverable: access denied" || r.record.Attempts != 0 {
		t.Fatalf("undeliverable record = %+v, want rejected with its reason and no retry", r)
	}
	if r := store.get(second); r.outcome != outbox.OutcomeDelivered {
		t.Fatalf("next record = %+v, want delivered", r)
	}
	if got := out.calls(canvasA); !slices.Equal(got, []int64{1, 2}) {
		t.Fatalf("calls = %v, want one attempt each", got)
	}
	if c := publisher.Counters(); c != (outbox.Counters{Delivered: 1, Rejected: 1}) {
		t.Fatalf("Counters = %+v", c)
	}
}

func TestPublisherRejectsMalformedRecordsWithoutDeliveringThem(t *testing.T) {
	store, out, now := &memStore{}, &sink{}, &clock{now: t0}
	var buffer bytes.Buffer
	var mu sync.Mutex
	logger := slog.New(slog.NewJSONHandler(&lockedWriter{w: &buffer, mu: &mu}, nil))
	reason := fmt.Errorf("postgres: outbox record 1: %w: unknown kind", outbox.ErrMalformed)
	poison := store.addMalformed(canvasA, 1, t0, reason)
	next := store.add(canvasA, 2, "", t0)
	other := store.add(canvasB, 1, "", t0)
	publisher := start(t, outbox.Config{Store: store, Sink: out, Now: now.Now, PollInterval: time.Hour, Logger: logger})

	eventually(t, "every record settled", func() bool {
		return store.get(next).outcome != "" && store.get(other).outcome != ""
	})
	if r := store.get(poison); r.outcome != outbox.OutcomeRejected || r.lastError != reason.Error() || r.record.Attempts != 0 || !r.settledAt.Equal(t0) {
		t.Fatalf("malformed record = %+v, want rejected at %v with %q and no retry", r, t0, reason)
	}
	for _, id := range []int64{next, other} {
		if r := store.get(id); r.outcome != outbox.OutcomeDelivered {
			t.Fatalf("record %d = %+v, want delivered", id, r)
		}
	}
	if got := out.calls(canvasA); !slices.Equal(got, []int64{2}) {
		t.Fatalf("canvas A calls = %v, want only revision 2: a malformed record never reaches the sink", got)
	}
	if got := out.calls(canvasB); !slices.Equal(got, []int64{1}) {
		t.Fatalf("canvas B calls = %v, want [1]", got)
	}
	if c := publisher.Counters(); c != (outbox.Counters{Delivered: 2, Rejected: 1}) {
		t.Fatalf("Counters = %+v, want 2 delivered, 1 rejected, no retries or store errors", c)
	}
	mu.Lock()
	logs := buffer.String()
	mu.Unlock()
	var line string
	for _, l := range strings.Split(logs, "\n") {
		if strings.Contains(l, fmt.Sprintf(`"outbox_id":%d`, poison)) {
			line = l
		}
	}
	for _, want := range []string{`"level":"WARN"`, `"msg":"outbox record rejected"`, `"error":"` + reason.Error() + `"`} {
		if !strings.Contains(line, want) {
			t.Fatalf("log of the malformed record %q lacks %s", line, want)
		}
	}
}

func TestPublisherBoundsEachAttempt(t *testing.T) {
	store, now := &memStore{}, &clock{now: t0}
	var mu sync.Mutex
	blocked := false
	out := &sink{deliver: func(ctx context.Context, _ outbox.Record) error {
		mu.Lock()
		first := !blocked
		blocked = true
		mu.Unlock()
		if first {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}}
	id := store.add(canvasA, 1, "", t0)
	publisher := start(t, outbox.Config{Store: store, Sink: out, Now: now.Now, PollInterval: time.Hour, AttemptTimeout: 50 * time.Millisecond, MinBackoff: time.Millisecond})

	eventually(t, "timed-out attempt recorded", func() bool { return store.get(id).record.Attempts == 1 })
	if r := store.get(id); r.lastError != context.DeadlineExceeded.Error() || r.outcome != "" {
		t.Fatalf("record after timeout = %+v, want pending with %q", r, context.DeadlineExceeded)
	}
	now.Advance(time.Millisecond)
	publisher.Notify()
	eventually(t, "delivered after timeout", func() bool { return store.get(id).outcome == outbox.OutcomeDelivered })
}

func TestPublisherNotifyWakesBeforeThePollInterval(t *testing.T) {
	store, out, now := &memStore{}, &sink{}, &clock{now: t0}
	publisher := start(t, outbox.Config{Store: store, Sink: out, Now: now.Now, PollInterval: time.Hour})
	time.Sleep(20 * time.Millisecond) // let the first pass find nothing
	id := store.add(canvasA, 1, "", t0)
	publisher.Notify()
	eventually(t, "notified record delivered", func() bool { return store.get(id).outcome == outbox.OutcomeDelivered })
}

func TestPublisherKeepsRunningThroughStoreFailures(t *testing.T) {
	store, out, now := &memStore{failRead: 2}, &sink{}, &clock{now: t0}
	id := store.add(canvasA, 1, "", t0)
	publisher := start(t, outbox.Config{Store: store, Sink: out, Now: now.Now, PollInterval: 5 * time.Millisecond})
	eventually(t, "delivered after the store recovered", func() bool { return store.get(id).outcome == outbox.OutcomeDelivered })
	if c := publisher.Counters(); c.StoreErrors != 2 || c.Delivered != 1 {
		t.Fatalf("Counters = %+v, want 2 store errors and 1 delivered", c)
	}
}

func TestPublisherCloseFinishesTheDeliveryInFlightAndKeepsTheRestPending(t *testing.T) {
	store, now := &memStore{}, &clock{now: t0}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	out := &sink{deliver: func(context.Context, outbox.Record) error {
		once.Do(func() { close(entered) })
		<-release
		return nil
	}}
	first := store.add(canvasA, 1, "", t0)
	second := store.add(canvasA, 2, "", t0)
	publisher := start(t, outbox.Config{Store: store, Sink: out, Now: now.Now, PollInterval: time.Hour})
	<-entered

	closed := make(chan error, 1)
	go func() { closed <- publisher.Close(context.Background()) }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned %v while a delivery was in flight", err)
	case <-time.After(50 * time.Millisecond):
	}
	if publisher.Running() {
		t.Fatal("Running() = true after Close started")
	}
	close(release)
	if err := <-closed; err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}
	if r := store.get(first); r.outcome != outbox.OutcomeDelivered {
		t.Fatalf("in-flight record = %+v, want delivered", r)
	}
	publisher.Notify()
	time.Sleep(20 * time.Millisecond)
	if r := store.get(second); r.outcome != "" || r.record.Attempts != 0 {
		t.Fatalf("record after Close = %+v, want pending and untouched", r)
	}
	if got := out.calls(canvasA); !slices.Equal(got, []int64{1}) {
		t.Fatalf("calls = %v, want only the in-flight delivery", got)
	}
	if err := publisher.Close(context.Background()); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	if err := publisher.Start(); !errors.Is(err, outbox.ErrClosed) {
		t.Fatalf("Start after Close = %v, want ErrClosed", err)
	}
}

func TestPublisherCloseDeadlineCancelsTheDeliveryInFlight(t *testing.T) {
	store, now := &memStore{}, &clock{now: t0}
	entered := make(chan struct{})
	var once sync.Once
	out := &sink{deliver: func(ctx context.Context, _ outbox.Record) error {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return ctx.Err()
	}}
	id := store.add(canvasA, 1, "", t0)
	publisher := start(t, outbox.Config{Store: store, Sink: out, Now: now.Now, PollInterval: time.Hour, AttemptTimeout: time.Hour})
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := publisher.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close = %v, want DeadlineExceeded", err)
	}
	if r := store.get(id); r.outcome != "" || r.record.Attempts != 0 || r.lastError != "" {
		t.Fatalf("record after an aborted Close = %+v, want pending as before", r)
	}
	if c := publisher.Counters(); c != (outbox.Counters{}) {
		t.Fatalf("Counters = %+v, want zero", c)
	}
}

func TestPublisherPurgesSettledRecordsAfterRetention(t *testing.T) {
	store, out, now := &memStore{}, &sink{}, &clock{now: t0}
	old := store.addSettled(t0.Add(-25 * time.Hour))
	recent := store.addSettled(t0.Add(-23 * time.Hour))
	publisher := start(t, outbox.Config{Store: store, Sink: out, Now: now.Now, PollInterval: time.Hour, Retention: 24 * time.Hour})

	eventually(t, "old record purged", func() bool { return publisher.Counters().Purged == 1 })
	if store.get(old).record.ID != 0 {
		t.Fatal("record settled 25h ago was not purged")
	}
	if store.get(recent).record.ID != recent {
		t.Fatal("record settled 23h ago was purged")
	}
	store.mu.Lock()
	cutoff := store.cutoffs[0]
	store.mu.Unlock()
	if want := t0.Add(-24 * time.Hour); !cutoff.Equal(want) {
		t.Fatalf("purge cutoff = %v, want %v", cutoff, want)
	}
}

func TestPublisherLogsTheCorrelationOfEachDelivery(t *testing.T) {
	store, out, now := &memStore{}, &sink{}, &clock{now: t0}
	var buffer bytes.Buffer
	var mu sync.Mutex
	logger := slog.New(slog.NewJSONHandler(&lockedWriter{w: &buffer, mu: &mu}, nil))
	id := store.add(canvasA, 7, "corr-1", t0)
	publisher := start(t, outbox.Config{Store: store, Sink: out, Now: now.Now, PollInterval: time.Hour, Logger: logger})
	eventually(t, "delivered", func() bool { return publisher.Counters().Delivered == 1 })
	mu.Lock()
	line := buffer.String()
	mu.Unlock()
	for _, want := range []string{`"msg":"outbox record delivered"`, `"correlation_id":"corr-1"`, fmt.Sprintf(`"outbox_id":%d`, id), `"tenant_id":"tenant-a"`, `"revision":7`} {
		if !strings.Contains(line, want) {
			t.Fatalf("log %q lacks %s", line, want)
		}
	}
}

type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func TestNewPublisherRejectsInvalidConfig(t *testing.T) {
	store, out := &memStore{}, &sink{}
	cases := map[string]outbox.Config{
		"no store":         {Sink: out},
		"no sink":          {Store: store},
		"negative poll":    {Store: store, Sink: out, PollInterval: -1},
		"negative batch":   {Store: store, Sink: out, BatchSize: -1},
		"min exceeds max":  {Store: store, Sink: out, MinBackoff: time.Second, MaxBackoff: time.Millisecond},
		"negative timeout": {Store: store, Sink: out, AttemptTimeout: -1},
	}
	for name, config := range cases {
		if _, err := outbox.NewPublisher(config); !errors.Is(err, outbox.ErrInvalidConfig) {
			t.Fatalf("%s: NewPublisher error = %v, want ErrInvalidConfig", name, err)
		}
	}
	publisher, err := outbox.NewPublisher(outbox.Config{Store: store, Sink: out})
	if err != nil {
		t.Fatalf("NewPublisher with defaults: %v", err)
	}
	if publisher.Running() {
		t.Fatal("Running() = true before Start")
	}
	if err := publisher.Close(context.Background()); err != nil {
		t.Fatalf("Close before Start = %v", err)
	}
}

func TestUndecodableRecordIsRejectedWithoutStallingOtherCanvases(t *testing.T) {
	server := pgtest.Start(t)
	ctx := context.Background()
	store, events, pool := openPG(t, server)
	for _, key := range []canvas.Key{canvasA, canvasB} {
		if _, err := store.Create(ctx, key); err != nil {
			t.Fatalf("Create(%v): %v", key, err)
		}
		mustCommit(t, store, key, canvas.Operation{ID: "op-1"}, canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: 1280, Height: 720}})
	}
	mustCommit(t, store, canvasA, canvas.Operation{ID: "op-2"}, canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: 640, Height: 480}})
	poison := mustPendingRecords(t, events, canvasA)[0]
	if _, err := pool.Exec(ctx, `UPDATE canvas_outbox SET mutation = '{"width":0}'::jsonb WHERE id = $1`, poison.ID); err != nil {
		t.Fatalf("corrupt record: %v", err)
	}

	hub := newHub(t, realtime.Config{Authorize: allowAll})
	publisher := start(t, outbox.Config{Store: events, Sink: outbox.NewHubSink(hub), PollInterval: time.Hour})
	eventually(t, "both canvases to drain", func() bool {
		return mustStats(t, events, canvasA.TenantID).Pending == 0 && mustStats(t, events, canvasB.TenantID).Pending == 0
	})

	assertDeliveredInOrder(t, hubEvents(t, hub, canvasB), 1, 1)
	assertDeliveredInOrder(t, hubEvents(t, hub, canvasA), 2, 2)
	if stats := mustStats(t, events, canvasA.TenantID); stats.Rejected != 1 {
		t.Fatalf("tenant A stats = %+v, want the corrupt record rejected", stats)
	}
	if stats := mustStats(t, events, canvasB.TenantID); stats.Rejected != 0 {
		t.Fatalf("tenant B stats = %+v, want nothing rejected", stats)
	}
	if c := publisher.Counters(); c != (outbox.Counters{Delivered: 2, Rejected: 1}) {
		t.Fatalf("Counters = %+v, want 2 delivered, 1 rejected, no store errors", c)
	}
}
