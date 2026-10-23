package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/outbox"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpapi"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpx"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/ingress"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/logging"
	argon2 "github.com/enzoruiz/pepicanvas/backend/internal/platform/password"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/pgtest"
	"github.com/enzoruiz/pepicanvas/backend/internal/realtime"
	"github.com/enzoruiz/pepicanvas/backend/internal/subscription"
)

// pgEnv is one server process over real PostgreSQL, wired as cmd/server
// wires it. Opening another pgEnv on the same server is a reconnection.
type pgEnv struct {
	t        *testing.T
	server   *pgtest.Server
	pool     *pgxpool.Pool
	canvases *postgres.CanvasStore
	http     *env
}

// openPGEnv connects to the current server address, migrates, and builds the
// handler. Options configure the canvas store.
func openPGEnv(t *testing.T, server *pgtest.Server, options ...postgres.CanvasStoreOption) *pgEnv {
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
	p := &pgEnv{t: t, server: server, pool: pool}
	p.serve(postgres.NewCanvasStore(pool, options...))
	return p
}

// serve rebuilds the handler around canvases, as cmd/server does.
func (p *pgEnv) serve(canvases *postgres.CanvasStore) {
	p.t.Helper()
	store := postgres.NewAccountStore(p.pool)
	// Cheap argon2id parameters keep the tests fast; verification reads them
	// from each hash.
	accounts, err := access.NewService(store, store, argon2.Argon2id{Memory: 1024, Iterations: 1})
	if err != nil {
		p.t.Fatal(err)
	}
	logs := &syncBuffer{}
	logger := slog.New(logging.NewHandler(slog.NewJSONHandler(logs, nil)))
	in, err := ingress.New(ingress.Config{AllowedOrigins: []string{origin}, Limits: ingress.DefaultLimits, Logger: logger})
	if err != nil {
		p.t.Fatal(err)
	}
	subscriptions := postgres.NewSubscriptionStore(p.pool)
	handler, err := httpapi.New(httpapi.Config{
		Accounts:      accounts,
		Memberships:   store,
		Canvases:      canvases,
		Subscriptions: subscription.NewService(subscriptions),
		Tenants:       subscriptions,
		Ingress:       in,
		Logger:        logger,
		Limits:        httpapi.DefaultLimits,
	})
	if err != nil {
		p.t.Fatal(err)
	}
	p.canvases = canvases
	p.http = &env{t: p.t, ingress: in, handler: handler, logs: logs}
}

func (p *pgEnv) register(op, email string) access.AccountID {
	p.t.Helper()
	res := p.http.do(http.MethodPost, "/api/accounts", registration(op, email, password))
	if res.StatusCode != http.StatusCreated {
		p.t.Fatalf("register %s: status %d", email, res.StatusCode)
	}
	return access.AccountID(decode(p.t, res)["account_id"].(string))
}

// operator registers an account, grants it the operator role as the
// grant-operator command does, and signs it in.
func (p *pgEnv) operator(email string) signedIn {
	p.t.Helper()
	id := p.register("op-pg-dim-reg-operator", email)
	granted, err := postgres.NewSubscriptionStore(p.pool).GrantOperator(context.Background(), email)
	if err != nil || granted != id {
		p.t.Fatalf("GrantOperator(%s) = %q, %v; want %q", email, granted, err, id)
	}
	return p.http.signIn(email)
}

// streamer registers, activates over HTTP, and signs in the account of email.
func (p *pgEnv) streamer(op signedIn, name, email string) streamer {
	p.t.Helper()
	p.register("op-pg-dim-reg-"+name, email)
	a := p.http.activate(op, "op-pg-dim-act-"+name, email)
	tenantID, canvasID := a["tenant_id"].(string), a["canvas_id"].(string)
	return streamer{
		signedIn: p.http.signIn(email),
		tenantID: tenantID,
		canvasID: canvasID,
		path:     "/api/tenants/" + tenantID + "/canvases/" + canvasID,
	}
}

