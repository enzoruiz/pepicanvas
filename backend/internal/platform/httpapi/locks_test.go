package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/locking"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpapi"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpx"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/ingress"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/locksession"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/ratelimit"
	revoke "github.com/enzoruiz/pepicanvas/backend/internal/revocation"
)

// Lock tests run inside a synctest bubble, so time is virtual: every lease
// and idle deadline is exact, and requests served without sleeping share one
// instant.

var lockSessionPattern = regexp.MustCompile(`^pls_[0-9a-f]{32}$`)

// lockOptions configure the lock service of one env; zero keeps the
// defaults.
type lockOptions struct {
	// ttl is both the lease TTL and the lock session idle time.
	ttl       time.Duration
	retention locking.Retention
}

// lockFixture is the lock service behind one env: a real manager, directory,
// and registry, closed when the test ends.
type lockFixture struct {
	manager   *locking.Manager
	directory *locksession.Directory
	registry  *revoke.Registry
	// versions holds each tenant's confirmed access version; absent is 1.
	versions *sync.Map
}

// memberGrants reads the env's memberships at each tenant's access version.
type memberGrants struct {
	memberships access.MembershipStore
	versions    *sync.Map
}

func (g memberGrants) MemberGrant(ctx context.Context, account access.AccountID, tenantID string) (access.Membership, uint64, bool, error) {
	membership, found, err := g.memberships.MembershipOf(ctx, account, tenantID)
	version := uint64(1)
	if v, ok := g.versions.Load(tenantID); ok {
		version = v.(uint64)
	}
	return membership, version, found, err
}

// countedSessions counts every directory call that can change a lock
// session or a lock, so tests can prove a rejected request never reached it.
type countedSessions struct {
	*locksession.Directory
	calls *calls
}

func (c countedSessions) Open(ctx context.Context, grant revoke.Grant, name string) (string, error) {
	c.calls.add("LockOpen")
	return c.Directory.Open(ctx, grant, name)
}

func (c countedSessions) Use(ctx context.Context, id, tenantID, accountID string, fn func(*locking.Session) error) error {
	c.calls.add("LockUse")
	return c.Directory.Use(ctx, id, tenantID, accountID, fn)
}

func (c countedSessions) End(ctx context.Context, id, tenantID, accountID string) {
	c.calls.add("LockEnd")
	c.Directory.End(ctx, id, tenantID, accountID)
}

