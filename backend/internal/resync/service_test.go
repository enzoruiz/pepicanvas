package resync_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
	"github.com/enzoruiz/pepicanvas/backend/internal/resync"
	"github.com/enzoruiz/pepicanvas/backend/internal/revocation"
)

// memStore serves committed canvases from memory and counts reads. onLoad,
// when set, runs inside Load before it answers.
type memStore struct {
	mu       sync.Mutex
	canvases map[canvas.Key]canvas.Canvas
	loads    atomic.Int64
	onLoad   func(ctx context.Context) error
}

func (s *memStore) Load(ctx context.Context, key canvas.Key) (canvas.Canvas, error) {
	s.loads.Add(1)
	if s.onLoad != nil {
		if err := s.onLoad(ctx); err != nil {
			return canvas.Canvas{}, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.canvases[key]
	if !ok {
		return canvas.Canvas{}, canvas.ErrNotFound
	}
	return c, nil
}

// facts returns the current access facts of one actor. Each call reads the
// next scripted answer, repeating the last one.
type facts struct {
	mu      sync.Mutex
	answers []access.Request
	err     error
	calls   atomic.Int64
}

func (f *facts) load(_ context.Context, _ canvas.Key) (access.Request, error) {
	n := f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return access.Request{}, f.err
	}
	return f.answers[min(int(n), len(f.answers))-1], nil
}

func viewer(role access.Role, actorTenant, resourceTenant string) access.Request {
	return access.Request{
		ActorTenantID:    actorTenant,
		ResourceTenantID: resourceTenant,
		Role:             role,
		Resource:         access.ResourceCanvas,
		Action:           access.ActionView,
		Subscription:     access.SubscriptionActive,
		Invitation:       access.InvitationNone,
		Membership:       access.MembershipActive,
	}
}

type nopConn struct{}

func (nopConn) Close(context.Context, revocation.Reason) error { return nil }
func (nopConn) Abort()                                         {}

var editorGrant = revocation.Grant{TenantID: "tenant-a", AccountID: "acct-1", SessionID: "sess-1", Client: revocation.ClientEditor, Version: 5}

type fixture struct {
	store    *memStore
	facts    *facts
	service  *resync.Service
	registry *revocation.Registry
	lease    *revocation.Lease
	stored   canvas.Canvas
}

func newFixture(t *testing.T, answers ...access.Request) *fixture {
	t.Helper()
	if len(answers) == 0 {
		answers = []access.Request{viewer(access.RoleModerator, "tenant-a", "tenant-a")}
	}
	stored := canvas.Canvas{Key: keyA, Dimensions: canvas.DefaultDimensions, Revision: 4, Objects: []canvas.Object{
		{ID: "text", Kind: canvas.KindText, Content: "hello", Transform: place, Z: 1},
		{ID: "video", Kind: canvas.KindVideo, Content: "asset-v", Transform: place, Z: 2},
		{ID: "audio", Kind: canvas.KindAudio, Content: "asset-a", Transform: place, Z: 3},
	}}
	other := stored
	other.Key, other.Objects = keyB, []canvas.Object{{ID: "secret", Kind: canvas.KindText, Content: "tenant b", Transform: place, Z: 1}}
	f := &fixture{
		store:  &memStore{canvases: map[canvas.Key]canvas.Canvas{keyA: stored, keyB: other}},
		facts:  &facts{answers: answers},
		stored: stored,
	}
	service, err := resync.NewService(f.store, f.facts.load)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	f.service = service
	registry, err := revocation.NewRegistry(revocation.Config{})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Close(context.Background()) })
	f.registry = registry
	f.lease = f.register(t, editorGrant)
	return f
}

func (f *fixture) register(t *testing.T, grant revocation.Grant) *revocation.Lease {
	t.Helper()
	lease, err := f.registry.Register(grant, nopConn{})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	return lease
}