// moderator registers the account of email and makes it a moderator of the
// owner's tenant.
func (p *pgEnv) moderator(owner streamer, email string) signedIn {
	p.t.Helper()
	id := p.register("op-pg-dim-reg-moderator", email)
	if _, err := p.pool.Exec(context.Background(),
		"INSERT INTO memberships (tenant_id, account_id, role) VALUES ($1, $2, 'moderator')", owner.tenantID, string(id)); err != nil {
		p.t.Fatalf("insert moderator membership: %v", err)
	}
	return p.http.signIn(email)
}

func (p *pgEnv) resize(s signedIn, path string, body any, extra ...option) *http.Response {
	p.t.Helper()
	return p.http.resize(s, path, body, extra...)
}

// lastBody is the raw body of the latest response.
func (p *pgEnv) lastBody() string {
	return p.http.bodies[len(p.http.bodies)-1]
}

func (p *pgEnv) load(s streamer) canvas.Canvas {
	p.t.Helper()
	loaded, err := p.canvases.Load(context.Background(), s.key())
	if err != nil {
		p.t.Fatalf("Load(%v): %v", s.key(), err)
	}
	return loaded
}

// expectStored checks the committed canvas row and its retained operation
// and outbox rows.
func (p *pgEnv) expectStored(s streamer, width, height int, revision int64, operations, records int) {
	p.t.Helper()
	loaded := p.load(s)
	if loaded.Dimensions != (canvas.Dimensions{Width: width, Height: height}) || loaded.Revision != revision {
		p.t.Fatalf("stored canvas = %v revision %d, want %dx%d revision %d", loaded.Dimensions, loaded.Revision, width, height, revision)
	}
	if got := p.count("SELECT count(*) FROM canvas_operations WHERE tenant_id = $1 AND canvas_id = $2", s.tenantID, s.canvasID); got != operations {
		p.t.Fatalf("canvas_operations rows = %d, want %d", got, operations)
	}
	if got := p.count("SELECT count(*) FROM canvas_outbox WHERE tenant_id = $1 AND canvas_id = $2", s.tenantID, s.canvasID); got != records {
		p.t.Fatalf("canvas_outbox rows = %d, want %d", got, records)
	}
}

func (p *pgEnv) count(query string, args ...any) int {
	p.t.Helper()
	var n int
	if err := p.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		p.t.Fatalf("%s: %v", query, err)
	}
	return n
}

func (s streamer) key() canvas.Key { return canvas.Key{TenantID: s.tenantID, CanvasID: s.canvasID} }

// sent is one response read by send.
type sent struct {
	status int
	header http.Header
	body   string
}

// send serves one request without touching test state, so goroutines can
// share it.
func send(handler http.Handler, method, path string, body any, options ...option) sent {
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(method, path, bytes.NewReader(raw))
	r.RemoteAddr = clientIP + ":40000"
	r.Header.Set("Origin", origin)
	r.Header.Set("Content-Type", "application/json")
	for _, o := range options {
		o(r)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	res := w.Result()
	read, _ := io.ReadAll(res.Body)
	return sent{status: res.StatusCode, header: res.Header, body: string(read)}
}

// pgClock is a settable time source for operation retention.
type pgClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *pgClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *pgClock) Set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

func pgEventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func newPGHub(t *testing.T) *realtime.Hub {
	t.Helper()
	hub, err := realtime.NewHub(realtime.Config{Authorize: func(context.Context, realtime.Command) error { return nil }})
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	t.Cleanup(func() { _ = hub.Close(context.Background()) })
	return hub
}

func pgHubEvents(t *testing.T, hub *realtime.Hub, key canvas.Key) []realtime.Event {
	t.Helper()
	events, err := hub.Events(context.Background(), realtime.Key{TenantID: key.TenantID, CanvasID: key.CanvasID}, 0)
	if err != nil {
		t.Fatalf("Events(%v): %v", key, err)
	}
	return events
}