// withLocks serves the env's API with the lock service, wired as cmd/server
// wires it.
func (e *env) withLocks(o lockOptions) lockFixture {
	e.t.Helper()
	manager, err := locking.NewManager(locking.Config{
		Authorize: httpapi.LockAuthorizer(e.config.Memberships),
		TTL:       o.ttl,
		Retention: o.retention,
		Logger:    e.config.Logger,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	registry, err := revoke.NewRegistry(revoke.Config{Logger: e.config.Logger})
	if err != nil {
		e.t.Fatal(err)
	}
	directory, err := locksession.New(locksession.Config{Manager: manager, Registry: registry, Idle: o.ttl, Logger: e.config.Logger})
	if err != nil {
		e.t.Fatal(err)
	}
	t := e.t
	t.Cleanup(func() {
		ctx := context.Background()
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
	x := lockFixture{manager: manager, directory: directory, registry: registry, versions: &sync.Map{}}
	e.config.Locks = &httpapi.Locks{
		Directory: countedSessions{Directory: directory, calls: e.calls},
		Manager:   manager,
		Grants:    memberGrants{memberships: e.config.Memberships, versions: x.versions},
	}
	handler, err := httpapi.New(e.config)
	if err != nil {
		e.t.Fatal(err)
	}
	e.handler = handler
	return x
}

// newLockEnv is an env with locks whose ingress budgets outlast a test's
// many lock requests on the fake clock.
func newLockEnv(t *testing.T, o lockOptions) (*env, lockFixture) {
	t.Helper()
	limits := ingress.DefaultLimits
	limits.UnsafePerIP = ratelimit.Rate{Limit: 10_000, Window: time.Minute}
	limits.MutationsPerSession = ratelimit.Rate{Limit: 10_000, Window: time.Minute}
	e := newEnvWith(t, httpapi.DefaultLimits, limits)
	return e, e.withLocks(o)
}

// lockParticipants are alice, the owner of a canvas, and bob, its moderator.
type lockParticipants struct {
	op    signedIn
	alice streamer
	bob   signedIn
	bobID access.AccountID
}

func (e *env) lockParticipants() lockParticipants {
	e.t.Helper()
	op := e.operator("op-http-lock-oper01", "operator@example.com")
	alice := e.streamer(op, "alice1", "alice@example.com")
	bobID := e.register("op-http-lock-bob001", "bob@example.com")
	e.moderators.Store([2]string{alice.tenantID, string(bobID)}, true)
	return lockParticipants{op: op, alice: alice, bob: e.signIn("bob@example.com"), bobID: bobID}
}

func lockSessionPath(path, id string) string { return path + "/locks/sessions/" + id }

func acquiring(op, object string) map[string]string {
	return map[string]string{"operation_id": op, "object_id": object}
}

func releasing(op, object, reason string) map[string]string {
	return map[string]string{"operation_id": op, "object_id": object, "reason": reason}
}

// openLock opens a lock session on the canvas at path and checks the answer.
func (e *env) openLock(s signedIn, path string) string {
	e.t.Helper()
	res := e.do(http.MethodPost, path+"/locks/sessions", nil, withSession(s.token), withCSRF(s.csrf))
	if res.StatusCode != http.StatusCreated {
		e.t.Fatalf("open lock session = %d %s", res.StatusCode, e.bodies[len(e.bodies)-1])
	}
	body := decode(e.t, res)
	id, _ := body["lock_session_id"].(string)
	if !lockSessionPattern.MatchString(id) || body["heartbeat_interval_ms"] != float64(2000) || body["ttl_ms"] != float64(5000) || len(body) != 3 {
		e.t.Fatalf("open lock session body = %v", body)
	}
	return id
}

// lockAction posts one lock action of the lock session id.
func (e *env) lockAction(s signedIn, path, id, action string, body any) *http.Response {
	e.t.Helper()
	return e.do(http.MethodPost, lockSessionPath(path, id)+"/"+action, body, withSession(s.token), withCSRF(s.csrf))
}

func (e *env) endLock(s signedIn, path, id string) *http.Response {
	e.t.Helper()
	return e.do(http.MethodDelete, lockSessionPath(path, id), nil, withSession(s.token), withCSRF(s.csrf))
}

// expectBody checks the status and the exact body of the latest response.
func (e *env) expectBody(res *http.Response, status int, want string) {
	e.t.Helper()
	if got := e.bodies[len(e.bodies)-1]; res.StatusCode != status || got != want+"\n" {
		e.t.Fatalf("answer = %d %s, want %d %s", res.StatusCode, got, status, want)
	}
}

// expectLocks checks the exact lock list s reads on the canvas at path.
func (e *env) expectLocks(s signedIn, path, want string) {
	e.t.Helper()
	e.expectBody(e.do(http.MethodGet, path+"/locks", nil, withSession(s.token)), http.StatusOK, want)
}

func granted(op, object, name string, mine bool, expiresMS int) string {
	return fmt.Sprintf(`{"operation_id":%q,"outcome":"granted","object_id":%q,"holder":{"name":%q,"mine":%t},"expires_in_ms":%d}`, op, object, name, mine, expiresMS)
}

func denied(op, object, name string, expiresMS int) string {
	return fmt.Sprintf(`{"operation_id":%q,"outcome":"denied","object_id":%q,"holder":{"name":%q,"mine":false},"expires_in_ms":%d}`, op, object, name, expiresMS)
}

func released(op, object, reason string) string {
	return fmt.Sprintf(`{"operation_id":%q,"outcome":"released","reason":%q,"object_id":%q}`, op, reason, object)
}

func lockOf(object, name string, mine bool) string {
	return fmt.Sprintf(`{"object_id":%q,"holder":{"name":%q,"mine":%t}}`, object, name, mine)
}

func lockList(entries ...string) string {
	return `{"locks":[` + strings.Join(entries, ",") + `]}`
}

// expectNamedError is expectError for one case of a table; subtests cannot
// run inside a synctest bubble.
func expectNamedError(t *testing.T, name string, res *http.Response, status int, code string) {
	t.Helper()
	if res.StatusCode != status {
		t.Fatalf("%s: status = %d, want %d", name, res.StatusCode, status)
	}
	expectError(t, res, status, code)
}

const noRenewal = `{"renewed":[],"lost":[]}`

// S3: the first accepted request holds the lock and every member sees its
// owner; two simultaneous requests produce exactly one holder.
func TestOneVisibleHolder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, _ := newLockEnv(t, lockOptions{})
		p := e.lockParticipants()
		path := p.alice.path
		aliceLock := e.openLock(p.alice.signedIn, path)
		bobLock := e.openLock(p.bob, path)

		e.expectBody(e.lockAction(p.alice.signedIn, path, aliceLock, "acquire", acquiring("op-lock-s3-000001", "obj-0")),
			http.StatusOK, granted("op-lock-s3-000001", "obj-0", "alice@example.com", true, 5000))
		e.expectLocks(p.alice.signedIn, path, lockList(lockOf("obj-0", "alice@example.com", true)))
		e.expectLocks(p.bob, path, lockList(lockOf("obj-0", "alice@example.com", false)))

		type attempt struct {
			s       signedIn
			id, op  string
			name    string
			outcome sent
		}
		attempts := []*attempt{
			{s: p.alice.signedIn, id: aliceLock, op: "op-lock-s3-000002", name: "alice@example.com"},
			{s: p.bob, id: bobLock, op: "op-lock-s3-000003", name: "bob@example.com"},
		}
		var wg sync.WaitGroup
		for _, a := range attempts {
			wg.Go(func() {
				a.outcome = send(e.handler, http.MethodPost, lockSessionPath(path, a.id)+"/acquire", acquiring(a.op, "obj-1"), withSession(a.s.token), withCSRF(a.s.csrf))
			})
		}
		wg.Wait()
		var winner, loser *attempt
		for _, a := range attempts {
			if a.outcome.status != http.StatusOK {
				t.Fatalf("acquire = %d %s", a.outcome.status, a.outcome.body)
			}
			if a.outcome.body == granted(a.op, "obj-1", a.name, true, 5000)+"\n" {
				if winner != nil {
					t.Fatal("two simultaneous acquires were both granted")
				}
				winner = a
			} else {
				loser = a
			}
		}
		if winner == nil || loser == nil {
			t.Fatalf("answers = %s / %s, want one granted and one denied", attempts[0].outcome.body, attempts[1].outcome.body)
		}
		if want := denied(loser.op, "obj-1", winner.name, 5000) + "\n"; loser.outcome.body != want {
			t.Fatalf("denied answer = %s, want %s", loser.outcome.body, want)
		}
		e.expectLocks(winner.s, path, lockList(lockOf("obj-0", "alice@example.com", winner == attempts[0]), lockOf("obj-1", winner.name, true)))
		e.expectLocks(loser.s, path, lockList(lockOf("obj-0", "alice@example.com", loser == attempts[0]), lockOf("obj-1", winner.name, false)))
	})
}

// S4: another participant cannot take or release a held object, but locks
// a different one.
func TestOtherParticipantsLockOtherObjects(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, _ := newLockEnv(t, lockOptions{})
		p := e.lockParticipants()
		path := p.alice.path
		aliceLock := e.openLock(p.alice.signedIn, path)
		bobLock := e.openLock(p.bob, path)
		e.expectBody(e.lockAction(p.alice.signedIn, path, aliceLock, "acquire", acquiring("op-lock-s4-000001", "obj-1")),
			http.StatusOK, granted("op-lock-s4-000001", "obj-1", "alice@example.com", true, 5000))

		e.expectBody(e.lockAction(p.bob, path, bobLock, "acquire", acquiring("op-lock-s4-000002", "obj-1")),
			http.StatusOK, denied("op-lock-s4-000002", "obj-1", "alice@example.com", 5000))
		expectError(t, e.lockAction(p.bob, path, bobLock, "release", releasing("op-lock-s4-000003", "obj-1", "finished")), http.StatusConflict, "not_lock_holder")
		e.expectLocks(p.bob, path, lockList(lockOf("obj-1", "alice@example.com", false)))

		e.expectBody(e.lockAction(p.bob, path, bobLock, "acquire", acquiring("op-lock-s4-000004", "obj-2")),
			http.StatusOK, granted("op-lock-s4-000004", "obj-2", "bob@example.com", true, 5000))
		e.expectLocks(p.alice.signedIn, path, lockList(lockOf("obj-1", "alice@example.com", true), lockOf("obj-2", "bob@example.com", false)))
	})
}