func (f *fixture) cut(t *testing.T, version uint64) {
	t.Helper()
	_, err := f.registry.Apply(context.Background(), revocation.Cut{
		OperationID: "cut-1",
		Type:        revocation.TypeRevocation,
		Target:      revocation.Target{Kind: revocation.TargetAccount, TenantID: "tenant-a", ID: "acct-1"},
		Version:     version,
	})
	if err != nil {
		t.Fatalf("Apply cut: %v", err)
	}
}

func newCorrelationID(t *testing.T) string {
	t.Helper()
	id, err := correlation.New()
	if err != nil {
		t.Fatalf("correlation.New: %v", err)
	}
	return id.String()
}

// requireNothing asserts that a failed resync delivered no state.
func requireNothing(t *testing.T, got resync.Snapshot, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("Snapshot error = %v, want %v", err, want)
	}
	if !reflect.DeepEqual(got, resync.Snapshot{}) {
		t.Fatalf("Snapshot delivered %+v with error %v", got, err)
	}
}

func TestNewServiceRequiresItsPorts(t *testing.T) {
	if _, err := resync.NewService(nil, (&facts{}).load); !errors.Is(err, resync.ErrInvalidConfig) {
		t.Fatalf("NewService(nil store) = %v, want ErrInvalidConfig", err)
	}
	if _, err := resync.NewService(&memStore{}, nil); !errors.Is(err, resync.ErrInvalidConfig) {
		t.Fatalf("NewService(nil access) = %v, want ErrInvalidConfig", err)
	}
}

func TestAuthorizedResyncDeliversTheCommittedState(t *testing.T) {
	for _, role := range []access.Role{access.RoleOwner, access.RoleModerator, access.RoleOBS} {
		t.Run(string(role), func(t *testing.T) {
			f := newFixture(t, viewer(role, "tenant-a", "tenant-a"))
			id := newCorrelationID(t)
			got, err := f.service.Snapshot(context.Background(), f.lease, resync.Request{Key: keyA, CorrelationID: id})
			if err != nil {
				t.Fatalf("Snapshot: %v", err)
			}
			if want := resync.NewSnapshot(f.stored, id); !reflect.DeepEqual(got, want) {
				t.Fatalf("Snapshot = %+v, want %+v", got, want)
			}
			if got.Playback["video"] != (resync.Playback{}) || got.Playback["audio"] != (resync.Playback{}) {
				t.Fatalf("media playback = %+v, want stopped at zero", got.Playback)
			}
			if loads, checks := f.store.loads.Load(), f.facts.calls.Load(); loads != 1 || checks != 2 {
				t.Fatalf("loads = %d, access checks = %d; want 1 read between 2 checks", loads, checks)
			}
		})
	}
}

func TestResyncWithoutCorrelationCarriesNone(t *testing.T) {
	f := newFixture(t)
	got, err := f.service.Snapshot(context.Background(), f.lease, resync.Request{Key: keyA})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got.CorrelationID != "" {
		t.Fatalf("CorrelationID = %q, want none", got.CorrelationID)
	}
}

func TestDeniedAccessReadsNothing(t *testing.T) {
	inactive := viewer(access.RoleModerator, "tenant-a", "tenant-a")
	inactive.Membership = access.MembershipInactive
	unsubscribed := viewer(access.RoleOwner, "tenant-a", "tenant-a")
	unsubscribed.Subscription = access.SubscriptionInactive
	edit := viewer(access.RoleModerator, "tenant-a", "tenant-a")
	edit.Action = access.ActionEdit
	membership := viewer(access.RoleOwner, "tenant-a", "tenant-a")
	membership.Resource = access.ResourceMembership
	for name, request := range map[string]access.Request{
		"inactive membership":        inactive,
		"inactive subscription":      unsubscribed,
		"operator role":              viewer(access.RoleOperator, "tenant-a", "tenant-a"),
		"facts for another action":   edit,
		"facts for another resource": membership,
		"facts for another tenant":   viewer(access.RoleOwner, "tenant-b", "tenant-b"),
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, request)
			got, err := f.service.Snapshot(context.Background(), f.lease, resync.Request{Key: keyA})
			requireNothing(t, got, err, access.ErrAccessDenied)
			if loads := f.store.loads.Load(); loads != 0 {
				t.Fatalf("store read %d times after denial", loads)
			}
		})
	}
}

