package resync_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/pgtest"
	"github.com/enzoruiz/pepicanvas/backend/internal/resync"
	"github.com/enzoruiz/pepicanvas/backend/internal/revocation"
)

// openPG connects to the current server address and migrates it.
func openPG(t *testing.T, server *pgtest.Server) (*postgres.CanvasStore, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	pool, err := postgres.Open(ctx, server.DSN(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return postgres.NewCanvasStore(pool), pool
}

func pgLoad(t *testing.T, store canvas.Store, key canvas.Key) canvas.Canvas {
	t.Helper()
	loaded, err := store.Load(context.Background(), key)
	if err != nil {
		t.Fatalf("Load(%+v): %v", key, err)
	}
	return loaded
}

var pgOperations atomic.Int64

func pgApply(t *testing.T, store canvas.Store, key canvas.Key, mutation canvas.Mutation) canvas.Result {
	t.Helper()
	result, err := store.Apply(context.Background(), key, canvas.Operation{ID: fmt.Sprintf("op-%d", pgOperations.Add(1))}, mutation)
	if err != nil {
		t.Fatalf("Apply(%T): %v", mutation, err)
	}
	return result
}

// allowOwn returns the access facts of an active owner of tenant: it allows
// viewing that tenant's canvases and nothing of another tenant.
func allowOwn(tenant string) resync.Access {
	return func(_ context.Context, key canvas.Key) (access.Request, error) {
		return access.Request{
			ActorTenantID:    tenant,
			ResourceTenantID: key.TenantID,
			Role:             access.RoleOwner,
			Resource:         access.ResourceCanvas,
			Action:           access.ActionView,
			Subscription:     access.SubscriptionActive,
			Invitation:       access.InvitationNone,
			Membership:       access.MembershipActive,
		}, nil
	}
}

func newPGRegistry(t *testing.T) *revocation.Registry {
	t.Helper()
	registry, err := revocation.NewRegistry(revocation.Config{})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Close(context.Background()) })
	return registry
}

func pgGrant(tenant, account string) revocation.Grant {
	return revocation.Grant{TenantID: tenant, AccountID: account, SessionID: "sess-" + account, Client: revocation.ClientEditor, Version: 5}
}

func pgRegister(t *testing.T, registry *revocation.Registry, grant revocation.Grant) *revocation.Lease {
	t.Helper()
	lease, err := registry.Register(grant, nopConn{})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	return lease
}

func newPGService(t *testing.T, loader resync.Loader, facts resync.Access) *resync.Service {
	t.Helper()
	service, err := resync.NewService(loader, facts)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return service
}

// storedFacts reads what a resync must never change: the canvas revision
// and its retained operations.
func storedFacts(t *testing.T, pool *pgxpool.Pool, key canvas.Key) [2]int64 {
	t.Helper()
	var revision, operations int64
	err := pool.QueryRow(context.Background(), `
		SELECT c.revision, (SELECT count(*) FROM canvas_operations o WHERE o.tenant_id = c.tenant_id AND o.canvas_id = c.canvas_id)
		FROM canvases c WHERE c.tenant_id = $1 AND c.canvas_id = $2`, key.TenantID, key.CanvasID).Scan(&revision, &operations)
	if err != nil {
		t.Fatalf("read stored facts: %v", err)
	}
	return [2]int64{revision, operations}
}

// seedEveryKind commits one mutation of every kind and leaves media objects.
func seedEveryKind(t *testing.T, store canvas.Store, key canvas.Key) {
	t.Helper()
	if _, err := store.Create(context.Background(), key); err != nil {
		t.Fatalf("Create: %v", err)
	}
	text := pgApply(t, store, key, canvas.CreateObject{Kind: canvas.KindText, Content: "hello", Transform: place})
	image := pgApply(t, store, key, canvas.CreateObject{Kind: canvas.KindImage, Content: "asset-i", Transform: place})
	pgApply(t, store, key, canvas.CreateObject{Kind: canvas.KindVideo, Content: "asset-v", Transform: place})
	pgApply(t, store, key, canvas.CreateObject{Kind: canvas.KindAudio, Content: "asset-a", Transform: place})
	pgApply(t, store, key, canvas.SetContent{ObjectID: text.ObjectID, Content: "edited"})
	pgApply(t, store, key, canvas.SetTransform{ObjectID: text.ObjectID, Transform: canvas.Transform{X: -3, Y: 4, Width: 9, Height: 8, Rotation: 12.5}})
	pgApply(t, store, key, canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: 640, Height: 360}})
	pgApply(t, store, key, canvas.DeleteObject{ObjectID: image.ObjectID})
}