// S5: finishing or cancelling frees the lock at once.
func TestFinishAndCancelReleaseAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, _ := newLockEnv(t, lockOptions{})
		p := e.lockParticipants()
		path := p.alice.path
		aliceLock := e.openLock(p.alice.signedIn, path)
		bobLock := e.openLock(p.bob, path)

		e.lockAction(p.alice.signedIn, path, aliceLock, "acquire", acquiring("op-lock-s5-000001", "obj-1"))
		e.expectBody(e.lockAction(p.alice.signedIn, path, aliceLock, "release", releasing("op-lock-s5-000002", "obj-1", "finished")),
			http.StatusOK, released("op-lock-s5-000002", "obj-1", "finished"))
		e.expectLocks(p.bob, path, lockList())

		e.expectBody(e.lockAction(p.bob, path, bobLock, "acquire", acquiring("op-lock-s5-000003", "obj-1")),
			http.StatusOK, granted("op-lock-s5-000003", "obj-1", "bob@example.com", true, 5000))
		e.expectBody(e.lockAction(p.bob, path, bobLock, "release", releasing("op-lock-s5-000004", "obj-1", "cancelled")),
			http.StatusOK, released("op-lock-s5-000004", "obj-1", "cancelled"))
		e.expectLocks(p.alice.signedIn, path, lockList())
		e.expectBody(e.lockAction(p.alice.signedIn, path, aliceLock, "acquire", acquiring("op-lock-s5-000005", "obj-1")),
			http.StatusOK, granted("op-lock-s5-000005", "obj-1", "alice@example.com", true, 5000))
	})
}

// S6, S11: heartbeats keep the lock; without one it is free exactly 5 s
// after the last accepted one, with no request from anybody.
func TestHeartbeatKeepsTheLockAndSilenceFreesItAtFiveSeconds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, _ := newLockEnv(t, lockOptions{})
		p := e.lockParticipants()
		path := p.alice.path
		aliceLock := e.openLock(p.alice.signedIn, path)
		bobLock := e.openLock(p.bob, path)
		e.lockAction(p.alice.signedIn, path, aliceLock, "acquire", acquiring("op-lock-s6-000001", "obj-1"))

		for range 3 {
			time.Sleep(4 * time.Second)
			e.expectBody(e.lockAction(p.alice.signedIn, path, aliceLock, "heartbeat", nil),
				http.StatusOK, `{"renewed":[{"object_id":"obj-1","expires_in_ms":5000}],"lost":[]}`)
			e.lockAction(p.bob, path, bobLock, "heartbeat", nil)
		}
		time.Sleep(5*time.Second - time.Millisecond)
		e.expectBody(e.lockAction(p.bob, path, bobLock, "acquire", acquiring("op-lock-s6-000002", "obj-1")),
			http.StatusOK, denied("op-lock-s6-000002", "obj-1", "alice@example.com", 1))
		e.expectLocks(p.bob, path, lockList(lockOf("obj-1", "alice@example.com", false)))

		time.Sleep(time.Millisecond)
		synctest.Wait()
		e.expectLocks(p.bob, path, lockList())
		e.expectBody(e.lockAction(p.bob, path, bobLock, "acquire", acquiring("op-lock-s6-000003", "obj-1")),
			http.StatusOK, granted("op-lock-s6-000003", "obj-1", "bob@example.com", true, 5000))
	})
}