// installCommitFault makes every transaction that writes an outbox record
// fail at COMMIT, after the mutation and the record were written.
func installCommitFault(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		CREATE FUNCTION fail_outbox_commit() RETURNS trigger LANGUAGE plpgsql AS
		$$ BEGIN RAISE EXCEPTION 'injected commit failure'; END $$;
		CREATE CONSTRAINT TRIGGER fail_outbox_commit AFTER INSERT ON canvas_outbox
		DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_outbox_commit();`); err != nil {
		t.Fatalf("install commit fault: %v", err)
	}
}

// S1: the canvas starts at 1920x1080, both inclusive limits commit, and a
// fresh process reads them back.
func TestPostgresResizeWithinInclusiveBounds(t *testing.T) {
	server := pgtest.Start(t)
	p := openPGEnv(t, server)
	op := p.operator("operator@example.com")
	alice := p.streamer(op, "alice1", "alice@example.com")
	p.http.expectCanvas(alice, 1920, 1080, 0)
	p.expectStored(alice, 1920, 1080, 0, 0, 0)

	for i, size := range [][2]int{{320, 180}, {3840, 2160}} {
		revision := i + 1
		expectResized(t, p.resize(alice.signedIn, alice.path, dimensions(fmt.Sprintf("op-pg-dim-bound-%02d", i), size[0], size[1])), alice.canvasID, size[0], size[1], revision)
		fresh := openPGEnv(t, server)
		fresh.http.expectCanvas(alice, size[0], size[1], revision)
		fresh.expectStored(alice, size[0], size[1], int64(revision), revision, revision)
	}
}

// S2: one invalid value of each class leaves the canvas, its retained
// operations, and its outbox unchanged, and retains no operation ID.
func TestPostgresInvalidResizeChangesNothing(t *testing.T) {
	p := openPGEnv(t, pgtest.Start(t))
	op := p.operator("operator@example.com")
	alice := p.streamer(op, "alice1", "alice@example.com")
	expectResized(t, p.resize(alice.signedIn, alice.path, dimensions("op-pg-dim-seed-01", 1280, 720)), alice.canvasID, 1280, 720, 1)

	const reused = `"operation_id":"op-pg-dim-bad-001"`
	for name, raw := range map[string]string{
		"width 319":      `{` + reused + `,"width":319,"height":720}`,
		"height 179":     `{` + reused + `,"width":1280,"height":179}`,
		"width 3841":     `{` + reused + `,"width":3841,"height":720}`,
		"height 2161":    `{` + reused + `,"width":1280,"height":2161}`,
		"fractional":     `{` + reused + `,"width":1280.5,"height":720}`,
		"string":         `{` + reused + `,"width":"1280","height":720}`,
		"missing height": `{` + reused + `,"width":1280}`,
	} {
		t.Run(name, func(t *testing.T) {
			expectError(t, p.resize(alice.signedIn, alice.path, nil, withBody("application/json", raw)), http.StatusBadRequest, "bad_request")
			p.expectStored(alice, 1280, 720, 1, 1, 1)
			p.http.expectCanvas(alice, 1280, 720, 1)
		})
	}
	expectResized(t, p.resize(alice.signedIn, alice.path, dimensions("op-pg-dim-bad-001", 1600, 900)), alice.canvasID, 1600, 900, 2)
}

// objectsOf returns the raw objects list of a canvas read.
func (p *pgEnv) objectsOf(s streamer) string {
	p.t.Helper()
	res := p.http.do(http.MethodGet, s.path, nil, withSession(s.token))
	if res.StatusCode != http.StatusOK {
		p.t.Fatalf("GET %s = %d", s.path, res.StatusCode)
	}
	var body struct {
		Objects json.RawMessage `json:"objects"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		p.t.Fatal(err)
	}
	return string(body.Objects)
}