// recordingStore stands in for the TE-012 publisher: every mutation the store
// commits is published as a resync.Event numbered by its revision. Events of
// concurrent writers can be published out of revision order.
type recordingStore struct {
	*postgres.CanvasStore
	mu        sync.Mutex
	committed []resync.Event
	subs      []chan resync.Event
}

func (r *recordingStore) subscribe(buffer int) <-chan resync.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	sub := make(chan resync.Event, buffer)
	r.subs = append(r.subs, sub)
	return sub
}

func (r *recordingStore) Apply(ctx context.Context, key canvas.Key, operation canvas.Operation, mutation canvas.Mutation) (canvas.Result, error) {
	result, err := r.CanvasStore.Apply(ctx, key, operation, mutation)
	if err != nil {
		return result, err
	}
	event := resync.Event{Key: key, Mutation: mutation, Result: result, CorrelationID: operation.CorrelationID}
	r.mu.Lock()
	r.committed = append(r.committed, event)
	subs := r.subs
	r.mu.Unlock()
	for _, sub := range subs {
		sub <- event
	}
	return result, nil
}

func (r *recordingStore) closeSubs() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, sub := range r.subs {
		close(sub)
	}
}

const (
	writersPerTenant  = 3
	mutationsPerWrite = 40
	replicasPerTenant = 3
)

// write commits mutationsPerWrite mutations of every kind on objects this
// writer created, pausing briefly so snapshots interleave with commits.
func write(t *testing.T, store canvas.Store, key canvas.Key, rng *rand.Rand, writer int) {
	kinds := []canvas.Kind{canvas.KindText, canvas.KindImage, canvas.KindVideo, canvas.KindAudio}
	var own []string
	for step := range mutationsPerWrite {
		var mutation canvas.Mutation
		action := step % 7
		if action >= 3 && len(own) == 0 {
			action = 0
		}
		transform := canvas.Transform{X: rng.IntN(200) - 100, Y: rng.IntN(200) - 100, Width: 1 + rng.IntN(500), Height: 1 + rng.IntN(500), Rotation: float64(rng.IntN(360))}
		switch action {
		case 0, 1, 2:
			mutation = canvas.CreateObject{Kind: kinds[(step+writer)%len(kinds)], Content: fmt.Sprintf("w%d-s%d", writer, step), Transform: transform}
		case 3:
			mutation = canvas.SetContent{ObjectID: own[rng.IntN(len(own))], Content: fmt.Sprintf("edit-w%d-s%d", writer, step)}
		case 4:
			mutation = canvas.SetTransform{ObjectID: own[rng.IntN(len(own))], Transform: transform}
		case 5:
			mutation = canvas.SetDimensions{Dimensions: canvas.Dimensions{Width: canvas.MinWidth + rng.IntN(1000), Height: canvas.MinHeight + rng.IntN(1000)}}
		default:
			i := rng.IntN(len(own))
			mutation = canvas.DeleteObject{ObjectID: own[i]}
			own = append(own[:i], own[i+1:]...)
		}
		operation := canvas.Operation{ID: fmt.Sprintf("w%d-s%d-%d", writer, step, pgOperations.Add(1)), CorrelationID: fmt.Sprintf("corr-w%d-s%d", writer, step)}
		result, err := store.Apply(context.Background(), key, operation, mutation)
		if err != nil {
			t.Errorf("writer %d step %d Apply(%T): %v", writer, step, mutation, err)
			return
		}
		if _, ok := mutation.(canvas.CreateObject); ok {
			own = append(own, result.ObjectID)
		}
		time.Sleep(time.Duration(rng.IntN(1500)) * time.Microsecond)
	}
}

// follower is one client replica fed by a faulty transport.
type follower struct {
	replica     *resync.Replica
	key         canvas.Key
	lease       *revocation.Lease
	service     *resync.Service
	rng         *rand.Rand
	resyncs     int
	concurrent  int // snapshots requested and answered while writers committed
	writing     *atomic.Bool
	correlation string
}

func (f *follower) resync(t *testing.T) bool {
	before := f.writing.Load()
	snapshot, err := f.service.Snapshot(context.Background(), f.lease, resync.Request{Key: f.key, CorrelationID: f.correlation})
	if err != nil {
		t.Errorf("Snapshot(%+v): %v", f.key, err)
		return false
	}
	if snapshot.CorrelationID != f.correlation || snapshot.Canvas.Key != f.key {
		t.Errorf("snapshot key %+v correlation %q, want %+v %q", snapshot.Canvas.Key, snapshot.CorrelationID, f.key, f.correlation)
		return false
	}
	if err := f.replica.Adopt(snapshot); err != nil {
		t.Errorf("Adopt: %v", err)
		return false
	}
	f.resyncs++
	if before && f.writing.Load() {
		f.concurrent++
	}
	return true
}