// S7: a lock session that went silent for 5 s or was ended is rejected,
// and a new session never gets its locks back.
func TestReconnectingNeverRestoresALock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, _ := newLockEnv(t, lockOptions{})
		p := e.lockParticipants()
		path := p.alice.path

		ended := e.openLock(p.alice.signedIn, path)
		e.lockAction(p.alice.signedIn, path, ended, "acquire", acquiring("op-lock-s7-000001", "obj-1"))
		for range 2 {
			if res := e.endLock(p.alice.signedIn, path, ended); res.StatusCode != http.StatusNoContent {
				t.Fatalf("end lock session = %d, want 204", res.StatusCode)
			}
		}
		e.expectLocks(p.bob, path, lockList())

		idle := e.openLock(p.alice.signedIn, path)
		e.lockAction(p.alice.signedIn, path, idle, "acquire", acquiring("op-lock-s7-000002", "obj-2"))
		time.Sleep(5 * time.Second)
		synctest.Wait()
		e.expectLocks(p.bob, path, lockList())

		for _, old := range []string{ended, idle} {
			expectError(t, e.lockAction(p.alice.signedIn, path, old, "heartbeat", nil), http.StatusGone, "lock_session_gone")
			expectError(t, e.lockAction(p.alice.signedIn, path, old, "acquire", acquiring("op-lock-s7-000003", "obj-3")), http.StatusGone, "lock_session_gone")
			expectError(t, e.lockAction(p.alice.signedIn, path, old, "release", releasing("op-lock-s7-000004", "obj-2", "finished")), http.StatusGone, "lock_session_gone")
		}
		e.expectLocks(p.bob, path, lockList())

		fresh := e.openLock(p.alice.signedIn, path)
		e.expectBody(e.lockAction(p.alice.signedIn, path, fresh, "heartbeat", nil), http.StatusOK, noRenewal)
		e.expectLocks(p.alice.signedIn, path, lockList())
	})
}

// S8: an access cut or a revoked membership frees the member's locks and
// rejects its later lock requests.
func TestAccessCutsFreeLocks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, x := newLockEnv(t, lockOptions{})
		p := e.lockParticipants()
		path := p.alice.path
		aliceLock := e.openLock(p.alice.signedIn, path)
		bobLock := e.openLock(p.bob, path)
		e.lockAction(p.alice.signedIn, path, aliceLock, "acquire", acquiring("op-lock-s8-000001", "obj-a"))
		e.lockAction(p.bob, path, bobLock, "acquire", acquiring("op-lock-s8-000002", "obj-b"))

		if _, err := x.registry.Apply(context.Background(), revoke.Cut{
			OperationID: "op-lock-s8-cut-01",
			Type:        revoke.TypeDeactivation,
			Target:      revoke.Target{Kind: revoke.TargetTenant, TenantID: p.alice.tenantID},
			Version:     2,
		}); err != nil {
			t.Fatalf("Apply cut: %v", err)
		}
		e.expectLocks(p.alice.signedIn, path, lockList())
		expectError(t, e.lockAction(p.alice.signedIn, path, aliceLock, "heartbeat", nil), http.StatusGone, "lock_session_gone")
		expectError(t, e.lockAction(p.bob, path, bobLock, "acquire", acquiring("op-lock-s8-000003", "obj-b")), http.StatusGone, "lock_session_gone")
		// A grant read before the cut is rejected by the registry.
		before := e.effects()
		expectError(t, e.do(http.MethodPost, path+"/locks/sessions", nil, withSession(p.bob.token), withCSRF(p.bob.csrf)), http.StatusForbidden, "forbidden")
		if got := e.callsTo("LockOpen"); got != before.calls["LockOpen"]+1 {
			t.Fatalf("LockOpen calls = %d, want %d", got, before.calls["LockOpen"]+1)
		}
		e.expectLocks(p.alice.signedIn, path, lockList())

		x.versions.Store(p.alice.tenantID, uint64(2))
		bobLock = e.openLock(p.bob, path)
		e.expectBody(e.lockAction(p.bob, path, bobLock, "acquire", acquiring("op-lock-s8-000004", "obj-b")),
			http.StatusOK, granted("op-lock-s8-000004", "obj-b", "bob@example.com", true, 5000))

		// The manager's own reauthorization ends a lock on a heartbeat once
		// the membership is gone.
		e.moderators.Delete([2]string{p.alice.tenantID, string(p.bobID)})
		var renewal locking.Renewal
		if err := x.directory.Use(context.Background(), bobLock, p.alice.tenantID, string(p.bobID), func(s *locking.Session) error {
			var err error
			renewal, err = s.Heartbeat(context.Background())
			return err
		}); err != nil {
			t.Fatalf("Heartbeat: %v", err)
		}
		if len(renewal.Renewed) != 0 || len(renewal.Lost) != 1 || renewal.Lost[0].Reason != locking.ReasonUnauthorized {
			t.Fatalf("renewal = %+v, want obj-b lost as unauthorized", renewal)
		}
		e.expectLocks(p.alice.signedIn, path, lockList())

		expectError(t, e.lockAction(p.bob, path, bobLock, "acquire", acquiring("op-lock-s8-000005", "obj-b")), http.StatusForbidden, "forbidden")
		e.moderators.Store([2]string{p.alice.tenantID, string(p.bobID)}, true)
		expectError(t, e.lockAction(p.bob, path, bobLock, "heartbeat", nil), http.StatusGone, "lock_session_gone")
	})
}