func TestOtherTenantCanvasIsNeverDelivered(t *testing.T) {
	// The actor belongs to tenant A and asks for the equal canvas ID of tenant B.
	f := newFixture(t, viewer(access.RoleOwner, "tenant-a", "tenant-b"))
	got, err := f.service.Snapshot(context.Background(), f.lease, resync.Request{Key: keyB})
	requireNothing(t, got, err, access.ErrAccessDenied)
	if loads := f.store.loads.Load(); loads != 0 {
		t.Fatalf("store read %d times", loads)
	}
}

func TestFactsFailureDenies(t *testing.T) {
	f := newFixture(t)
	f.facts.err = errors.New("membership store unavailable")
	got, err := f.service.Snapshot(context.Background(), f.lease, resync.Request{Key: keyA})
	requireNothing(t, got, err, access.ErrAccessDenied)
	if f.store.loads.Load() != 0 {
		t.Fatal("store was read without facts")
	}
}

func TestRevocationBetweenReadAndDeliveryDeliversNothing(t *testing.T) {
	revoked := viewer(access.RoleModerator, "tenant-a", "tenant-a")
	revoked.Membership = access.MembershipInactive
	f := newFixture(t, viewer(access.RoleModerator, "tenant-a", "tenant-a"), revoked)
	got, err := f.service.Snapshot(context.Background(), f.lease, resync.Request{Key: keyA})
	requireNothing(t, got, err, access.ErrAccessDenied)
	if loads := f.store.loads.Load(); loads != 1 {
		t.Fatalf("loads = %d, want 1", loads)
	}
}

func TestCutLeaseReceivesNoState(t *testing.T) {
	t.Run("cut before resync", func(t *testing.T) {
		f := newFixture(t)
		f.cut(t, 6)
		got, err := f.service.Snapshot(context.Background(), f.lease, resync.Request{Key: keyA})
		requireNothing(t, got, err, revocation.ErrAccessRevoked)
		if f.store.loads.Load() != 0 || f.facts.calls.Load() != 0 {
			t.Fatalf("loads = %d, checks = %d after the cut", f.store.loads.Load(), f.facts.calls.Load())
		}
	})
	t.Run("cut during a read that honors cancellation", func(t *testing.T) {
		f := newFixture(t)
		f.store.onLoad = func(ctx context.Context) error {
			f.cut(t, 6)
			<-ctx.Done()
			return ctx.Err()
		}
		got, err := f.service.Snapshot(context.Background(), f.lease, resync.Request{Key: keyA})
		requireNothing(t, got, err, revocation.ErrAccessRevoked)
	})
	t.Run("cut during a read that ignores cancellation", func(t *testing.T) {
		f := newFixture(t)
		f.store.onLoad = func(context.Context) error {
			f.cut(t, 6)
			return nil
		}
		got, err := f.service.Snapshot(context.Background(), f.lease, resync.Request{Key: keyA})
		requireNothing(t, got, err, revocation.ErrAccessRevoked)
	})
}

func TestReconnection(t *testing.T) {
	f := newFixture(t)
	f.cut(t, 6)
	// A reconnection with the grant read before the cut gets no lease, so no
	// resync can run for it.
	if _, err := f.registry.Register(editorGrant, nopConn{}); !errors.Is(err, revocation.ErrAccessRevoked) {
		t.Fatalf("Register(stale grant) = %v, want ErrAccessRevoked", err)
	}
	// A reconnection authorized after the cut resyncs normally.
	fresh := editorGrant
	fresh.Version = 7
	lease := f.register(t, fresh)
	got, err := f.service.Snapshot(context.Background(), lease, resync.Request{Key: keyA})
	if err != nil {
		t.Fatalf("Snapshot after authorized reconnection: %v", err)
	}
	if want := resync.NewSnapshot(f.stored, ""); !reflect.DeepEqual(got, want) {
		t.Fatalf("Snapshot = %+v, want %+v", got, want)
	}
}