// S3: resizing keeps every object's absolute position, size, rotation, and
// order, including objects partly and fully outside the new area.
func TestPostgresResizePreservesObjectsAbsolutely(t *testing.T) {
	p := openPGEnv(t, pgtest.Start(t))
	op := p.operator("operator@example.com")
	alice := p.streamer(op, "alice1", "alice@example.com")
	ctx := context.Background()
	for i, object := range []canvas.CreateObject{
		{Kind: canvas.KindText, Content: "inside", Transform: canvas.Transform{X: 10, Y: 20, Width: 100, Height: 50, Rotation: 0}},
		{Kind: canvas.KindText, Content: "partly outside", Transform: canvas.Transform{X: 600, Y: 300, Width: 100, Height: 120, Rotation: 15.5}},
		{Kind: canvas.KindText, Content: "fully outside", Transform: canvas.Transform{X: 1200, Y: 800, Width: 300, Height: 100, Rotation: 329.75}},
	} {
		if _, err := p.canvases.Apply(ctx, alice.key(), canvas.Operation{ID: fmt.Sprintf("op-pg-dim-object-%d", i)}, object); err != nil {
			t.Fatalf("create %s: %v", object.Content, err)
		}
	}
	before := p.load(alice).Objects
	if len(before) != 3 || before[0].Z == before[1].Z || before[1].Z == before[2].Z || before[0].Z == before[2].Z {
		t.Fatalf("seeded objects = %+v, want three with distinct z", before)
	}
	listed := p.objectsOf(alice)
	if !strings.Contains(listed, `"fully outside"`) {
		t.Fatalf("GET objects = %s", listed)
	}

	for i, size := range [][2]int{{640, 360}, {1920, 1080}} {
		expectResized(t, p.resize(alice.signedIn, alice.path, dimensions(fmt.Sprintf("op-pg-dim-preserve-%d", i), size[0], size[1])), alice.canvasID, size[0], size[1], 4+i)
		if after := p.load(alice).Objects; !reflect.DeepEqual(after, before) {
			t.Fatalf("objects after resize to %v = %+v, want %+v", size, after, before)
		}
		if got := p.objectsOf(alice); got != listed {
			t.Fatalf("GET objects after resize to %v = %s, want %s", size, got, listed)
		}
	}
}

// S4: a moderator, another streamer, and an operator are refused, and
// nothing is written.
func TestPostgresOnlyTheOwnerResizes(t *testing.T) {
	p := openPGEnv(t, pgtest.Start(t))
	op := p.operator("operator@example.com")
	alice := p.streamer(op, "alice1", "alice@example.com")
	bob := p.streamer(op, "bob001", "bob@example.com")
	moderator := p.moderator(alice, "moderator@example.com")
	if res := p.http.do(http.MethodGet, alice.path, nil, withSession(moderator.token)); res.StatusCode != http.StatusOK {
		t.Fatalf("moderator GET = %d, want 200", res.StatusCode)
	}

	for name, caller := range map[string]signedIn{
		"moderator":               moderator,
		"another active streamer": bob.signedIn,
		"operator":                op,
	} {
		t.Run(name, func(t *testing.T) {
			expectError(t, p.resize(caller, alice.path, dimensions("op-pg-dim-deny-01", 1280, 720)), http.StatusForbidden, "forbidden")
			p.expectStored(alice, 1920, 1080, 0, 0, 0)
		})
	}
	p.http.expectCanvas(alice, 1920, 1080, 0)
	p.expectStored(bob, 1920, 1080, 0, 0, 0)
}