// S9: locks of tenant A are invisible to and unaffected by members of
// tenant B, and equal object IDs in two tenants are independent.
func TestTenantsHaveIndependentLocks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, _ := newLockEnv(t, lockOptions{})
		p := e.lockParticipants()
		carol := e.streamer(p.op, "carol1", "carol@example.com")
		a, b := p.alice.path, carol.path
		aliceLock := e.openLock(p.alice.signedIn, a)
		e.lockAction(p.alice.signedIn, a, aliceLock, "acquire", acquiring("op-lock-s9-000001", "obj-1"))
		carolLock := e.openLock(carol.signedIn, b)

		before := e.effects()
		for name, res := range map[string]func() *http.Response{
			"open": func() *http.Response {
				return e.do(http.MethodPost, a+"/locks/sessions", nil, withSession(carol.token), withCSRF(carol.csrf))
			},
			"list": func() *http.Response { return e.do(http.MethodGet, a+"/locks", nil, withSession(carol.token)) },
			"acquire with A's ID": func() *http.Response {
				return e.lockAction(carol.signedIn, a, aliceLock, "acquire", acquiring("op-lock-s9-000002", "obj-1"))
			},
			"release with A's ID": func() *http.Response {
				return e.lockAction(carol.signedIn, a, aliceLock, "release", releasing("op-lock-s9-000003", "obj-1", "finished"))
			},
			"heartbeat with A's ID": func() *http.Response { return e.lockAction(carol.signedIn, a, aliceLock, "heartbeat", nil) },
			"acquire with her own ID": func() *http.Response {
				return e.lockAction(carol.signedIn, a, carolLock, "acquire", acquiring("op-lock-s9-000004", "obj-1"))
			},
			"alice's ID on B's canvas": func() *http.Response { return e.lockAction(p.alice.signedIn, b, aliceLock, "heartbeat", nil) },
			"alice lists B's canvas":   func() *http.Response { return e.do(http.MethodGet, b+"/locks", nil, withSession(p.alice.token)) },
			"alice acquires on B, too": func() *http.Response {
				return e.lockAction(p.alice.signedIn, b, carolLock, "acquire", acquiring("op-lock-s9-000005", "obj-1"))
			},
		} {
			expectNamedError(t, name, res(), http.StatusForbidden, "forbidden")
		}
		if got := e.callsTo("LockOpen") + e.callsTo("LockUse"); got != before.calls["LockOpen"]+before.calls["LockUse"] {
			t.Fatal("a cross-tenant request reached a lock session")
		}
		// Ending another tenant's session ID is a no-op.
		if res := e.endLock(carol.signedIn, a, aliceLock); res.StatusCode != http.StatusNoContent {
			t.Fatalf("end = %d, want 204", res.StatusCode)
		}
		e.expectBody(e.lockAction(p.alice.signedIn, a, aliceLock, "heartbeat", nil), http.StatusOK, `{"renewed":[{"object_id":"obj-1","expires_in_ms":5000}],"lost":[]}`)
		e.expectBody(e.lockAction(carol.signedIn, b, carolLock, "acquire", acquiring("op-lock-s9-000001", "obj-1")),
			http.StatusOK, granted("op-lock-s9-000001", "obj-1", "carol@example.com", true, 5000))
		e.expectLocks(p.alice.signedIn, a, lockList(lockOf("obj-1", "alice@example.com", true)))
		e.expectLocks(carol.signedIn, b, lockList(lockOf("obj-1", "carol@example.com", true)))
		e.expectBody(e.lockAction(p.alice.signedIn, a, aliceLock, "release", releasing("op-lock-s9-000006", "obj-1", "finished")),
			http.StatusOK, released("op-lock-s9-000006", "obj-1", "finished"))
		e.expectLocks(carol.signedIn, b, lockList(lockOf("obj-1", "carol@example.com", true)))
	})
}