func TestReleasedLeaseReceivesNoState(t *testing.T) {
	f := newFixture(t)
	f.lease.Release()
	got, err := f.service.Snapshot(context.Background(), f.lease, resync.Request{Key: keyA})
	requireNothing(t, got, err, revocation.ErrReleased)
}

func TestShutdown(t *testing.T) {
	t.Run("rejects new resyncs", func(t *testing.T) {
		f := newFixture(t)
		if err := f.registry.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		got, err := f.service.Snapshot(context.Background(), f.lease, resync.Request{Key: keyA})
		requireNothing(t, got, err, revocation.ErrClosed)
		if f.store.loads.Load() != 0 {
			t.Fatal("store read after shutdown")
		}
	})
	t.Run("cancels a resync in flight", func(t *testing.T) {
		f := newFixture(t)
		f.store.onLoad = func(ctx context.Context) error {
			go func() { _ = f.registry.Close(context.Background()) }()
			<-ctx.Done()
			return ctx.Err()
		}
		got, err := f.service.Snapshot(context.Background(), f.lease, resync.Request{Key: keyA})
		requireNothing(t, got, err, revocation.ErrClosed)
	})
}

func TestInvalidRequestsAreRejectedFirst(t *testing.T) {
	f := newFixture(t)
	got, err := f.service.Snapshot(context.Background(), f.lease, resync.Request{Key: canvas.Key{TenantID: "tenant-a"}})
	requireNothing(t, got, err, canvas.ErrInvalid)
	if !errors.Is(err, resync.ErrInvalidRequest) {
		t.Fatalf("invalid key error = %v, want ErrInvalidRequest", err)
	}
	for _, id := range []string{"has space", strings.Repeat("c", 129), "tab\t"} {
		got, err = f.service.Snapshot(context.Background(), f.lease, resync.Request{Key: keyA, CorrelationID: id})
		requireNothing(t, got, err, resync.ErrInvalidRequest)
	}
	got, err = f.service.Snapshot(context.Background(), nil, resync.Request{Key: keyA})
	requireNothing(t, got, err, access.ErrAccessDenied)
	if f.store.loads.Load() != 0 || f.facts.calls.Load() != 0 {
		t.Fatal("an invalid request reached access or the store")
	}
}

func TestMissingCanvasIsNotFound(t *testing.T) {
	f := newFixture(t)
	missing := canvas.Key{TenantID: "tenant-a", CanvasID: "canvas-9"}
	got, err := f.service.Snapshot(context.Background(), f.lease, resync.Request{Key: missing})
	requireNothing(t, got, err, canvas.ErrNotFound)
}

func TestRetriesReturnTheSameSnapshot(t *testing.T) {
	f := newFixture(t)
	block := make(chan struct{})
	f.store.onLoad = func(ctx context.Context) error {
		select {
		case <-block:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	got, err := f.service.Snapshot(ctx, f.lease, resync.Request{Key: keyA})
	requireNothing(t, got, err, context.DeadlineExceeded)

	close(block)
	first, err := f.service.Snapshot(context.Background(), f.lease, resync.Request{Key: keyA})
	if err != nil {
		t.Fatalf("retry after timeout: %v", err)
	}
	second, err := f.service.Snapshot(context.Background(), f.register(t, editorGrant), resync.Request{Key: keyA})
	if err != nil {
		t.Fatalf("retry after reconnection: %v", err)
	}
	want := resync.NewSnapshot(f.stored, "")
	if !reflect.DeepEqual(first, want) || !reflect.DeepEqual(second, want) {
		t.Fatalf("retries = %+v and %+v, want %+v", first, second, want)
	}
}