func (f *follower) deliver(t *testing.T, event resync.Event) bool {
	if err := f.replica.Receive(event); err != nil {
		t.Errorf("Receive(revision %d): %v", event.Result.Revision, err)
		return false
	}
	return true
}

// follow consumes the transport: about 20% of events are lost, 20% are
// duplicated, and the rest are held and released in random order. The
// replica resyncs whenever it reports a discontinuity.
func (f *follower) follow(t *testing.T, inbox <-chan resync.Event) {
	var held []resync.Event
	release := func(all bool) bool {
		for len(held) > 0 && (all || len(held) > 4 || f.rng.IntN(3) == 0) {
			i := f.rng.IntN(len(held))
			event := held[i]
			held = append(held[:i], held[i+1:]...)
			if !f.deliver(t, event) {
				return false
			}
		}
		return true
	}
	if !f.resync(t) {
		return
	}
	for event := range inbox {
		switch roll := f.rng.IntN(10); {
		case roll < 2: // lost
		case roll < 4:
			held = append(held, event, event)
		default:
			held = append(held, event)
		}
		if !release(false) {
			return
		}
		if f.replica.NeedsResync() && !f.resync(t) {
			return
		}
	}
	release(true)
}

func TestReplicasConvergeOverPostgresUnderFaultyDelivery(t *testing.T) {
	server := pgtest.Start(t)
	store, _ := openPG(t, server)
	registry := newPGRegistry(t)

	for _, seed := range []uint64{1, 7, 42, 1001, 65535} {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			// Both tenants use the same canvas ID.
			canvasID := fmt.Sprintf("board-%d", seed)
			tenants := []string{"pg-tenant-a", "pg-tenant-b"}
			recorders := map[string]*recordingStore{}
			followers := map[string][]*follower{}
			inboxes := map[*follower]<-chan resync.Event{}
			var writing atomic.Bool
			writing.Store(true)
			for ti, tenant := range tenants {
				key := canvas.Key{TenantID: tenant, CanvasID: canvasID}
				if _, err := store.Create(context.Background(), key); err != nil {
					t.Fatalf("Create: %v", err)
				}
				recorder := &recordingStore{CanvasStore: store}
				recorders[tenant] = recorder
				service := newPGService(t, store, allowOwn(tenant))
				for i := range replicasPerTenant {
					f := &follower{
						replica:     resync.NewReplica(key),
						key:         key,
						lease:       pgRegister(t, registry, pgGrant(tenant, fmt.Sprintf("acct-%d-%d", seed, i))),
						service:     service,
						rng:         rand.New(rand.NewPCG(seed, uint64(ti*10+i))),
						writing:     &writing,
						correlation: fmt.Sprintf("resync-%s-%d", tenant, i),
					}
					followers[tenant] = append(followers[tenant], f)
					inboxes[f] = recorder.subscribe(writersPerTenant * mutationsPerWrite)
				}
			}

			var readers sync.WaitGroup
			for _, fs := range followers {
				for _, f := range fs {
					readers.Go(func() { f.follow(t, inboxes[f]) })
				}
			}
			var writers sync.WaitGroup
			for ti, tenant := range tenants {
				key := canvas.Key{TenantID: tenant, CanvasID: canvasID}
				for w := range writersPerTenant {
					rng := rand.New(rand.NewPCG(seed, uint64(1000+ti*10+w)))
					writers.Go(func() { write(t, recorders[tenant], key, rng, w) })
				}
			}
			writers.Wait()
			writing.Store(false)
			for _, recorder := range recorders {
				recorder.closeSubs()
			}
			readers.Wait()
			if t.Failed() {
				t.FailNow()
			}

			concurrent := 0
			for ti, tenant := range tenants {
				key := canvas.Key{TenantID: tenant, CanvasID: canvasID}
				committed := pgLoad(t, store, key)
				if got, want := committed.Revision, int64(len(recorders[tenant].committed)); got != want || want != writersPerTenant*mutationsPerWrite {
					t.Fatalf("%s committed revision %d, published %d events, want %d", tenant, got, want, writersPerTenant*mutationsPerWrite)
				}
				want := resync.NewSnapshot(committed, "")
				foreign := recorders[tenants[1-ti]].committed[0]
				for i, f := range followers[tenant] {
					// A heartbeat announces the final revision; a lost tail resyncs.
					f.replica.Notice(committed.Revision)
					if f.replica.NeedsResync() && !f.resync(t) {
						t.FailNow()
					}
					if got := f.replica.State(); !reflect.DeepEqual(got, want) || f.replica.NeedsResync() {
						t.Fatalf("%s replica %d state = %+v (needs resync %v), want %+v", tenant, i, got, f.replica.NeedsResync(), want)
					}
					if err := f.replica.Receive(foreign); !errors.Is(err, resync.ErrForeign) {
						t.Fatalf("%s replica %d Receive(foreign) = %v, want ErrForeign", tenant, i, err)
					}
					if err := f.replica.Adopt(resync.NewSnapshot(pgLoad(t, store, foreign.Key), "")); !errors.Is(err, resync.ErrForeign) {
						t.Fatalf("%s replica %d Adopt(foreign) = %v, want ErrForeign", tenant, i, err)
					}
					if got := f.replica.State(); !reflect.DeepEqual(got, want) || f.replica.NeedsResync() {
						t.Fatalf("%s replica %d changed after foreign state: %+v", tenant, i, got)
					}
					concurrent += f.concurrent
					t.Logf("%s replica %d: %d resyncs, %d concurrent with writers", tenant, i, f.resyncs, f.concurrent)
				}
			}
			if concurrent == 0 {
				t.Fatal("no snapshot was taken while writers committed")
			}
		})
	}
}