// S10: every lock action reauthorizes the actor; one that lost access is
// denied and its locks are freed at once. Answers name the holder by its
// account email and carry no secret or identifier of anybody.
func TestEveryLockActionReauthorizes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, _ := newLockEnv(t, lockOptions{})
		p := e.lockParticipants()
		path := p.alice.path
		aliceID := e.sessionAccount(p.alice.signedIn)
		first := len(e.bodies)
		aliceLock := e.openLock(p.alice.signedIn, path)
		bobLock := e.openLock(p.bob, path)
		e.lockAction(p.alice.signedIn, path, aliceLock, "acquire", acquiring("op-lock-s10-00001", "obj-a"))
		e.lockAction(p.bob, path, bobLock, "acquire", acquiring("op-lock-s10-00002", "obj-b"))
		e.lockAction(p.bob, path, bobLock, "acquire", acquiring("op-lock-s10-00003", "obj-a"))
		e.expectLocks(p.bob, path, lockList(lockOf("obj-a", "alice@example.com", false), lockOf("obj-b", "bob@example.com", true)))

		e.moderators.Delete([2]string{p.alice.tenantID, string(p.bobID)})
		expectError(t, e.lockAction(p.bob, path, bobLock, "release", releasing("op-lock-s10-00004", "obj-b", "finished")), http.StatusForbidden, "forbidden")
		e.expectLocks(p.alice.signedIn, path, lockList(lockOf("obj-a", "alice@example.com", true)))
		for name, res := range map[string]func() *http.Response{
			"open": func() *http.Response {
				return e.do(http.MethodPost, path+"/locks/sessions", nil, withSession(p.bob.token), withCSRF(p.bob.csrf))
			},
			"heartbeat": func() *http.Response { return e.lockAction(p.bob, path, bobLock, "heartbeat", nil) },
			"acquire": func() *http.Response {
				return e.lockAction(p.bob, path, bobLock, "acquire", acquiring("op-lock-s10-00005", "obj-c"))
			},
			"release": func() *http.Response {
				return e.lockAction(p.bob, path, bobLock, "release", releasing("op-lock-s10-00006", "obj-b", "finished"))
			},
			"list": func() *http.Response { return e.do(http.MethodGet, path+"/locks", nil, withSession(p.bob.token)) },
		} {
			expectNamedError(t, name, res(), http.StatusForbidden, "forbidden")
		}
		e.expectLocks(p.alice.signedIn, path, lockList(lockOf("obj-a", "alice@example.com", true)))
		e.moderators.Store([2]string{p.alice.tenantID, string(p.bobID)}, true)
		expectError(t, e.lockAction(p.bob, path, bobLock, "heartbeat", nil), http.StatusGone, "lock_session_gone")

		bodies := e.bodies[first:]
		for name, s := range map[string]string{
			"alice token": p.alice.token, "alice CSRF": p.alice.csrf, "bob token": p.bob.token, "bob CSRF": p.bob.csrf,
			"alice account ID": aliceID, "bob account ID": string(p.bobID),
		} {
			for _, body := range bodies {
				if strings.Contains(body, s) {
					t.Errorf("a lock answer contains the %s: %s", name, body)
				}
			}
		}
		for name, id := range map[string]string{"alice": aliceLock, "bob": bobLock} {
			n := 0
			for _, body := range bodies {
				if strings.Contains(body, id) {
					n++
				}
			}
			if n != 1 {
				t.Errorf("%s's lock session ID appears in %d answers, want only its open answer", name, n)
			}
		}
	})
}

// S12, CTRL-IDEM-01: a retried acquire or release, also from a new lock
// session after a reconnection, has one effect and the same answer; another
// intent under the same operation ID is a conflict; after the retention the
// operation ID is a new intent.
func TestLockRetriesHaveOneEffect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, _ := newLockEnv(t, lockOptions{retention: locking.Retention{TTL: time.Minute}})
		p := e.lockParticipants()
		path := p.alice.path
		first := e.openLock(p.alice.signedIn, path)
		bobLock := e.openLock(p.bob, path)

		const acquire, release = "op-lock-s12-00001", "op-lock-s12-00002"
		answer := granted(acquire, "obj-1", "alice@example.com", true, 5000)
		e.expectBody(e.lockAction(p.alice.signedIn, path, first, "acquire", acquiring(acquire, "obj-1")), http.StatusOK, answer)
		e.expectBody(e.lockAction(p.alice.signedIn, path, first, "acquire", acquiring(acquire, "obj-1")), http.StatusOK, answer)
		before := e.effects()
		expectError(t, e.lockAction(p.alice.signedIn, path, first, "acquire", acquiring(acquire, "obj-2")), http.StatusConflict, "conflict")
		e.expectLocks(p.bob, path, lockList(lockOf("obj-1", "alice@example.com", false)))
		if got := e.callsTo("LockUse"); got != before.calls["LockUse"]+1 {
			t.Fatalf("LockUse calls = %d, want %d", got, before.calls["LockUse"]+1)
		}

		e.expectBody(e.lockAction(p.alice.signedIn, path, first, "release", releasing(release, "obj-1", "finished")), http.StatusOK, released(release, "obj-1", "finished"))
		e.expectBody(e.lockAction(p.bob, path, bobLock, "acquire", acquiring("op-lock-s12-00003", "obj-1")),
			http.StatusOK, granted("op-lock-s12-00003", "obj-1", "bob@example.com", true, 5000))
		// The retried release has no second effect: bob keeps the lock.
		e.expectBody(e.lockAction(p.alice.signedIn, path, first, "release", releasing(release, "obj-1", "finished")), http.StatusOK, released(release, "obj-1", "finished"))
		expectError(t, e.lockAction(p.alice.signedIn, path, first, "release", releasing(release, "obj-1", "cancelled")), http.StatusConflict, "conflict")
		e.expectLocks(p.alice.signedIn, path, lockList(lockOf("obj-1", "bob@example.com", false)))

		// After a reconnection the retries answer the same, without effect.
		e.endLock(p.alice.signedIn, path, first)
		second := e.openLock(p.alice.signedIn, path)
		e.expectBody(e.lockAction(p.alice.signedIn, path, second, "acquire", acquiring(acquire, "obj-1")), http.StatusOK, answer)
		e.expectBody(e.lockAction(p.alice.signedIn, path, second, "release", releasing(release, "obj-1", "finished")), http.StatusOK, released(release, "obj-1", "finished"))
		e.expectLocks(p.alice.signedIn, path, lockList(lockOf("obj-1", "bob@example.com", false)))
		e.expectBody(e.lockAction(p.alice.signedIn, path, second, "heartbeat", nil), http.StatusOK, noRenewal)

		time.Sleep(time.Minute)
		third := e.openLock(p.alice.signedIn, path)
		e.expectBody(e.lockAction(p.alice.signedIn, path, third, "acquire", acquiring(acquire, "obj-2")),
			http.StatusOK, granted(acquire, "obj-2", "alice@example.com", true, 5000))
		e.expectLocks(p.alice.signedIn, path, lockList(lockOf("obj-2", "alice@example.com", true)))
	})
}