// S11, CTRL-WEB-01: over real PostgreSQL, a missing or invalid CSRF token and
// an absent or hostile Origin are refused by the ingress with its exact
// forbidden answer, leaving the canvas row, its revision, its retained
// operations, and its outbox unchanged; the valid request then commits once.
func TestPostgresIngressRefusesResizeBeforeTheDomain(t *testing.T) {
	p := openPGEnv(t, pgtest.Start(t))
	op := p.operator("operator@example.com")
	alice := p.streamer(op, "alice1", "alice@example.com")
	other := p.http.signIn("alice@example.com")
	const id = "op-pg-dim-web-001"

	for _, c := range []struct {
		name   string
		opts   []option
		reason ingress.Reason
	}{
		{"missing CSRF", []option{func(r *http.Request) { r.Header.Del(httpapi.CSRFHeader) }}, ingress.ReasonCSRF},
		{"invalid CSRF", []option{withCSRF("not-the-token")}, ingress.ReasonCSRF},
		{"other session CSRF", []option{withCSRF(other.csrf)}, ingress.ReasonCSRF},
		{"absent Origin", []option{withoutOrigin()}, ingress.ReasonOrigin},
		{"hostile Origin", []option{withOrigin("https://evil.test")}, ingress.ReasonOrigin},
	} {
		count := reasonCount(p.http.ingress, c.reason)
		expectError(t, p.resize(alice.signedIn, alice.path, dimensions(id, 1280, 720), c.opts...), http.StatusForbidden, "forbidden")
		if got := reasonCount(p.http.ingress, c.reason); got != count+1 {
			t.Fatalf("%s: %s rejections = %d, want %d", c.name, c.reason, got, count+1)
		}
		p.expectStored(alice, 1920, 1080, 0, 0, 0)
	}
	p.http.expectCanvas(alice, 1920, 1080, 0)

	expectResized(t, p.resize(alice.signedIn, alice.path, dimensions(id, 1280, 720)), alice.canvasID, 1280, 720, 1)
	p.expectStored(alice, 1280, 720, 1, 1, 1)
}

