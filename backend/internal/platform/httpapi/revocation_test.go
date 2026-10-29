package httpapi_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/media"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpapi"
)

// trace records, in order, the membership reads, canvas loads, and media
// usage reads of the routes under test.
type trace struct {
	mu     sync.Mutex
	events []string
}

func (tr *trace) add(event string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.events = append(tr.events, event)
}

func (tr *trace) take() []string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	events := tr.events
	tr.events = nil
	return events
}

// revokingMemberships answers account's membership for its first keep reads
// and then as removed, as a revocation confirmed in between does. A negative
// keep never revokes.
type revokingMemberships struct {
	access.MembershipStore
	trace   *trace
	account access.AccountID
	keep    int
	mu      sync.Mutex
	reads   int
}

func (m *revokingMemberships) MembershipOf(ctx context.Context, account access.AccountID, tenantID string) (access.Membership, bool, error) {
	m.trace.add("membership")
	membership, found, err := m.MembershipStore.MembershipOf(ctx, account, tenantID)
	if account != m.account || m.keep < 0 {
		return membership, found, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reads++
	if m.reads > m.keep {
		return access.Membership{}, false, nil
	}
	return membership, found, err
}

type tracedCanvases struct {
	httpapi.CanvasStore
	trace *trace
}

func (c tracedCanvases) Load(ctx context.Context, key canvas.Key) (canvas.Canvas, error) {
	c.trace.add("load")
	return c.CanvasStore.Load(ctx, key)
}

// actorMedia is a media store that records the actor of every command and
// rejects the commands of revoked, as the PostgreSQL store does when the
// actor's membership is gone at the commit.
type actorMedia struct {
	media.Store
	trace   *trace
	revoked access.AccountID
	mu      sync.Mutex
	actors  []string
}

func (s *actorMedia) Usage(_ context.Context, key media.CanvasKey) (media.Usage, error) {
	s.trace.add("usage")
	return media.Usage{QuotaBytes: media.QuotaBytes, UsedBytes: 4000}, nil
}

func (s *actorMedia) Abandoned(context.Context, media.CanvasKey, string) (media.Abandon, error) {
	return media.Abandon{}, media.ErrNotFound
}

func (s *actorMedia) Apply(_ context.Context, key media.CanvasKey, operation media.Operation, command media.Command) (media.Result, error) {
	s.mu.Lock()
	s.actors = append(s.actors, fmt.Sprintf("%T:%s", command, operation.AccountID))
	s.mu.Unlock()
	if operation.AccountID == string(s.revoked) {
		return media.Result{}, fmt.Errorf("postgres: apply media command: %w", access.ErrAccessDenied)
	}
	return media.Result{AssetID: "asset-1", Usage: media.Usage{QuotaBytes: media.QuotaBytes, UsedBytes: 4000}}, nil
}

func (s *actorMedia) takeActors() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	actors := s.actors
	s.actors = nil
	return actors
}

// unusedStorage fails the test if an upload reaches the storage or the
// inspector.
type unusedStorage struct{ t *testing.T }

func (u unusedStorage) Put(context.Context, string, media.Owner, io.Reader, int64) error {
	u.t.Error("storage Put called")
	return errors.New("unused")
}

func (u unusedStorage) Open(context.Context, string) (io.ReadCloser, error) {
	u.t.Error("storage Open called")
	return nil, errors.New("unused")
}

func (u unusedStorage) Delete(context.Context, string) error { return nil }

func (u unusedStorage) Inspect(context.Context, io.Reader) (media.Inspection, error) {
	u.t.Error("inspector called")
	return media.Inspection{}, errors.New("unused")
}

// revocationEnv is tenant-a with an owner, a moderator, and a canvas with one
// object, served over the actor-recording media store.
type revocationEnv struct {
	*env
	trace                *trace
	media                *actorMedia
	owner, moderator     streamer
	ownerID, moderatorID access.AccountID
}

const revocationPath = "/api/tenants/tenant-a/canvases/main"

func newRevocationEnv(t *testing.T) *revocationEnv {
	t.Helper()
	e := newEnv(t, httpapi.DefaultLimits)
	owner := e.register("op-http-revoke-001", "owner@example.com")
	moderator := e.register("op-http-revoke-002", "moderator@example.com")
	active := access.SubscriptionActive
	e.store.SetMembership(owner, access.Membership{TenantID: "tenant-a", Role: access.RoleOwner, Subscription: active})
	e.store.SetMembership(moderator, access.Membership{TenantID: "tenant-a", Role: access.RoleModerator, Subscription: active})
	key := canvas.Key{TenantID: "tenant-a", CanvasID: "main"}
	e.canvases.byID[key] = canvas.Canvas{Key: key, Dimensions: canvas.DefaultDimensions, Revision: 3, Objects: []canvas.Object{
		{ID: "obj-a", Kind: canvas.KindText, Content: "content of tenant-a", Transform: canvas.Transform{Width: 10, Height: 10}, Z: 1},
	}}
	r := &revocationEnv{env: e, trace: &trace{}, ownerID: owner, moderatorID: moderator}
	r.media = &actorMedia{trace: r.trace}
	o, m := e.signIn("owner@example.com"), e.signIn("moderator@example.com")
	r.owner = streamer{signedIn: o, path: revocationPath}
	r.moderator = streamer{signedIn: m, path: revocationPath}
	return r
}