// S14, CTRL-TRACE-01 primitive: the acquire record carries the request's
// correlation ID and never the holder's identifiers.
func TestLockRecordsCarryTheRequestCorrelation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, _ := newLockEnv(t, lockOptions{})
		p := e.lockParticipants()
		path := p.alice.path
		aliceID := e.sessionAccount(p.alice.signedIn)
		res := e.do(http.MethodPost, path+"/locks/sessions", nil, withSession(p.alice.token), withCSRF(p.alice.csrf))
		opened := res.Header.Get(httpx.CorrelationHeader)
		aliceLock := decode(t, res)["lock_session_id"].(string)
		res = e.lockAction(p.alice.signedIn, path, aliceLock, "acquire", acquiring("op-lock-s14-00001", "obj-1"))
		acquired := res.Header.Get(httpx.CorrelationHeader)

		logs := e.logs.String()
		lockRecords := 0
		for _, line := range strings.Split(logs, "\n") {
			var r map[string]any
			if json.Unmarshal([]byte(line), &r) != nil {
				continue
			}
			if msg, _ := r["msg"].(string); strings.HasPrefix(msg, "lease ") || strings.HasPrefix(msg, "lock session ") {
				lockRecords++
				for name, s := range map[string]string{"account ID": aliceID, "lock session ID": aliceLock} {
					if strings.Contains(line, s) {
						t.Errorf("a lock record contains the %s: %s", name, line)
					}
				}
			}
		}
		if lockRecords == 0 {
			t.Fatal("no lock record was logged")
		}
		if logLine(logs, "lock session opened", opened) == nil {
			t.Fatalf("no lock session record with the open correlation %s\n%s", opened, logs)
		}
		record := logLine(logs, "lease acquire", acquired)
		if record == nil {
			t.Fatalf("no acquire record with the request correlation %s\n%s", acquired, logs)
		}
		if record["outcome"] != "granted" || record["object_id"] != "obj-1" || record["tenant_id"] != p.alice.tenantID {
			t.Fatalf("acquire record = %v", record)
		}
		for _, key := range []string{"account_id", "session_id", "holder", "name"} {
			if _, ok := record[key]; ok {
				t.Errorf("acquire record carries %s: %v", key, record)
			}
		}
		for name, s := range map[string]string{
			"email": "alice@example.com", "session token": p.alice.token, "CSRF token": p.alice.csrf,
		} {
			if strings.Contains(logs, s) {
				t.Errorf("logs contain the %s", name)
			}
		}
	})
}

// S15, CTRL-READY-01 primitive: without a lock service, or after it closed,
// every lock route answers a retryable unavailable error without effect.
func TestLockServiceUnavailable(t *testing.T) {
	t.Run("not configured", func(t *testing.T) {
		e := newEnv(t, httpapi.DefaultLimits)
		op := e.operator("op-http-lock-oper01", "operator@example.com")
		alice := e.streamer(op, "alice1", "alice@example.com")
		id := "pls_" + strings.Repeat("0", 32)
		auth := []option{withSession(alice.token), withCSRF(alice.csrf)}
		for name, res := range map[string]*http.Response{
			"open":      e.do(http.MethodPost, alice.path+"/locks/sessions", nil, auth...),
			"heartbeat": e.do(http.MethodPost, lockSessionPath(alice.path, id)+"/heartbeat", nil, auth...),
			"acquire":   e.do(http.MethodPost, lockSessionPath(alice.path, id)+"/acquire", acquiring("op-lock-s15-00001", "obj-1"), auth...),
			"release":   e.do(http.MethodPost, lockSessionPath(alice.path, id)+"/release", releasing("op-lock-s15-00002", "obj-1", "finished"), auth...),
			"end":       e.do(http.MethodDelete, lockSessionPath(alice.path, id), nil, auth...),
			"list":      e.do(http.MethodGet, alice.path+"/locks", nil, withSession(alice.token)),
		} {
			t.Run(name, func(t *testing.T) {
				expectError(t, res, http.StatusServiceUnavailable, "lock_unavailable")
			})
		}
	})
	t.Run("closed", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e, x := newLockEnv(t, lockOptions{})
			p := e.lockParticipants()
			path := p.alice.path
			aliceLock := e.openLock(p.alice.signedIn, path)
			e.lockAction(p.alice.signedIn, path, aliceLock, "acquire", acquiring("op-lock-s15-00003", "obj-1"))
			if err := x.directory.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			expectError(t, e.do(http.MethodPost, path+"/locks/sessions", nil, withSession(p.bob.token), withCSRF(p.bob.csrf)), http.StatusServiceUnavailable, "lock_unavailable")
			expectError(t, e.lockAction(p.alice.signedIn, path, aliceLock, "acquire", acquiring("op-lock-s15-00004", "obj-2")), http.StatusServiceUnavailable, "lock_unavailable")
			expectError(t, e.lockAction(p.alice.signedIn, path, aliceLock, "heartbeat", nil), http.StatusServiceUnavailable, "lock_unavailable")
			e.expectLocks(p.bob, path, lockList())
		})
	})
}