func TestSnapshotNeverSeesAnUncommittedMutation(t *testing.T) {
	server := pgtest.Start(t)
	store, pool := openPG(t, server)
	key := canvas.Key{TenantID: "pg-tenant-a", CanvasID: "board"}
	seedEveryKind(t, store, key)
	service := newPGService(t, store, allowOwn(key.TenantID))
	lease := pgRegister(t, newPGRegistry(t), pgGrant(key.TenantID, "acct-1"))
	request := resync.Request{Key: key, CorrelationID: "corr-uncommitted"}
	want := resync.NewSnapshot(pgLoad(t, store, key), request.CorrelationID)
	stored := storedFacts(t, pool, key)

	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "UPDATE canvases SET revision = revision + 1, width = 3840, next_z = next_z + 1 WHERE tenant_id = $1 AND canvas_id = $2", key.TenantID, key.CanvasID); err != nil {
		t.Fatalf("update canvas: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO canvas_objects (tenant_id, canvas_id, object_id, kind, content, x, y, width, height, rotation, z)
		VALUES ($1, $2, 'uncommitted', 'text', 'invisible', 0, 0, 1, 1, 0, 999)`, key.TenantID, key.CanvasID); err != nil {
		t.Fatalf("insert object: %v", err)
	}

	// The open transaction holds the canvas row lock; the snapshot neither
	// waits for it nor sees its writes.
	snapshotCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	start := time.Now()
	got, err := service.Snapshot(snapshotCtx, lease, request)
	if err != nil {
		t.Fatalf("Snapshot during an open transaction: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Snapshot waited %v for the open transaction", elapsed)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Snapshot during an open transaction = %+v, want the committed %+v", got, want)
	}

	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	got, err = service.Snapshot(ctx, lease, request)
	if err != nil {
		t.Fatalf("Snapshot after rollback: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Snapshot after rollback = %+v, want %+v", got, want)
	}
	if after := storedFacts(t, pool, key); after != stored {
		t.Fatalf("stored revision and operations = %v, want %v", after, stored)
	}
}

// slowLoader delays before reading, without watching ctx while it waits.
type slowLoader struct {
	inner resync.Loader
	delay atomic.Int64 // nanoseconds
}

func (l *slowLoader) Load(ctx context.Context, key canvas.Key) (canvas.Canvas, error) {
	time.Sleep(time.Duration(l.delay.Load()))
	return l.inner.Load(ctx, key)
}

func TestResyncRetriesAfterTimeoutAndRestartReturnTheSameSnapshot(t *testing.T) {
	server := pgtest.Start(t)
	store, pool := openPG(t, server)
	key := canvas.Key{TenantID: "pg-tenant-a", CanvasID: "board"}
	seedEveryKind(t, store, key)
	stored := storedFacts(t, pool, key)
	request := resync.Request{Key: key, CorrelationID: "corr-retry"}
	want := resync.NewSnapshot(pgLoad(t, store, key), request.CorrelationID)
	if len(want.Playback) != 2 {
		t.Fatalf("seed playback = %+v, want the video and audio stopped", want.Playback)
	}

	loader := &slowLoader{inner: store}
	loader.delay.Store(int64(100 * time.Millisecond))
	service := newPGService(t, loader, allowOwn(key.TenantID))
	lease := pgRegister(t, newPGRegistry(t), pgGrant(key.TenantID, "acct-1"))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	got, err := service.Snapshot(ctx, lease, request)
	requireNothing(t, got, err, context.DeadlineExceeded)

	loader.delay.Store(0)
	retry, err := service.Snapshot(context.Background(), lease, request)
	if err != nil {
		t.Fatalf("retry after timeout: %v", err)
	}
	if !reflect.DeepEqual(retry, want) {
		t.Fatalf("retry after timeout = %+v, want %+v", retry, want)
	}
	if after := storedFacts(t, pool, key); after != stored {
		t.Fatalf("stored revision and operations after retries = %v, want %v", after, stored)
	}

	// Reconnection after a server restart: new pool, store, service,
	// registry, and lease.
	server.Restart(t)
	restarted, restartedPool := openPG(t, server)
	service = newPGService(t, restarted, allowOwn(key.TenantID))
	lease = pgRegister(t, newPGRegistry(t), pgGrant(key.TenantID, "acct-1"))
	for i := range 2 {
		again, err := service.Snapshot(context.Background(), lease, request)
		if err != nil {
			t.Fatalf("retry %d after restart: %v", i, err)
		}
		if !reflect.DeepEqual(again, want) {
			t.Fatalf("retry %d after restart = %+v, want %+v", i, again, want)
		}
	}
	if after := storedFacts(t, restartedPool, key); after != stored {
		t.Fatalf("stored revision and operations after restart = %v, want %v", after, stored)
	}
}

// cuttingLoader cuts the lease while a real Load runs: before it reads, or
// after it read and before the service delivers.
type cuttingLoader struct {
	inner      resync.Loader
	cut        func()
	afterRead  bool
	loaded     atomic.Int64
	loadErrors atomic.Int64
}

func (l *cuttingLoader) Load(ctx context.Context, key canvas.Key) (canvas.Canvas, error) {
	if !l.afterRead {
		l.cut()
	}
	loaded, err := l.inner.Load(ctx, key)
	if err != nil {
		l.loadErrors.Add(1)
	} else {
		l.loaded.Add(1)
	}
	if l.afterRead {
		l.cut()
	}
	return loaded, err
}

func TestLeaseCutDuringARealLoadDeliversNothing(t *testing.T) {
	server := pgtest.Start(t)
	store, pool := openPG(t, server)
	key := canvas.Key{TenantID: "pg-tenant-a", CanvasID: "board"}
	seedEveryKind(t, store, key)
	stored := storedFacts(t, pool, key)

	for name, afterRead := range map[string]bool{"before the read": false, "after the read": true} {
		t.Run(name, func(t *testing.T) {
			registry := newPGRegistry(t)
			grant := pgGrant(key.TenantID, "acct-cut")
			lease := pgRegister(t, registry, grant)
			loader := &cuttingLoader{inner: store, afterRead: afterRead}
			loader.cut = func() {
				_, err := registry.Apply(context.Background(), revocation.Cut{
					OperationID: "cut-" + name[:5],
					Type:        revocation.TypeRevocation,
					Target:      revocation.Target{Kind: revocation.TargetAccount, TenantID: grant.TenantID, ID: grant.AccountID},
					Version:     grant.Version + 1,
				})
				if err != nil {
					t.Errorf("Apply cut: %v", err)
				}
			}
			service := newPGService(t, loader, allowOwn(key.TenantID))
			got, err := service.Snapshot(context.Background(), lease, resync.Request{Key: key, CorrelationID: "corr-cut"})
			requireNothing(t, got, err, revocation.ErrAccessRevoked)
			if afterRead && loader.loaded.Load() != 1 {
				t.Fatalf("the store answered %d times before the cut, want 1", loader.loaded.Load())
			}
			if !afterRead && loader.loaded.Load()+loader.loadErrors.Load() != 1 {
				t.Fatalf("Load ran %d times, want 1", loader.loaded.Load()+loader.loadErrors.Load())
			}
			// The cut lease stays cut; a retry delivers nothing and reads nothing.
			got, err = service.Snapshot(context.Background(), lease, resync.Request{Key: key})
			requireNothing(t, got, err, revocation.ErrAccessRevoked)
			if total := loader.loaded.Load() + loader.loadErrors.Load(); total != 1 {
				t.Fatalf("Load ran %d times after the cut, want 1", total)
			}
			if after := storedFacts(t, pool, key); after != stored {
				t.Fatalf("stored revision and operations = %v, want %v", after, stored)
			}
		})
	}
}
