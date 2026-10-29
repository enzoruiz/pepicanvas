package httpapi_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/locking"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpapi"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/locksession"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/pgtest"
	revoke "github.com/enzoruiz/pepicanvas/backend/internal/revocation"
	"github.com/enzoruiz/pepicanvas/backend/internal/subscription"
)

// withLocks wires the lock service and the cut relay over PostgreSQL, as
// cmd/server wires them, and serves p's API with them.
func (p *pgEnv) withLocks() *locksession.Directory {
	p.t.Helper()
	accounts := postgres.NewAccountStore(p.pool)
	manager, err := locking.NewManager(locking.Config{Authorize: httpapi.LockAuthorizer(accounts)})
	if err != nil {
		p.t.Fatal(err)
	}
	registry, err := revoke.NewRegistry(revoke.Config{})
	if err != nil {
		p.t.Fatal(err)
	}
	directory, err := locksession.New(locksession.Config{Manager: manager, Registry: registry})
	if err != nil {
		p.t.Fatal(err)
	}
	subscriptions := postgres.NewSubscriptionStore(p.pool)
	relay, err := subscription.NewCutRelay(subscription.RelayConfig{Store: subscriptions, Cutter: registry})
	if err != nil {
		p.t.Fatal(err)
	}
	if err := relay.Start(); err != nil {
		p.t.Fatal(err)
	}
	t := p.t
	t.Cleanup(func() {
		ctx := context.Background()
		if err := relay.Close(ctx); err != nil {
			t.Errorf("relay Close: %v", err)
		}
		if err := directory.Close(ctx); err != nil {
			t.Errorf("directory Close: %v", err)
		}
		if err := manager.Close(ctx); err != nil {
			t.Errorf("manager Close: %v", err)
		}
		if err := registry.Close(ctx); err != nil {
			t.Errorf("registry Close: %v", err)
		}
	})
	p.configure = func(config *httpapi.Config) {
		config.Subscriptions = subscription.NewService(subscriptions, subscription.WithCommitted(relay.Notify))
		config.Locks = &httpapi.Locks{Directory: directory, Manager: manager, Grants: accounts}
	}
	p.serve(p.canvases)
	return directory
}

// S1, S2, S4, S8 with real mutations: a protected mutation commits through
// the gate only for the holder of the object's lock, the other participant
// is rejected without a write, creating an object needs no lock, and after a
// deactivation cut no protected commit goes through.
func TestPostgresLockedMutationsCommitOnlyForTheHolder(t *testing.T) {
	p := openPGEnv(t, pgtest.Start(t))
	directory := p.withLocks()
	op := p.operator("operator@example.com")
	alice := p.streamer(op, "alice1", "alice@example.com")
	bob := p.moderator(alice, "bob@example.com")
	aliceID, bobID := p.http.sessionAccount(alice.signedIn), p.http.sessionAccount(bob)
	key := alice.key()
	ctx := context.Background()
	operations := 0
	next := func() canvas.Operation {
		operations++
		return canvas.Operation{ID: fmt.Sprintf("op-pg-lock-mut-%03d", operations)}
	}
	// commit runs mutation through the gate in the lock session id.
	commit := func(id, account string, mutation canvas.Mutation) (canvas.Result, error) {
		var result canvas.Result
		err := directory.Use(ctx, id, key.TenantID, account, func(s *locking.Session) error {
			var err error
			result, err = locking.ApplyLocked(ctx, s, p.canvases, key, next(), mutation)
			return err
		})
		return result, err
	}
	expectRevision := func(want int64) {
		t.Helper()
		if got := p.load(alice).Revision; got != want {
			t.Fatalf("revision = %d, want %d", got, want)
		}
	}

	aliceLock := p.http.openLock(alice.signedIn, alice.path)
	bobLock := p.http.openLock(bob, alice.path)
	created, err := commit(bobLock, bobID, canvas.CreateObject{Kind: canvas.KindText, Content: "hello", Transform: canvas.Transform{Width: 100, Height: 40}})
	if err != nil || created.Revision != 1 {
		t.Fatalf("unlocked CreateObject = %+v, %v; want revision 1", created, err)
	}
	object := created.ObjectID
	res := p.http.lockAction(alice.signedIn, alice.path, aliceLock, "acquire", acquiring("op-pg-lock-acq-001", object))
	if got := decode(t, res); res.StatusCode != http.StatusOK || got["outcome"] != "granted" {
		t.Fatalf("acquire = %d %v, want granted", res.StatusCode, got)
	}

	protected := []canvas.Mutation{
		canvas.SetTransform{ObjectID: object, Transform: canvas.Transform{X: 10, Y: 20, Width: 120, Height: 50, Rotation: 15}},
		canvas.SetContent{ObjectID: object, Content: "edited"},
		canvas.DeleteObject{ObjectID: object},
	}
	for i, mutation := range protected {
		if _, err := commit(bobLock, bobID, mutation); !errors.Is(err, locking.ErrNotHolder) {
			t.Fatalf("%T by the other participant: %v, want ErrNotHolder", mutation, err)
		}
		expectRevision(int64(1 + i))
		result, err := commit(aliceLock, aliceID, mutation)
		if err != nil || result.Revision != int64(2+i) {
			t.Fatalf("%T by the holder = %+v, %v; want revision %d", mutation, result, err, 2+i)
		}
	}
	if n := p.count("SELECT count(*) FROM canvas_objects WHERE tenant_id = $1 AND canvas_id = $2", key.TenantID, key.CanvasID); n != 0 {
		t.Fatalf("objects after the holder's delete = %d, want 0", n)
	}

	second, err := commit(aliceLock, aliceID, canvas.CreateObject{Kind: canvas.KindText, Content: "second", Transform: canvas.Transform{Width: 10, Height: 10}})
	if err != nil {
		t.Fatal(err)
	}
	p.http.lockAction(alice.signedIn, alice.path, aliceLock, "acquire", acquiring("op-pg-lock-acq-002", second.ObjectID))
	var session *locking.Session
	if err := directory.Use(ctx, aliceLock, key.TenantID, aliceID, func(s *locking.Session) error { session = s; return nil }); err != nil {
		t.Fatal(err)
	}

	p.http.deactivate(op, "op-pg-lock-deact-01", "alice@example.com")
	pgEventually(t, "the deactivation cut to end the lock session", func() bool {
		return errors.Is(directory.Use(ctx, aliceLock, key.TenantID, aliceID, func(*locking.Session) error { return nil }), locksession.ErrGone)
	})
	if _, err := locking.ApplyLocked(ctx, session, p.canvases, key, next(), canvas.SetContent{ObjectID: second.ObjectID, Content: "late"}); !errors.Is(err, locking.ErrDetached) {
		t.Fatalf("protected commit after the cut: %v, want ErrDetached", err)
	}
	if _, err := locking.ApplyLocked(ctx, nil, p.canvases, key, next(), canvas.CreateObject{Kind: canvas.KindText, Content: "late", Transform: canvas.Transform{Width: 10, Height: 10}}); !errors.Is(err, canvas.ErrInactive) {
		t.Fatalf("unprotected commit after the deactivation: %v, want ErrInactive", err)
	}
	expectRevision(second.Revision)
	if _, err := commit(bobLock, bobID, canvas.DeleteObject{ObjectID: second.ObjectID}); !errors.Is(err, locksession.ErrGone) {
		t.Fatalf("the moderator's session after the cut: %v, want ErrGone", err)
	}
}