// Malformed lock requests are rejected before any lock changes.
func TestMalformedLockRequestsHaveNoEffect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, _ := newLockEnv(t, lockOptions{})
		p := e.lockParticipants()
		path := p.alice.path
		aliceLock := e.openLock(p.alice.signedIn, path)
		e.lockAction(p.alice.signedIn, path, aliceLock, "acquire", acquiring("op-lock-bad-00001", "obj-1"))
		want := lockList(lockOf("obj-1", "alice@example.com", true))

		auth := []option{withSession(p.alice.token), withCSRF(p.alice.csrf)}
		acquire := lockSessionPath(path, aliceLock) + "/acquire"
		release := lockSessionPath(path, aliceLock) + "/release"
		for name, c := range map[string]struct {
			path string
			body string
		}{
			"malformed session ID":  {lockSessionPath(path, "pls_"+strings.Repeat("Z", 32)) + "/acquire", `{"operation_id":"op-lock-bad-00002","object_id":"obj-2"}`},
			"token-shaped ID":       {lockSessionPath(path, strings.TrimPrefix(p.alice.token, "pcs_")) + "/heartbeat", `{}`},
			"unknown field":         {acquire, `{"operation_id":"op-lock-bad-00003","object_id":"obj-2","canvas_id":"other"}`},
			"missing object":        {acquire, `{"operation_id":"op-lock-bad-00004"}`},
			"missing operation":     {acquire, `{"object_id":"obj-2"}`},
			"spaced object":         {acquire, `{"operation_id":"op-lock-bad-00005","object_id":"obj 2"}`},
			"not JSON":              {acquire, `operation_id=op-lock-bad-00006`},
			"trailing data":         {acquire, `{"operation_id":"op-lock-bad-00007","object_id":"obj-2"} {}`},
			"unknown release cause": {release, `{"operation_id":"op-lock-bad-00008","object_id":"obj-1","reason":"expired"}`},
			"missing release cause": {release, `{"operation_id":"op-lock-bad-00009","object_id":"obj-1"}`},
		} {
			expectNamedError(t, name, e.do(http.MethodPost, c.path, nil, append([]option{withBody("application/json", c.body)}, auth...)...), http.StatusBadRequest, "bad_request")
			e.expectLocks(p.alice.signedIn, path, want)
		}
		// No rejected operation ID was retained.
		e.expectBody(e.lockAction(p.alice.signedIn, path, aliceLock, "acquire", acquiring("op-lock-bad-00004", "obj-2")),
			http.StatusOK, granted("op-lock-bad-00004", "obj-2", "alice@example.com", true, 5000))
		e.expectBody(e.lockAction(p.alice.signedIn, path, aliceLock, "release", releasing("op-lock-bad-00008", "obj-1", "finished")),
			http.StatusOK, released("op-lock-bad-00008", "obj-1", "finished"))
	})
}

// A failing membership read answers internal and leaves the lock session
// and its locks in place.
func TestLockMembershipFailureIsInternal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e, x := newLockEnv(t, lockOptions{})
		p := e.lockParticipants()
		path := p.alice.path
		aliceLock := e.openLock(p.alice.signedIn, path)
		e.lockAction(p.alice.signedIn, path, aliceLock, "acquire", acquiring("op-lock-int-00001", "obj-1"))

		failing := failingMemberships{err: errors.New("membership read failed")}
		locks := *e.config.Locks
		locks.Grants = memberGrants{memberships: failing, versions: x.versions}
		e.config.Memberships, e.config.Locks = failing, &locks
		handler, err := httpapi.New(e.config)
		if err != nil {
			t.Fatal(err)
		}
		working := e.handler
		e.handler = handler
		expectError(t, e.lockAction(p.alice.signedIn, path, aliceLock, "heartbeat", nil), http.StatusInternalServerError, "internal")
		expectError(t, e.do(http.MethodPost, path+"/locks/sessions", nil, withSession(p.bob.token), withCSRF(p.bob.csrf)), http.StatusInternalServerError, "internal")
		e.handler = working
		e.expectLocks(p.bob, path, lockList(lockOf("obj-1", "alice@example.com", false)))
	})
}

type failingMemberships struct{ err error }

func (f failingMemberships) MembershipOf(context.Context, access.AccountID, string) (access.Membership, bool, error) {
	return access.Membership{}, false, f.err
}