// serve rebuilds the handler so the moderator's membership is answered for
// its first keep reads and then as removed, and media commands of revoked
// are rejected by the store.
func (r *revocationEnv) serve(keep int, revoked access.AccountID) {
	r.t.Helper()
	config := r.config
	config.Memberships = &revokingMemberships{MembershipStore: r.config.Memberships, trace: r.trace, account: r.moderatorID, keep: keep}
	config.Canvases = tracedCanvases{CanvasStore: r.config.Canvases, trace: r.trace}
	r.media.revoked = revoked
	config.Media = r.media
	config.Storage = unusedStorage{r.t}
	config.Inspector = unusedStorage{r.t}
	handler, err := httpapi.New(config)
	if err != nil {
		r.t.Fatal(err)
	}
	r.handler = handler
	r.trace.take()
	r.media.takeActors()
}

func (r *revocationEnv) mediaCommands(s streamer) map[string]func() *http.Response {
	auth := []option{withSession(s.token), withCSRF(s.csrf)}
	return map[string]func() *http.Response{
		"upload": func() *http.Response {
			return r.do(http.MethodPost, s.path+"/media/uploads?operation_id=op-up&reference_id=ref-up", nil,
				append(auth, withBody("application/octet-stream", "data"))...)
		},
		"attach": func() *http.Response {
			return r.do(http.MethodPost, s.path+"/media/references", map[string]string{"operation_id": "op-at", "asset_id": "asset-1", "reference_id": "ref-at"}, auth...)
		},
		"detach": func() *http.Response {
			return r.do(http.MethodPost, s.path+"/media/references/ref-1/detach", map[string]string{"operation_id": "op-de"}, auth...)
		},
	}
}

// S2, S9: every media command carries the session's account to the store,
// and a moderator whose membership the store finds gone at the commit gets
// the route's forbidden, with no internal detail; the owner is unaffected.
func TestMediaCommandsCarryTheActorAndARevokedOneIsForbidden(t *testing.T) {
	r := newRevocationEnv(t)
	r.serve(-1, r.moderatorID)
	for name, command := range r.mediaCommands(r.moderator) {
		t.Run("moderator "+name, func(t *testing.T) {
			res := command()
			body := r.bodies[len(r.bodies)-1]
			expectError(t, res, http.StatusForbidden, "forbidden")
			if strings.Contains(body, "postgres") || strings.Contains(body, "asset-1") || strings.Contains(body, "denied") {
				t.Fatalf("forbidden body leaks detail: %s", body)
			}
			want := map[string]string{"upload": "media.Reserve", "attach": "media.Attach", "detach": "media.Detach"}[name]
			if got := r.media.takeActors(); !slices.Equal(got, []string{want + ":" + string(r.moderatorID)}) {
				t.Fatalf("store commands = %v, want one %s by the moderator", got, want)
			}
		})
	}
	if strings.Contains(r.logs.String(), "media reference") {
		t.Fatalf("a rejected command was logged as confirmed: %s", r.logs.String())
	}

	ownerID := string(r.ownerID)
	for name, kind := range map[string]string{"attach": "media.Attach", "detach": "media.Detach"} {
		t.Run("owner "+name, func(t *testing.T) {
			if res := r.mediaCommands(r.owner)[name](); res.StatusCode != http.StatusOK {
				t.Fatalf("owner %s: status %d", name, res.StatusCode)
			}
			if got := r.media.takeActors(); !slices.Equal(got, []string{kind + ":" + ownerID}) {
				t.Fatalf("store commands = %v, want one %s by the owner", got, kind)
			}
		})
	}
}

// S8, S9: the canvas and media usage reads reauthorize the membership after
// loading and before the body is written. A membership removed in between
// gets the route's forbidden and no canvas data; the owner still reads.
func TestCanvasReadsReauthorizeBeforeDelivery(t *testing.T) {
	r := newRevocationEnv(t)
	for name, c := range map[string]struct {
		path   string
		events []string
	}{
		"canvas": {revocationPath, []string{"membership", "load", "membership"}},
		"usage":  {revocationPath + "/media/usage", []string{"membership", "usage", "membership"}},
	} {
		t.Run(name, func(t *testing.T) {
			r.serve(-1, "")
			ownerRes := r.do(http.MethodGet, c.path, nil, withSession(r.owner.token))
			if ownerRes.StatusCode != http.StatusOK {
				t.Fatalf("owner: status %d", ownerRes.StatusCode)
			}
			ownerBody := r.bodies[len(r.bodies)-1]
			if got := r.trace.take(); !slices.Equal(got, c.events) {
				t.Fatalf("owner read = %v, want %v", got, c.events)
			}
			if res := r.do(http.MethodGet, c.path, nil, withSession(r.moderator.token)); res.StatusCode != http.StatusOK || r.bodies[len(r.bodies)-1] != ownerBody {
				t.Fatalf("moderator before the revocation: status %d body %s", res.StatusCode, r.bodies[len(r.bodies)-1])
			}

			// The route check passes; the membership is gone at delivery.
			r.serve(1, "")
			res := r.do(http.MethodGet, c.path, nil, withSession(r.moderator.token))
			body := r.bodies[len(r.bodies)-1]
			expectError(t, res, http.StatusForbidden, "forbidden")
			for _, field := range []string{"content of tenant-a", "canvas_id", "width", "revision", "objects", "quota_bytes", "used_bytes"} {
				if strings.Contains(body, field) {
					t.Fatalf("forbidden body carries %q: %s", field, body)
				}
			}
			if got := r.trace.take(); !slices.Equal(got, c.events) {
				t.Fatalf("revoked read = %v, want %v", got, c.events)
			}
			// S5: the owner reads the same content after the revocation.
			if res := r.do(http.MethodGet, c.path, nil, withSession(r.owner.token)); res.StatusCode != http.StatusOK || r.bodies[len(r.bodies)-1] != ownerBody {
				t.Fatalf("owner after the revocation: status %d body %s", res.StatusCode, r.bodies[len(r.bodies)-1])
			}
		})
	}
}