// S5: concurrent resizes commit one at a time; the last confirmed revision
// defines the dimensions.
func TestPostgresConcurrentResizesLastConfirmedWins(t *testing.T) {
	p := openPGEnv(t, pgtest.Start(t))
	op := p.operator("operator@example.com")
	alice := p.streamer(op, "alice1", "alice@example.com")

	const n = 12
	results := make([]sent, n)
	begin := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			<-begin
			results[i] = send(p.http.handler, http.MethodPut, alice.path+"/dimensions",
				dimensions(fmt.Sprintf("op-pg-dim-race-%02d", i), 1280+i, 720+i),
				withSession(alice.token), withCSRF(alice.csrf))
		})
	}
	close(begin)
	wg.Wait()

	widthAt := map[int64]int{}
	for i, r := range results {
		if r.status != http.StatusOK {
			t.Fatalf("request %d = %d %s", i, r.status, r.body)
		}
		var body struct {
			Width, Height int
			Revision      int64
		}
		if err := json.Unmarshal([]byte(r.body), &body); err != nil {
			t.Fatal(err)
		}
		if body.Width != 1280+i || body.Height != 720+i {
			t.Fatalf("request %d answered %s", i, r.body)
		}
		if _, dup := widthAt[body.Revision]; dup {
			t.Fatalf("revision %d confirmed twice", body.Revision)
		}
		widthAt[body.Revision] = body.Width
	}
	for revision := int64(1); revision <= n; revision++ {
		if _, ok := widthAt[revision]; !ok {
			t.Fatalf("revisions = %v, want 1..%d", widthAt, n)
		}
	}
	last := widthAt[n]
	p.http.expectCanvas(alice, last, last-1280+720, n)
	p.expectStored(alice, last, last-1280+720, n, n, n)

	// The outbox holds every resize in its confirmed order.
	rows, err := p.pool.Query(context.Background(),
		"SELECT revision, (mutation->>'width')::int FROM canvas_outbox WHERE tenant_id = $1 AND canvas_id = $2 ORDER BY id", alice.tenantID, alice.canvasID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var want int64 = 1
	for rows.Next() {
		var revision int64
		var width int
		if err := rows.Scan(&revision, &width); err != nil {
			t.Fatal(err)
		}
		if revision != want || width != widthAt[revision] {
			t.Fatalf("outbox record %d = revision %d width %d, want width %d", want, revision, width, widthAt[want])
		}
		want++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// S6, S10: after a lost answer and a database restart, a reconnecting client
// reads the confirmed dimensions, a retry gets the same answer with one
// effect, and a different payload under the same operation ID is refused.
func TestPostgresResizeReplaySurvivesRestart(t *testing.T) {
	server := pgtest.Start(t)
	p := openPGEnv(t, server)
	op := p.operator("operator@example.com")
	alice := p.streamer(op, "alice1", "alice@example.com")
	expectResized(t, p.resize(alice.signedIn, alice.path, dimensions("op-pg-dim-idem-01", 1280, 720)), alice.canvasID, 1280, 720, 1)
	first := p.lastBody()
	p.pool.Close()

	server.Restart(t)
	p = openPGEnv(t, server)
	p.http.expectCanvas(alice, 1280, 720, 1)
	again := p.http.signIn("alice@example.com")
	for _, s := range []signedIn{alice.signedIn, again} {
		p.resize(s, alice.path, dimensions("op-pg-dim-idem-01", 1280, 720))
		if got := p.lastBody(); got != first {
			t.Fatalf("replay after restart = %s, want %s", got, first)
		}
	}
	p.expectStored(alice, 1280, 720, 1, 1, 1)

	expectError(t, p.resize(alice.signedIn, alice.path, dimensions("op-pg-dim-idem-01", 1600, 900)), http.StatusConflict, "conflict")
	p.expectStored(alice, 1280, 720, 1, 1, 1)
	p.http.expectCanvas(alice, 1280, 720, 1)
}

// S10: an operation ID replays until its retention ends; at exactly the
// retention it starts a new mutation.
func TestPostgresResizeRetentionBoundary(t *testing.T) {
	server := pgtest.Start(t)
	t0 := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	clock := &pgClock{now: t0}
	const retention = postgres.DefaultOperationRetention
	p := openPGEnv(t, server, postgres.WithClock(clock.Now), postgres.WithRetention(retention))
	op := p.operator("operator@example.com")
	alice := p.streamer(op, "alice1", "alice@example.com")
	expectResized(t, p.resize(alice.signedIn, alice.path, dimensions("op-pg-dim-ttl-01", 1280, 720)), alice.canvasID, 1280, 720, 1)
	first := p.lastBody()

	clock.Set(t0.Add(retention - time.Nanosecond))
	p.resize(alice.signedIn, alice.path, dimensions("op-pg-dim-ttl-01", 1280, 720))
	if got := p.lastBody(); got != first {
		t.Fatalf("replay at retention-1ns = %s, want %s", got, first)
	}
	expectError(t, p.resize(alice.signedIn, alice.path, dimensions("op-pg-dim-ttl-01", 1600, 900)), http.StatusConflict, "conflict")
	p.expectStored(alice, 1280, 720, 1, 1, 1)

	clock.Set(t0.Add(retention))
	expectResized(t, p.resize(alice.signedIn, alice.path, dimensions("op-pg-dim-ttl-01", 1280, 720)), alice.canvasID, 1280, 720, 2)
	p.expectStored(alice, 1280, 720, 2, 1, 2)
}

// S7: resizing one canvas never changes another, and a refusal on another
// tenant's canvas reveals nothing about it.
func TestPostgresResizeIsolatesCanvases(t *testing.T) {
	p := openPGEnv(t, pgtest.Start(t))
	op := p.operator("operator@example.com")
	alice := p.streamer(op, "alice1", "alice@example.com")
	bob := p.streamer(op, "bob001", "bob@example.com")

	expectResized(t, p.resize(alice.signedIn, alice.path, dimensions("op-pg-dim-iso-01", 1280, 720)), alice.canvasID, 1280, 720, 1)
	p.http.expectCanvas(bob, 1920, 1080, 0)
	p.expectStored(bob, 1920, 1080, 0, 0, 0)
	for _, body := range p.http.bodies[len(p.http.bodies)-2:] {
		if strings.Contains(body, alice.tenantID) || strings.Contains(body, alice.canvasID) {
			t.Fatalf("bob's canvas reveals alice's: %s", body)
		}
	}

	missing := "/api/tenants/" + alice.tenantID + "/canvases/missing"
	for _, path := range []string{alice.path, missing} {
		res := p.resize(bob.signedIn, path, dimensions("op-pg-dim-iso-02", 3840, 2160))
		body := p.lastBody()
		expectError(t, res, http.StatusForbidden, "forbidden")
		if strings.Contains(body, alice.tenantID) || strings.Contains(body, alice.canvasID) || strings.Contains(body, "1280") {
			t.Fatalf("refusal reveals alice's canvas: %s", body)
		}
	}
	p.expectStored(alice, 1280, 720, 1, 1, 1)
	p.expectStored(bob, 1920, 1080, 0, 0, 0)
	p.http.expectCanvas(alice, 1280, 720, 1)
}

// S9 (CTRL-CONS-01), S15: the dimensions, the retained operation, and the
// outbox record commit in one transaction; a failure at COMMIT leaves none.
func TestPostgresResizeCommitFailureLeavesNothing(t *testing.T) {
	p := openPGEnv(t, pgtest.Start(t))
	op := p.operator("operator@example.com")
	alice := p.streamer(op, "alice1", "alice@example.com")

	installCommitFault(t, p.pool)
	expectError(t, p.resize(alice.signedIn, alice.path, dimensions("op-pg-dim-cons-01", 1280, 720)), http.StatusInternalServerError, "internal")
	p.expectStored(alice, 1920, 1080, 0, 0, 0)
	p.http.expectCanvas(alice, 1920, 1080, 0)

	if _, err := p.pool.Exec(context.Background(), "DROP TRIGGER fail_outbox_commit ON canvas_outbox"); err != nil {
		t.Fatalf("remove commit fault: %v", err)
	}
	expectResized(t, p.resize(alice.signedIn, alice.path, dimensions("op-pg-dim-cons-01", 1280, 720)), alice.canvasID, 1280, 720, 1)
	p.expectStored(alice, 1280, 720, 1, 1, 1)
}

// S9 (CTRL-CONS-01), S12: a process that confirms resizes and crashes before
// publishing confirms nothing in realtime; after a restart the publisher
// delivers every resize in order, with the request correlation.
func TestPostgresResizeIsDeliveredAfterACrashBeforePublish(t *testing.T) {
	server := pgtest.Start(t)
	p := openPGEnv(t, server)
	op := p.operator("operator@example.com")
	alice := p.streamer(op, "alice1", "alice@example.com")
	crashed := newPGHub(t)

	sizes := [][2]int{{1280, 720}, {1600, 900}}
	correlations := make([]string, len(sizes))
	for i, size := range sizes {
		operationID := fmt.Sprintf("op-pg-dim-crash-%d", i)
		res := p.resize(alice.signedIn, alice.path, dimensions(operationID, size[0], size[1]))
		correlations[i] = res.Header.Get(httpx.CorrelationHeader)
		expectResized(t, res, alice.canvasID, size[0], size[1], i+1)
		for _, table := range []string{"canvas_operations", "canvas_outbox"} {
			query := "SELECT count(*) FROM " + table + " WHERE tenant_id = $1 AND canvas_id = $2 AND operation_id = $3 AND correlation_id = $4"
			if n := p.count(query, alice.tenantID, alice.canvasID, operationID, correlations[i]); n != 1 {
				t.Fatalf("%s has no row for %s with correlation %s", table, operationID, correlations[i])
			}
		}
	}
	events := postgres.NewOutboxStore(p.pool)
	if stats, err := events.Stats(context.Background(), alice.tenantID); err != nil || stats.Pending != 2 {
		t.Fatalf("Stats before publication = %+v, %v; want 2 pending", stats, err)
	}
	if got := pgHubEvents(t, crashed, alice.key()); len(got) != 0 {
		t.Fatalf("hub confirmed %+v before publication", got)
	}
	p.pool.Close()

	server.Restart(t)
	p = openPGEnv(t, server)
	events = postgres.NewOutboxStore(p.pool)
	hub := newPGHub(t)
	publisher, err := outbox.NewPublisher(outbox.Config{Store: events, Sink: outbox.NewHubSink(hub), PollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	if err := publisher.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = publisher.Close(context.Background()) })
	pgEventually(t, "every resize to be delivered", func() bool {
		stats, err := events.Stats(context.Background(), alice.tenantID)
		return err == nil && stats.Pending == 0
	})

	delivered := pgHubEvents(t, hub, alice.key())
	if len(delivered) != len(sizes) {
		t.Fatalf("hub has %d events, want %d: %+v", len(delivered), len(sizes), delivered)
	}
	for i, event := range delivered {
		var payload struct {
			Kind     string `json:"kind"`
			Revision int64  `json:"revision"`
			Mutation struct {
				Width  int `json:"width"`
				Height int `json:"height"`
			} `json:"mutation"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("event %d payload %s: %v", i, event.Payload, err)
		}
		if event.Kind != realtime.KindSet || event.ObjectID != outbox.CanvasObjectID || payload.Kind != "set_dimensions" ||
			payload.Revision != int64(i+1) || payload.Mutation.Width != sizes[i][0] || payload.Mutation.Height != sizes[i][1] {
			t.Fatalf("event %d = %+v payload %s, want set_dimensions %v revision %d", i, event, event.Payload, sizes[i], i+1)
		}
		if event.CorrelationID != correlations[i] {
			t.Fatalf("event %d correlation = %q, want %q", i, event.CorrelationID, correlations[i])
		}
	}
	if counters := publisher.Counters(); counters.Delivered != 2 || counters.Rejected != 0 {
		t.Fatalf("Counters = %+v, want 2 delivered", counters)
	}
	if got := pgHubEvents(t, crashed, alice.key()); len(got) != 0 {
		t.Fatalf("crashed hub confirmed %+v", got)
	}
	p.http.expectCanvas(alice, 1600, 900, 2)
}

// S14: with PostgreSQL unreachable the resize answers internal, and after
// recovery the dimensions are those confirmed before.
func TestPostgresResizeWithTheDatabaseDownChangesNothing(t *testing.T) {
	server := pgtest.Start(t)
	p := openPGEnv(t, server)
	op := p.operator("operator@example.com")
	alice := p.streamer(op, "alice1", "alice@example.com")
	expectResized(t, p.resize(alice.signedIn, alice.path, dimensions("op-pg-dim-down-01", 1280, 720)), alice.canvasID, 1280, 720, 1)

	// Only the canvas store loses its connections: the session and the owner
	// check pass, and the commit cannot run.
	canvasPool, err := postgres.Open(context.Background(), server.DSN(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	p.serve(postgres.NewCanvasStore(canvasPool))
	canvasPool.Close()
	expectError(t, p.resize(alice.signedIn, alice.path, dimensions("op-pg-dim-down-02", 1600, 900)), http.StatusInternalServerError, "internal")

	// Every connection is gone.
	p.pool.Close()
	expectError(t, p.resize(alice.signedIn, alice.path, dimensions("op-pg-dim-down-03", 1600, 900)), http.StatusInternalServerError, "internal")

	recovered := openPGEnv(t, server)
	recovered.http.expectCanvas(alice, 1280, 720, 1)
	recovered.expectStored(alice, 1280, 720, 1, 1, 1)
	expectResized(t, recovered.resize(alice.signedIn, alice.path, dimensions("op-pg-dim-down-02", 1600, 900)), alice.canvasID, 1600, 900, 2)
}
