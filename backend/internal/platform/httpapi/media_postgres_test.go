package httpapi_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/media"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpapi"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpx"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/ingress"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/pgtest"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/s3"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/s3/s3test"
	"github.com/enzoruiz/pepicanvas/backend/internal/subscription"
)

// crashed is the panic value of a simulated process crash between two
// stages; httpx.Recover answers it as an internal error, and nothing of the
// interrupted upload runs after it.
type crashed string

// mediaFaults are the faults and hooks a test arms around the media store,
// the storage, and the inspector. Every hook may panic with crashed.
type mediaFaults struct {
	mu sync.Mutex
	// inspect answers the inspection of n stored bytes; nil accepts them as
	// an image.
	inspect func(ctx context.Context, n int64) (media.Inspection, error)
	// beforePut and afterPut run around a successful Put; failPut fails it.
	beforePut, afterPut func()
	failPut             error
	// committed runs after a media command committed, before its caller
	// sees the result.
	committed func(ctx context.Context, command media.Command)
}

func (f *mediaFaults) set(arm func(*mediaFaults)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	arm(f)
}

// reset disarms every fault.
func (f *mediaFaults) reset() {
	f.set(func(f *mediaFaults) {
		f.inspect, f.beforePut, f.afterPut, f.failPut, f.committed = nil, nil, nil, nil, nil
	})
}

func (f *mediaFaults) snapshot() mediaFaults {
	f.mu.Lock()
	defer f.mu.Unlock()
	return mediaFaults{inspect: f.inspect, beforePut: f.beforePut, afterPut: f.afterPut, failPut: f.failPut, committed: f.committed}
}

type faultStore struct {
	media.Store
	faults *mediaFaults
}

func (s faultStore) Apply(ctx context.Context, key media.CanvasKey, operation media.Operation, command media.Command) (media.Result, error) {
	result, err := s.Store.Apply(ctx, key, operation, command)
	if hook := s.faults.snapshot().committed; err == nil && hook != nil {
		hook(ctx, command)
	}
	return result, err
}

type faultStorage struct {
	*s3.Storage
	faults *mediaFaults
}

func (s faultStorage) Put(ctx context.Context, key string, owner media.Owner, body io.Reader, size int64) error {
	f := s.faults.snapshot()
	if f.beforePut != nil {
		f.beforePut()
	}
	if f.failPut != nil {
		return f.failPut
	}
	if err := s.Storage.Put(ctx, key, owner, body, size); err != nil {
		return err
	}
	if f.afterPut != nil {
		f.afterPut()
	}
	return nil
}

// fakeInspector reads the whole stored input and, unless a fault answers,
// accepts it as an image of exactly the bytes read.
type fakeInspector struct{ faults *mediaFaults }

func (i fakeInspector) Inspect(ctx context.Context, input io.Reader) (media.Inspection, error) {
	n, err := io.Copy(io.Discard, input)
	if err != nil {
		return media.Inspection{}, err
	}
	if inspect := i.faults.snapshot().inspect; inspect != nil {
		return inspect(ctx, n)
	}
	return media.Inspection{Kind: media.KindImage, InputBytes: n, StreamCount: 1, Dimensions: media.Dimensions{Width: 16, Height: 16}}, nil
}

// mediaEnv is the API over PostgreSQL and MinIO with the media routes wired:
// the PostgreSQL media store on a settable clock, the S3 storage, and the
// fake inspector, each behind the faults.
type mediaEnv struct {
	*pgEnv
	minio   *s3test.Server
	storage *s3.Storage
	store   *postgres.MediaStore
	// clock is the media store clock; limits is the API limiter clock.
	clock, limits *pgClock
	faults        *mediaFaults
	op            signedIn
	// seeded are the ready assets written straight to the store, which have
	// no stored object.
	seeded map[string]bool
	seq    int
}

func newMediaEnv(t *testing.T, limits httpapi.Limits) *mediaEnv {
	t.Helper()
	minio := s3test.Start(t)
	p := openPGEnv(t, pgtest.Start(t))
	start := time.Now().UTC().Truncate(time.Microsecond)
	m := &mediaEnv{
		pgEnv: p, minio: minio, storage: minio.Storage(t),
		clock: &pgClock{now: start}, limits: &pgClock{now: start},
		faults: &mediaFaults{}, seeded: map[string]bool{},
	}
	m.store = postgres.NewMediaStore(p.pool, postgres.WithMediaClock(m.clock.Now))
	p.configure = func(c *httpapi.Config) {
		c.Media = faultStore{Store: m.store, faults: m.faults}
		c.Storage = faultStorage{Storage: m.storage, faults: m.faults}
		c.Inspector = fakeInspector{faults: m.faults}
		c.Limits = limits
		c.Now = m.limits.Now
	}
	p.serve(p.canvases)
	m.op = p.operator("operator@example.com")
	return m
}

func (s streamer) media() media.CanvasKey {
	return media.CanvasKey{TenantID: s.tenantID, CanvasID: s.canvasID}
}

// moderatorOf registers the account of email and makes it a moderator of
// the owner's tenant.
func (m *mediaEnv) moderatorOf(owner streamer, name, email string) (access.AccountID, signedIn) {
	m.t.Helper()
	id := m.register("op-pg-media-reg-"+name, email)
	if _, err := m.pool.Exec(context.Background(),
		"INSERT INTO memberships (tenant_id, account_id, role) VALUES ($1, $2, 'moderator')", owner.tenantID, string(id)); err != nil {
		m.t.Fatalf("insert moderator membership: %v", err)
	}
	return id, m.http.signIn(email)
}

func (m *mediaEnv) next(prefix string) string {
	m.seq++
	return fmt.Sprintf("%s-%03d", prefix, m.seq)
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	data := make([]byte, n)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	return data
}

func uploadQuery(op, ref string, replace bool) string {
	query := "operation_id=" + op + "&reference_id=" + ref
	if replace {
		query += "&replace=true"
	}
	return query
}

func authOf(s signedIn) []option { return []option{withSession(s.token), withCSRF(s.csrf)} }

// serveRaw serves one media request on ctx without touching test state, so
// goroutines can share it.
func (m *mediaEnv) serveRaw(ctx context.Context, method, target, contentType string, body io.Reader, length int64, options ...option) sent {
	r := httptest.NewRequestWithContext(ctx, method, target, body)
	r.ContentLength = length
	r.RemoteAddr = clientIP + ":40000"
	r.Header.Set("Origin", origin)
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	for _, o := range options {
		o(r)
	}
	w := httptest.NewRecorder()
	m.http.handler.ServeHTTP(w, r)
	res := w.Result()
	read, _ := io.ReadAll(res.Body)
	return sent{status: res.StatusCode, header: res.Header, body: string(read)}
}

// uploadBody sends body as an upload with the declared length.
func (m *mediaEnv) uploadBody(ctx context.Context, s signedIn, path, query string, body io.Reader, length int64, options ...option) sent {
	return m.serveRaw(ctx, http.MethodPost, path+"/media/uploads?"+query, "application/octet-stream", body, length, append(authOf(s), options...)...)
}

func (m *mediaEnv) upload(s signedIn, path, op, ref string, replace bool, data []byte, options ...option) sent {
	return m.uploadBody(context.Background(), s, path, uploadQuery(op, ref, replace), bytes.NewReader(data), int64(len(data)), options...)
}

func (m *mediaEnv) postJSON(s signedIn, target string, body any, options ...option) sent {
	raw, err := json.Marshal(body)
	if err != nil {
		m.t.Fatal(err)
	}
	return m.serveRaw(context.Background(), http.MethodPost, target, "application/json", bytes.NewReader(raw), int64(len(raw)), append(authOf(s), options...)...)
}

func (m *mediaEnv) attach(s signedIn, path, op, assetID, ref string, options ...option) sent {
	return m.postJSON(s, path+"/media/references", map[string]string{"operation_id": op, "asset_id": assetID, "reference_id": ref}, options...)
}

func (m *mediaEnv) detach(s signedIn, path, op, ref string, options ...option) sent {
	return m.postJSON(s, path+"/media/references/"+ref+"/detach", map[string]string{"operation_id": op}, options...)
}

func (m *mediaEnv) getUsage(s signedIn, path string) sent {
	return m.serveRaw(context.Background(), http.MethodGet, path+"/media/usage", "", nil, 0, withSession(s.token))
}

// usageJSON is the exact public usage body.
type usageJSON struct {
	QuotaBytes     int64 `json:"quota_bytes"`
	UsedBytes      int64 `json:"used_bytes"`
	ReservedBytes  int64 `json:"reserved_bytes"`
	AvailableBytes int64 `json:"available_bytes"`
}

func usageFor(used, reserved int64) usageJSON {
	return usageJSON{QuotaBytes: media.QuotaBytes, UsedBytes: used, ReservedBytes: reserved, AvailableBytes: media.QuotaBytes - used - reserved}
}

// uploadJSON is the exact upload body; object_id is absent when the upload
// created no canvas object.
type uploadJSON struct {
	AssetID  string    `json:"asset_id"`
	ObjectID string    `json:"object_id,omitempty"`
	Kind     string    `json:"kind"`
	Usage    usageJSON `json:"usage"`
}

type attachJSON struct {
	AssetID string    `json:"asset_id"`
	Usage   usageJSON `json:"usage"`
}

type detachJSON struct {
	Usage usageJSON `json:"usage"`
}

// strictBody decodes a success body, failing on a missing or extra field at
// any level.
func strictBody[T any](t *testing.T, s sent, status int) T {
	t.Helper()
	if s.status != status {
		t.Fatalf("status = %d, want %d: %s", s.status, status, s.body)
	}
	var into T
	decoder := json.NewDecoder(strings.NewReader(s.body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&into); err != nil {
		t.Fatalf("decode %s: %v", s.body, err)
	}
	var generic, again map[string]any
	_ = json.Unmarshal([]byte(s.body), &generic)
	encoded, _ := json.Marshal(into)
	_ = json.Unmarshal(encoded, &again)
	if !fieldsEqual(generic, again) {
		t.Fatalf("body %s has fields other than %s", s.body, encoded)
	}
	return into
}

// fieldsEqual compares the field names of two decoded objects, recursively.
func fieldsEqual(a, b map[string]any) bool {
	if !slices.Equal(slices.Sorted(maps.Keys(a)), slices.Sorted(maps.Keys(b))) {
		return false
	}
	for k, v := range a {
		if nested, ok := v.(map[string]any); ok {
			other, ok := b[k].(map[string]any)
			if !ok || !fieldsEqual(nested, other) {
				return false
			}
		}
	}
	return true
}

func wantUploaded(t *testing.T, s sent, used, reserved int64) uploadJSON {
	t.Helper()
	got := strictBody[uploadJSON](t, s, http.StatusCreated)
	if got.AssetID == "" || got.Kind != "image" || (got.ObjectID != "" && got.ObjectID != got.AssetID) || got.Usage != usageFor(used, reserved) {
		t.Fatalf("upload = %+v, want an image asset with usage %+v", got, usageFor(used, reserved))
	}
	return got
}

// wantMediaError checks the exact error body; reason is the rejection code
// or empty.
func wantMediaError(t *testing.T, s sent, status int, code, reason string) {
	t.Helper()
	id := s.header.Get(httpx.CorrelationHeader)
	want := fmt.Sprintf(`{"error":%q,"correlation_id":%q}`, code, id)
	if reason != "" {
		want = fmt.Sprintf(`{"error":%q,"reason":%q,"correlation_id":%q}`, code, reason, id)
	}
	if s.status != status || strings.TrimSpace(s.body) != want {
		t.Fatalf("answer = %d %s, want %d %s", s.status, s.body, status, want)
	}
}

func (m *mediaEnv) usage(s streamer) media.Usage {
	m.t.Helper()
	usage, err := m.store.Usage(context.Background(), s.media())
	if err != nil {
		m.t.Fatalf("Usage: %v", err)
	}
	return usage
}

func (m *mediaEnv) wantUsage(s streamer, used, reserved int64) {
	m.t.Helper()
	want := media.Usage{QuotaBytes: media.QuotaBytes, UsedBytes: used, ReservedBytes: reserved}
	if got := m.usage(s); got != want {
		m.t.Fatalf("usage = %+v, want %+v", got, want)
	}
}

// seed commits a ready asset of n bytes straight to the store, without a
// stored object, to bring a canvas near its quota.
func (m *mediaEnv) seed(s streamer, n int64, ref string) string {
	m.t.Helper()
	ctx := context.Background()
	reserved, err := m.store.Apply(ctx, s.media(), media.Operation{ID: "seed-reserve-" + ref}, media.Reserve{DeclaredBytes: n})
	if err != nil {
		m.t.Fatalf("seed Reserve: %v", err)
	}
	if _, err := m.store.Apply(ctx, s.media(), media.Operation{ID: "seed-complete-" + ref},
		media.Complete{AssetID: reserved.AssetID, ReferenceID: ref, Kind: media.KindImage}); err != nil {
		m.t.Fatalf("seed Complete: %v", err)
	}
	m.seeded[reserved.StorageKey] = true
	return reserved.AssetID
}

// wantBucket checks the bucket holds exactly the objects of the ready assets
// that are not seeded, and no incomplete upload, and that no asset is left in
// another state.
func (m *mediaEnv) wantBucket() {
	m.t.Helper()
	rows, err := m.pool.Query(context.Background(), "SELECT storage_key FROM media_assets WHERE state = 'ready'")
	if err != nil {
		m.t.Fatal(err)
	}
	var want []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			m.t.Fatal(err)
		}
		if !m.seeded[key] {
			want = append(want, key)
		}
	}
	rows.Close()
	got := m.minio.Keys(m.t)
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		m.t.Fatalf("bucket keys = %v, want the ready objects %v", got, want)
	}
	if incomplete := m.minio.IncompleteUploads(m.t); len(incomplete) != 0 {
		m.t.Fatalf("incomplete multipart uploads = %v", incomplete)
	}
	if n := m.count("SELECT count(*) FROM media_assets WHERE state <> 'ready'"); n != 0 {
		m.t.Fatalf("%d assets are not ready, want none", n)
	}
}

// served reads a ready asset through the upload pipeline.
func (m *mediaEnv) served(s streamer, assetID string) ([]byte, error) {
	_, body, err := media.NewUploads(m.store, m.storage, nil).Open(context.Background(), s.media(), assetID)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return io.ReadAll(body)
}

func (m *mediaEnv) wantServed(s streamer, assetID string, data []byte) {
	m.t.Helper()
	got, err := m.served(s, assetID)
	if err != nil || !bytes.Equal(got, data) {
		m.t.Fatalf("Open(%s) = %d bytes, %v; want the %d bytes uploaded", assetID, len(got), err, len(data))
	}
}

func (m *mediaEnv) wantNotServed(s streamer, assetID string) {
	m.t.Helper()
	if _, err := m.served(s, assetID); !errors.Is(err, media.ErrNotFound) {
		m.t.Fatalf("Open(%s) = %v, want ErrNotFound", assetID, err)
	}
}

// unreadable fails the test if an upload body is read.
type unreadable struct{ t *testing.T }

func (u unreadable) Read([]byte) (int, error) {
	u.t.Error("the upload body was read")
	return 0, io.EOF
}

// S1: the quota is inclusive. Uploads that bring the canvas to exactly
// 1 000 000 000 bytes are accepted; one more byte is rejected with the usage
// and the bucket unchanged, and a declared size above the quota is rejected
// before anything is reserved or read.
func TestMediaQuotaIsInclusive(t *testing.T) {
	m := newMediaEnv(t, httpapi.DefaultLimits)
	alice := m.streamer(m.op, "alice1", "alice@example.com")
	bob := m.streamer(m.op, "bob001", "bob@example.com")
	const last = 64 << 10
	seeded := media.QuotaBytes - 2*last
	m.seed(alice, seeded, "seed")

	first := randomBytes(t, last)
	a := wantUploaded(t, m.upload(alice.signedIn, alice.path, "op-quota-01", "ref-01", false, first), seeded+last, 0)
	b := wantUploaded(t, m.upload(alice.signedIn, alice.path, "op-quota-02", "ref-02", false, randomBytes(t, last)), media.QuotaBytes, 0)
	if got := strictBody[usageJSON](t, m.getUsage(alice.signedIn, alice.path), http.StatusOK); got != usageFor(media.QuotaBytes, 0) || got.AvailableBytes != 0 {
		t.Fatalf("usage at the quota = %+v", got)
	}
	m.wantServed(alice, a.AssetID, first)
	m.wantBucket()
	if b.AssetID == a.AssetID {
		t.Fatal("two uploads share an asset")
	}

	wantMediaError(t, m.upload(alice.signedIn, alice.path, "op-quota-03", "ref-03", false, []byte{1}), http.StatusRequestEntityTooLarge, "quota_exceeded", "")
	m.wantUsage(alice, media.QuotaBytes, 0)
	m.wantBucket()

	over := m.uploadBody(context.Background(), bob.signedIn, bob.path, uploadQuery("op-quota-04", "ref-04", false), unreadable{t}, media.QuotaBytes+1)
	wantMediaError(t, over, http.StatusRequestEntityTooLarge, "quota_exceeded", "")
	m.wantUsage(bob, 0, 0)
	if n := m.count("SELECT count(*) FROM media_operations WHERE tenant_id = $1", bob.tenantID); n != 0 {
		t.Fatalf("an oversized upload retained %d operations", n)
	}
}

// S2: every upload counts its full size, even when it duplicates the bytes
// of another.
func TestMediaDuplicatesCountTwice(t *testing.T) {
	m := newMediaEnv(t, httpapi.DefaultLimits)
	alice := m.streamer(m.op, "alice1", "alice@example.com")
	data := randomBytes(t, 50_000)
	first := wantUploaded(t, m.upload(alice.signedIn, alice.path, "op-dup-01", "ref-01", false, data), 50_000, 0)
	second := wantUploaded(t, m.upload(alice.signedIn, alice.path, "op-dup-02", "ref-02", false, data), 100_000, 0)
	if first.AssetID == second.AssetID {
		t.Fatal("a duplicate upload reused the first asset")
	}
	m.wantUsage(alice, 100_000, 0)
	m.wantServed(alice, first.AssetID, data)
	m.wantServed(alice, second.AssetID, data)
	m.wantBucket()
	if got := len(m.minio.Keys(t)); got != 2 {
		t.Fatalf("bucket holds %d objects, want 2", got)
	}
}

// S3: two concurrent uploads that do not both fit are decided atomically:
// exactly one is stored, the other is rejected without leaving an object,
// and the usage never exceeds the quota.
func TestMediaConcurrentUploadsNearTheLimit(t *testing.T) {
	m := newMediaEnv(t, httpapi.DefaultLimits)
	for round := range 3 {
		s := m.streamer(m.op, fmt.Sprintf("race%02d", round), fmt.Sprintf("race%d@example.com", round))
		const size = 40_000
		m.seed(s, media.QuotaBytes-size-size/2, "seed")
		start := make(chan struct{})
		results := make(chan sent, 2)
		var wg sync.WaitGroup
		for i := range 2 {
			data := randomBytes(t, size)
			wg.Go(func() {
				<-start
				results <- m.upload(s.signedIn, s.path, fmt.Sprintf("op-race-%d-%d", round, i), fmt.Sprintf("ref-%d", i), false, data)
			})
		}
		close(start)
		wg.Wait()
		close(results)
		var won, lost []sent
		for r := range results {
			if r.status == http.StatusCreated {
				won = append(won, r)
			} else {
				lost = append(lost, r)
			}
		}
		if len(won) != 1 || len(lost) != 1 {
			t.Fatalf("round %d: %d uploads won and %d lost, want one each", round, len(won), len(lost))
		}
		wantUploaded(t, won[0], media.QuotaBytes-size/2, 0)
		wantMediaError(t, lost[0], http.StatusRequestEntityTooLarge, "quota_exceeded", "")
		m.wantUsage(s, media.QuotaBytes-size/2, 0)
		m.wantBucket()
	}
}

// S4: a rejected, cancelled, or failed upload releases its reservation and
// leaves no stored bytes, and the asset is never served.
func TestMediaFailedUploadsReleaseTheirReservation(t *testing.T) {
	m := newMediaEnv(t, httpapi.DefaultLimits)
	alice := m.streamer(m.op, "alice1", "alice@example.com")
	kept := randomBytes(t, 10_000)
	keptAsset := wantUploaded(t, m.upload(alice.signedIn, alice.path, "op-fail-00", "kept", false, kept), 10_000, 0)
	data := randomBytes(t, 200<<10)

	t.Run("rejected by inspection", func(t *testing.T) {
		m.faults.set(func(f *mediaFaults) {
			f.inspect = func(context.Context, int64) (media.Inspection, error) {
				return media.Inspection{}, media.Validate(media.Inspection{Kind: "svg"}, media.DefaultLimits())
			}
		})
		defer m.faults.reset()
		wantMediaError(t, m.upload(alice.signedIn, alice.path, "op-fail-01", "ref-01", false, data), http.StatusUnprocessableEntity, "media_rejected", "unsupported_kind")
		m.wantUsage(alice, 10_000, 0)
		m.wantBucket()
	})

	t.Run("cancelled while the body streams", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		body := &stallingBody{prefix: bytes.NewReader(data[:64<<10]), started: make(chan struct{}, 1), release: make(chan struct{})}
		defer close(body.release)
		go func() {
			select {
			case <-body.started:
			case <-time.After(time.Minute):
			}
			cancel()
		}()
		answer := m.uploadBody(ctx, alice.signedIn, alice.path, uploadQuery("op-fail-02", "ref-02", false), body, int64(len(data)))
		wantMediaError(t, answer, http.StatusBadGateway, "upload_failed", "")
		m.wantUsage(alice, 10_000, 0)
		m.wantBucket()
	})

	t.Run("storage fails", func(t *testing.T) {
		m.faults.set(func(f *mediaFaults) { f.failPut = errors.New("disk full at https://bucket.example.com") })
		defer m.faults.reset()
		answer := m.upload(alice.signedIn, alice.path, "op-fail-03", "ref-03", false, data)
		wantMediaError(t, answer, http.StatusBadGateway, "upload_failed", "")
		if strings.Contains(answer.body, "bucket") {
			t.Fatalf("the answer reflects the storage error: %s", answer.body)
		}
		m.wantUsage(alice, 10_000, 0)
		m.wantBucket()
	})

	t.Run("body shorter than its Content-Length", func(t *testing.T) {
		answer := m.uploadBody(context.Background(), alice.signedIn, alice.path, uploadQuery("op-fail-04", "ref-04", false), bytes.NewReader(data[:1000]), int64(len(data)))
		wantMediaError(t, answer, http.StatusBadGateway, "upload_failed", "")
		m.wantUsage(alice, 10_000, 0)
		m.wantBucket()
	})

	m.wantServed(alice, keptAsset.AssetID, kept)
}

// stallingBody returns prefix, signals started, then blocks until release
// is closed and fails, like a client connection that stops sending.
type stallingBody struct {
	prefix  *bytes.Reader
	started chan struct{}
	release chan struct{}
}

func (r *stallingBody) Read(p []byte) (int, error) {
	if r.prefix.Len() > 0 {
		return r.prefix.Read(p)
	}
	select {
	case r.started <- struct{}{}:
	default:
	}
	<-r.release
	return 0, errors.New("client went away")
}

// The upload request is checked before any reservation: an unknown length,
// an empty or non-binary body, or malformed parameters are rejected with
// nothing retained or stored.
func TestMediaUploadRejectsMalformedRequests(t *testing.T) {
	m := newMediaEnv(t, httpapi.DefaultLimits)
	alice := m.streamer(m.op, "alice1", "alice@example.com")
	valid := uploadQuery("op-bad-01", "ref-01", false)
	cases := []struct {
		name, query, contentType string
		length                   int64
		status                   int
		code                     string
	}{
		{"unknown length", valid, "application/octet-stream", -1, http.StatusLengthRequired, "length_required"},
		{"empty body", valid, "application/octet-stream", 0, http.StatusBadRequest, "bad_request"},
		{"JSON body", valid, "application/json", 10, http.StatusBadRequest, "bad_request"},
		{"no content type", valid, "", 10, http.StatusBadRequest, "bad_request"},
		{"replace is not a boolean", valid + "&replace=yes", "application/octet-stream", 10, http.StatusBadRequest, "bad_request"},
		{"no operation", "reference_id=ref-01", "application/octet-stream", 10, http.StatusBadRequest, "bad_request"},
		{"no reference", "operation_id=op-bad-01", "application/octet-stream", 10, http.StatusBadRequest, "bad_request"},
		{"operation too long", uploadQuery(strings.Repeat("o", 125), "ref-01", false), "application/octet-stream", 10, http.StatusBadRequest, "bad_request"},
		{"reference with a space", "operation_id=op-bad-01&reference_id=a%20b", "application/octet-stream", 10, http.StatusBadRequest, "bad_request"},
		{"replacing a missing reference", uploadQuery("op-bad-02", "missing", true), "application/octet-stream", 10, http.StatusNotFound, "not_found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			answer := m.serveRaw(context.Background(), http.MethodPost, alice.path+"/media/uploads?"+tc.query, tc.contentType, unreadable{t}, tc.length, authOf(alice.signedIn)...)
			wantMediaError(t, answer, tc.status, tc.code, "")
			m.wantUsage(alice, 0, 0)
			m.wantBucket()
			if n := m.count("SELECT count(*) FROM media_assets"); n != 0 {
				t.Fatalf("%d assets written", n)
			}
		})
	}
}

// S5: detaching a reference releases nothing while another one remains;
// detaching the last one releases the used bytes at once.
func TestMediaReferences(t *testing.T) {
	m := newMediaEnv(t, httpapi.DefaultLimits)
	alice := m.streamer(m.op, "alice1", "alice@example.com")
	_, mod := m.moderatorOf(alice, "mod001", "mod@example.com")
	data := randomBytes(t, 30_000)
	up := wantUploaded(t, m.upload(alice.signedIn, alice.path, "op-ref-01", "ref-01", false, data), 30_000, 0)

	attached := strictBody[attachJSON](t, m.attach(mod, alice.path, "op-ref-02", up.AssetID, "ref-02"), http.StatusOK)
	if attached != (attachJSON{AssetID: up.AssetID, Usage: usageFor(30_000, 0)}) {
		t.Fatalf("attach = %+v", attached)
	}
	wantMediaError(t, m.attach(alice.signedIn, alice.path, "op-ref-03", up.AssetID, "ref-02"), http.StatusConflict, "reference_exists", "")
	wantMediaError(t, m.attach(alice.signedIn, alice.path, "op-ref-04", "no-such-asset", "ref-04"), http.StatusNotFound, "not_found", "")
	wantMediaError(t, m.detach(alice.signedIn, alice.path, "op-ref-05", "no-such-reference"), http.StatusNotFound, "not_found", "")
	wantMediaError(t, m.attach(alice.signedIn, alice.path, "op-ref-06", up.AssetID, "bad ref"), http.StatusBadRequest, "bad_request", "")

	if got := strictBody[detachJSON](t, m.detach(alice.signedIn, alice.path, "op-ref-07", "ref-01"), http.StatusOK); got.Usage != usageFor(30_000, 0) {
		t.Fatalf("detach of one of two references = %+v", got)
	}
	m.wantServed(alice, up.AssetID, data)
	if got := strictBody[detachJSON](t, m.detach(mod, alice.path, "op-ref-08", "ref-02"), http.StatusOK); got.Usage != usageFor(0, 0) {
		t.Fatalf("detach of the last reference = %+v", got)
	}
	m.wantUsage(alice, 0, 0)
	m.wantNotServed(alice, up.AssetID)
	wantMediaError(t, m.attach(alice.signedIn, alice.path, "op-ref-09", up.AssetID, "ref-09"), http.StatusNotFound, "not_found", "")
}

// S6: a replacement needs room for the whole new file; without it the old
// asset stays served under its reference. A successful replacement moves the
// reference and releases the old asset in the same commit.
func TestMediaReplacement(t *testing.T) {
	m := newMediaEnv(t, httpapi.DefaultLimits)
	alice := m.streamer(m.op, "alice1", "alice@example.com")
	old := randomBytes(t, 20_000)
	oldAsset := wantUploaded(t, m.upload(alice.signedIn, alice.path, "op-rep-01", "hero", false, old), 20_000, 0)
	fill := media.QuotaBytes - 20_000 - 29_999
	m.seed(alice, fill, "fill")

	replacement := randomBytes(t, 30_000)
	wantMediaError(t, m.upload(alice.signedIn, alice.path, "op-rep-02", "hero", true, replacement), http.StatusRequestEntityTooLarge, "quota_exceeded", "")
	m.wantUsage(alice, fill+20_000, 0)
	m.wantServed(alice, oldAsset.AssetID, old)
	wantMediaError(t, m.attach(alice.signedIn, alice.path, "op-rep-03", oldAsset.AssetID, "hero"), http.StatusConflict, "reference_exists", "")
	m.wantBucket()

	strictBody[detachJSON](t, m.detach(alice.signedIn, alice.path, "op-rep-04", "fill"), http.StatusOK)
	m.wantUsage(alice, 20_000, 0)
	replaced := wantUploaded(t, m.upload(alice.signedIn, alice.path, "op-rep-05", "hero", true, replacement), 30_000, 0)
	m.wantNotServed(alice, oldAsset.AssetID)
	m.wantServed(alice, replaced.AssetID, replacement)
	// The reference now names the replacement: detaching it releases it.
	if got := strictBody[detachJSON](t, m.detach(alice.signedIn, alice.path, "op-rep-06", "hero"), http.StatusOK); got.Usage != usageFor(0, 0) {
		t.Fatalf("detach after the replacement = %+v", got)
	}
}

// S8: another tenant's owner or moderator gets the canvas route's forbidden
// on every media route of the canvas, which stays unchanged, and the usage
// body holds only the four aggregate fields.
func TestMediaIsolatesTenants(t *testing.T) {
	m := newMediaEnv(t, httpapi.DefaultLimits)
	alice := m.streamer(m.op, "alice1", "alice@example.com")
	bob := m.streamer(m.op, "bob001", "bob@example.com")
	_, bobMod := m.moderatorOf(bob, "bobmod", "bobmod@example.com")
	data := randomBytes(t, 12_345)
	up := wantUploaded(t, m.upload(alice.signedIn, alice.path, "op-iso-01", "ref-01", false, data), 12_345, 0)

	raw := m.getUsage(alice.signedIn, alice.path)
	if got := strictBody[usageJSON](t, raw, http.StatusOK); got != usageFor(12_345, 0) {
		t.Fatalf("usage = %+v", got)
	}
	if strings.Contains(raw.body, up.AssetID) || strings.Contains(raw.body, "ref-01") {
		t.Fatalf("the usage body names an asset: %s", raw.body)
	}

	for name, s := range map[string]signedIn{"other owner": bob.signedIn, "other moderator": bobMod} {
		t.Run(name, func(t *testing.T) {
			wantMediaError(t, m.getUsage(s, alice.path), http.StatusForbidden, "forbidden", "")
			wantMediaError(t, m.upload(s, alice.path, "op-iso-02", "ref-02", false, data), http.StatusForbidden, "forbidden", "")
			wantMediaError(t, m.attach(s, alice.path, "op-iso-03", up.AssetID, "ref-03"), http.StatusForbidden, "forbidden", "")
			wantMediaError(t, m.detach(s, alice.path, "op-iso-04", "ref-01"), http.StatusForbidden, "forbidden", "")
			// The other tenant's own canvas cannot reach the asset either.
			wantMediaError(t, m.attach(s, bob.path, "op-iso-05", up.AssetID, "ref-05"), http.StatusNotFound, "not_found", "")
			m.wantUsage(alice, 12_345, 0)
			m.wantUsage(bob, 0, 0)
			m.wantServed(alice, up.AssetID, data)
			m.wantBucket()
		})
	}
	// A missing canvas of the caller's own tenant is the same forbidden.
	missing := "/api/tenants/" + alice.tenantID + "/canvases/no-such-canvas"
	wantMediaError(t, m.getUsage(alice.signedIn, missing), http.StatusForbidden, "forbidden", "")
	wantMediaError(t, m.upload(alice.signedIn, missing, "op-iso-06", "ref-06", false, data), http.StatusForbidden, "forbidden", "")
	wantMediaError(t, m.attach(alice.signedIn, missing, "op-iso-07", up.AssetID, "ref-07"), http.StatusForbidden, "forbidden", "")
	wantMediaError(t, m.detach(alice.signedIn, missing, "op-iso-08", "ref-01"), http.StatusForbidden, "forbidden", "")
	m.wantBucket()
}

// S15 (CTRL-WEB-01): the media mutations are ingress Session routes: a
// hostile or absent Origin, or a missing or invalid CSRF token, is refused
// before the domain, with nothing reserved or stored, for an undeclared and
// for a declared image upload alike; the valid token and Origin are accepted.
func TestMediaMutationsCheckOriginAndCSRFFirst(t *testing.T) {
	m := newMediaEnv(t, httpapi.DefaultLimits)
	alice := m.streamer(m.op, "alice1", "alice@example.com")
	up := wantUploaded(t, m.upload(alice.signedIn, alice.path, "op-web-01", "ref-01", false, []byte("kept")), 4, 0)
	before := m.count("SELECT count(*) FROM media_operations")
	for name, refuse := range map[string][]option{
		"hostile Origin": {withOrigin("https://evil.test")},
		"absent Origin":  {withoutOrigin()},
		"missing CSRF":   {withCSRF("")},
		"invalid CSRF":   {withCSRF("not-the-token")},
	} {
		t.Run(name, func(t *testing.T) {
			for _, contentType := range []string{"application/octet-stream", "image/png", "image/svg+xml"} {
				answer := m.serveRaw(context.Background(), http.MethodPost, alice.path+"/media/uploads?"+uploadQuery("op-web-02", "ref-02", false),
					contentType, unreadable{t}, 4, append(authOf(alice.signedIn), refuse...)...)
				wantMediaError(t, answer, http.StatusForbidden, "forbidden", "")
			}
			wantMediaError(t, m.attach(alice.signedIn, alice.path, "op-web-03", up.AssetID, "ref-03", refuse...), http.StatusForbidden, "forbidden", "")
			wantMediaError(t, m.detach(alice.signedIn, alice.path, "op-web-04", "ref-01", refuse...), http.StatusForbidden, "forbidden", "")
			if got := m.count("SELECT count(*) FROM media_operations"); got != before {
				t.Fatalf("media operations %d -> %d", before, got)
			}
			m.wantUsage(alice, 4, 0)
			m.wantBucket()
		})
	}
	m.faults.set(inspectAs(media.KindImage, "image/png"))
	defer m.faults.reset()
	data := pngOf(t, 2, 2)
	wantUploaded(t, m.uploadAs(alice.signedIn, alice.path, "op-web-05", "ref-05", "image/png", data), 4+int64(len(data)), 0)
}

// S9: access lost between the route check and a commit is rechecked at the
// commit: the request is forbidden and nothing is committed, and a failed
// upload still releases its reservation.
func TestMediaReauthorizesAtCommit(t *testing.T) {
	m := newMediaEnv(t, httpapi.DefaultLimits)
	alice := m.streamer(m.op, "alice1", "alice@example.com")
	modID, mod := m.moderatorOf(alice, "mod001", "mod@example.com")
	keptData := randomBytes(t, 1000)
	kept := wantUploaded(t, m.upload(alice.signedIn, alice.path, "op-auth-00", "kept", false, keptData), 1000, 0)
	var operatorID string
	if err := m.pool.QueryRow(context.Background(), "SELECT account_id FROM operators").Scan(&operatorID); err != nil {
		t.Fatal(err)
	}
	service := subscription.NewService(postgres.NewSubscriptionStore(m.pool))
	deactivations := 0
	deactivate := func() {
		deactivations++
		got, err := service.Deactivate(context.Background(), subscription.DeactivateRequest{
			OperatorID: access.AccountID(operatorID), OperationID: fmt.Sprintf("op-media-deact-%d", deactivations), Email: "alice@example.com",
		})
		if err != nil || !got.Deactivated {
			t.Errorf("Deactivate = %+v, %v", got, err)
		}
	}
	reactivate := func() {
		m.http.activate(m.op, fmt.Sprintf("op-media-react-%d", deactivations), "alice@example.com")
	}
	revoke := func() {
		if _, err := m.pool.Exec(context.Background(), "DELETE FROM memberships WHERE account_id = $1", string(modID)); err != nil {
			t.Errorf("revoke moderator: %v", err)
		}
	}
	restore := func() {
		if _, err := m.pool.Exec(context.Background(), "INSERT INTO memberships (tenant_id, account_id, role) VALUES ($1, $2, 'moderator')", alice.tenantID, string(modID)); err != nil {
			t.Fatalf("restore moderator: %v", err)
		}
	}
	// hooked serves with a hook that runs after the given membership read of
	// alice's tenant: 1 is the route check, 2 the reauthorization of the
	// first command, 3 that of the second.
	hooked := func(afterCalls int, hook func()) *deactivatingMemberships {
		h := &deactivatingMemberships{tenantID: alice.tenantID, afterCalls: afterCalls, deactivate: hook}
		m.memberships = func(store access.MembershipStore) access.MembershipStore {
			h.MembershipStore = store
			return h
		}
		m.serve(m.canvases)
		return h
	}
	unchanged := func(t *testing.T) {
		t.Helper()
		m.wantUsage(alice, 1000, 0)
		m.wantBucket()
		m.wantServed(alice, kept.AssetID, keptData)
	}

	t.Run("moderator revoked after the reservation", func(t *testing.T) {
		hooked(2, revoke)
		defer restore()
		wantMediaError(t, m.upload(mod, alice.path, "op-auth-01", "ref-01", false, randomBytes(t, 5000)), http.StatusForbidden, "forbidden", "")
		unchanged(t)
	})
	t.Run("tenant deactivated after the completion was reauthorized", func(t *testing.T) {
		hooked(3, deactivate)
		defer reactivate()
		wantMediaError(t, m.upload(alice.signedIn, alice.path, "op-auth-02", "ref-02", false, randomBytes(t, 5000)), http.StatusForbidden, "forbidden", "")
		unchanged(t)
	})
	t.Run("moderator revoked before an attach commits", func(t *testing.T) {
		hooked(1, revoke)
		defer restore()
		wantMediaError(t, m.attach(mod, alice.path, "op-auth-03", kept.AssetID, "ref-03"), http.StatusForbidden, "forbidden", "")
		unchanged(t)
		if n := m.count("SELECT count(*) FROM media_references WHERE tenant_id = $1", alice.tenantID); n != 1 {
			t.Fatalf("references = %d, want 1", n)
		}
	})
	t.Run("tenant deactivated before a detach commits", func(t *testing.T) {
		hooked(2, deactivate)
		defer reactivate()
		wantMediaError(t, m.detach(alice.signedIn, alice.path, "op-auth-04", "kept"), http.StatusForbidden, "forbidden", "")
		unchanged(t)
	})
	// After the access returns, the same canvas works again.
	hooked(0, nil)
	wantUploaded(t, m.upload(mod, alice.path, "op-auth-05", "ref-05", false, randomBytes(t, 10)), 1010, 0)
}

// reconciler runs TE-013 passes over the store and bucket, with a clock past
// the orphan grace of every object.
func (m *mediaEnv) reconciler() *media.Reconciler {
	m.t.Helper()
	r, err := media.NewReconciler(media.ReconcilerConfig{Store: m.store, Storage: m.storage, Now: func() time.Time { return time.Now().Add(2 * time.Hour) }})
	if err != nil {
		m.t.Fatalf("NewReconciler: %v", err)
	}
	m.t.Cleanup(func() { _ = r.Close(context.Background()) })
	return r
}

func reconcile(t *testing.T, r *media.Reconciler, purged int) {
	t.Helper()
	report, err := r.Run(context.Background(), "")
	if err != nil || !report.Complete || report.Failures != 0 || report.Purged != purged ||
		report.OrphansDeleted != 0 || report.MissingReady != 0 {
		t.Fatalf("Run = %+v, %v; want a complete pass that purges %d", report, err, purged)
	}
}

// wantCrashed checks the answer of a simulated crash: httpx.Recover, which
// runs outside the correlation middleware, answers a bare internal error.
func wantCrashed(t *testing.T, s sent) {
	t.Helper()
	if s.status != http.StatusInternalServerError || strings.TrimSpace(s.body) != `{"error":"internal"}` {
		t.Fatalf("answer = %d %s, want the internal error of a crash", s.status, s.body)
	}
}

func (m *mediaEnv) expireReservations() {
	m.clock.Set(m.clock.Now().Add(postgres.DefaultReservationTTL + time.Second))
}

// S12 (CTRL-S3-01): every PostgreSQL/S3 transition of the upload, replace,
// and detach journey is interrupted through HTTP; reconciliation converges
// without serving an incomplete asset or deleting a valid one, and the usage
// counts only the ready assets.
func TestMediaReconcilesEveryInterruptedTransition(t *testing.T) {
	m := newMediaEnv(t, httpapi.DefaultLimits)
	alice := m.streamer(m.op, "alice1", "alice@example.com")
	r := m.reconciler()
	// ready holds the bytes of every ready asset.
	ready := map[string][]byte{}
	used := func() int64 {
		var n int64
		for _, data := range ready {
			n += int64(len(data))
		}
		return n
	}
	upload := func(op, ref string) string {
		data := randomBytes(t, 4096+len(ready))
		up := wantUploaded(t, m.upload(alice.signedIn, alice.path, op, ref, false, data), used()+int64(len(data)), 0)
		ready[up.AssetID] = data
		return up.AssetID
	}
	keptID := upload("op-s3-00", "kept")
	converged := func(t *testing.T) {
		t.Helper()
		reconcile(t, r, 0)
		m.wantUsage(alice, used(), 0)
		m.wantBucket()
		for id, data := range ready {
			m.wantServed(alice, id, data)
		}
	}
	crash := func(t *testing.T, op, ref string, replace bool, data []byte) {
		t.Helper()
		defer m.faults.reset()
		wantCrashed(t, m.upload(alice.signedIn, alice.path, op, ref, replace, data))
	}
	pendingAsset := func(t *testing.T) string {
		t.Helper()
		var id string
		if err := m.pool.QueryRow(context.Background(), "SELECT asset_id FROM media_assets WHERE state = 'pending'").Scan(&id); err != nil {
			t.Fatalf("pending asset: %v", err)
		}
		return id
	}

	t.Run("reserve committed, crash before the Put", func(t *testing.T) {
		m.faults.set(func(f *mediaFaults) { f.beforePut = func() { panic(crashed("before Put")) } })
		crash(t, "op-s3-01", "ref-01", false, randomBytes(t, 3000))
		pending := pendingAsset(t)
		m.wantNotServed(alice, pending)
		m.wantUsage(alice, used(), 3000)
		reconcile(t, r, 0)
		m.expireReservations()
		reconcile(t, r, 1)
		converged(t)
	})
	t.Run("Put done, crash before the completion", func(t *testing.T) {
		m.faults.set(func(f *mediaFaults) { f.afterPut = func() { panic(crashed("after Put")) } })
		crash(t, "op-s3-02", "ref-02", false, randomBytes(t, 3000))
		pending := pendingAsset(t)
		m.wantNotServed(alice, pending)
		if n := len(m.minio.Keys(t)); n != len(ready)+1 {
			t.Fatalf("bucket holds %d objects, want the %d ready and the pending one", n, len(ready))
		}
		// The pending asset holds its object until its reservation expires.
		reconcile(t, r, 0)
		if n := len(m.minio.Keys(t)); n != len(ready)+1 {
			t.Fatal("the object of a pending asset was deleted")
		}
		m.expireReservations()
		reconcile(t, r, 1)
		converged(t)
	})
	t.Run("rejection committed, crash before the Delete", func(t *testing.T) {
		m.faults.set(func(f *mediaFaults) {
			f.inspect = func(context.Context, int64) (media.Inspection, error) {
				return media.Inspection{}, media.Validate(media.Inspection{Kind: "svg"}, media.DefaultLimits())
			}
			f.committed = func(_ context.Context, command media.Command) {
				if _, ok := command.(media.Abandon); ok {
					panic(crashed("after Abandon"))
				}
			}
		})
		crash(t, "op-s3-03", "ref-03", false, randomBytes(t, 3000))
		m.wantUsage(alice, used(), 0)
		reconcile(t, r, 1)
		converged(t)
	})
	t.Run("replacement completed, crash before the old asset is deleted", func(t *testing.T) {
		old := upload("op-s3-04", "hero")
		m.faults.set(func(f *mediaFaults) {
			f.committed = func(_ context.Context, command media.Command) {
				if _, ok := command.(media.Complete); ok {
					panic(crashed("after Complete"))
				}
			}
		})
		data := randomBytes(t, 3000)
		crash(t, "op-s3-05", "hero", true, data)
		delete(ready, old)
		m.wantNotServed(alice, old)
		var replacement string
		if err := m.pool.QueryRow(context.Background(), "SELECT asset_id FROM media_references WHERE tenant_id = $1 AND reference_id = 'hero'",
			alice.tenantID).Scan(&replacement); err != nil || replacement == old {
			t.Fatalf("hero names %q, %v; want the replacement", replacement, err)
		}
		m.wantServed(alice, replacement, data)
		ready[replacement] = data
		// A retry of the upload answers the committed replacement.
		retried := strictBody[uploadJSON](t, m.upload(alice.signedIn, alice.path, "op-s3-05", "hero", true, data), http.StatusCreated)
		if retried.AssetID != replacement || retried.Usage != usageFor(used(), 0) {
			t.Fatalf("retry = %+v, want asset %s with usage %+v", retried, replacement, usageFor(used(), 0))
		}
		reconcile(t, r, 1)
		converged(t)
	})
	t.Run("detach committed, crash before the Delete", func(t *testing.T) {
		id := upload("op-s3-06", "ref-06")
		m.faults.set(func(f *mediaFaults) {
			f.committed = func(_ context.Context, command media.Command) {
				if _, ok := command.(media.Detach); ok {
					panic(crashed("after Detach"))
				}
			}
		})
		wantCrashed(t, m.detach(alice.signedIn, alice.path, "op-s3-07", "ref-06"))
		m.faults.reset()
		delete(ready, id)
		m.wantNotServed(alice, id)
		if got := strictBody[detachJSON](t, m.detach(alice.signedIn, alice.path, "op-s3-07", "ref-06"), http.StatusOK); got.Usage != usageFor(used(), 0) {
			t.Fatalf("detach retry = %+v", got)
		}
		reconcile(t, r, 1)
		converged(t)
	})
	m.wantServed(alice, keptID, ready[keptID])
}

// S13 (CTRL-IDEM-01): a retry of an upload, attach, or detach under the same
// operation ID, after an answer, after a client timeout, or from a new
// session, answers the same status and body with one effect; a different
// payload is a conflict; an upload retried while the first attempt runs is
// in progress; and once the 10-minute retention ends the operation ID is
// forgotten, so a retry is a new command: the upload finds its reference
// taken and stores nothing, the detach finds no reference, and the attach
// attaches again.
func TestMediaRetriesHaveOneEffect(t *testing.T) {
	m := newMediaEnv(t, httpapi.DefaultLimits)
	alice := m.streamer(m.op, "alice1", "alice@example.com")
	data := randomBytes(t, 7000)
	start := m.clock.Now()

	// The first answer is lost: the client times out right after the commit.
	ctx, cancel := context.WithCancel(context.Background())
	m.faults.set(func(f *mediaFaults) {
		f.committed = func(_ context.Context, command media.Command) {
			if _, ok := command.(media.Complete); ok {
				cancel()
			}
		}
	})
	first := m.uploadBody(ctx, alice.signedIn, alice.path, uploadQuery("op-idem-01", "ref-01", false), bytes.NewReader(data), int64(len(data)))
	m.faults.reset()
	up := wantUploaded(t, first, 7000, 0)
	retried := m.upload(alice.signedIn, alice.path, "op-idem-01", "ref-01", false, data)
	if retried.status != first.status || retried.body != first.body {
		t.Fatalf("retry after the timeout = %d %s, want %d %s", retried.status, retried.body, first.status, first.body)
	}
	reconnected := m.http.signIn("alice@example.com")
	again := m.upload(reconnected, alice.path, "op-idem-01", "ref-01", false, data)
	if again.status != first.status || again.body != first.body {
		t.Fatalf("retry after the reconnection = %d %s, want %d %s", again.status, again.body, first.status, first.body)
	}
	m.wantUsage(alice, 7000, 0)
	m.wantBucket()
	if n := len(m.minio.Keys(t)); n != 1 {
		t.Fatalf("bucket holds %d objects, want 1", n)
	}

	// A different payload under the same operation ID is a conflict.
	wantMediaError(t, m.upload(alice.signedIn, alice.path, "op-idem-01", "ref-01", false, data[:6999]), http.StatusConflict, "conflict", "")
	wantMediaError(t, m.upload(alice.signedIn, alice.path, "op-idem-01", "ref-01", true, data), http.StatusConflict, "conflict", "")
	m.wantUsage(alice, 7000, 0)

	attach := m.attach(alice.signedIn, alice.path, "op-idem-02", up.AssetID, "ref-02")
	strictBody[attachJSON](t, attach, http.StatusOK)
	if again := m.attach(reconnected, alice.path, "op-idem-02", up.AssetID, "ref-02"); again.status != attach.status || again.body != attach.body {
		t.Fatalf("attach retry = %d %s, want %d %s", again.status, again.body, attach.status, attach.body)
	}
	wantMediaError(t, m.attach(alice.signedIn, alice.path, "op-idem-02", up.AssetID, "ref-other"), http.StatusConflict, "conflict", "")
	detach := m.detach(alice.signedIn, alice.path, "op-idem-03", "ref-02")
	strictBody[detachJSON](t, detach, http.StatusOK)
	if again := m.detach(reconnected, alice.path, "op-idem-03", "ref-02"); again.status != detach.status || again.body != detach.body {
		t.Fatalf("detach retry = %d %s, want %d %s", again.status, again.body, detach.status, detach.body)
	}
	if n := m.count("SELECT count(*) FROM media_references WHERE tenant_id = $1", alice.tenantID); n != 1 {
		t.Fatalf("references = %d, want 1", n)
	}

	// A retry while the first attempt is still being inspected is in
	// progress, and later receives the committed answer.
	inspecting, release := make(chan struct{}), make(chan struct{})
	m.faults.set(func(f *mediaFaults) {
		f.inspect = func(_ context.Context, n int64) (media.Inspection, error) {
			close(inspecting)
			<-release
			return media.Inspection{Kind: media.KindImage, InputBytes: n, StreamCount: 1, Dimensions: media.Dimensions{Width: 1, Height: 1}}, nil
		}
	})
	slow := randomBytes(t, 3000)
	done := make(chan sent, 1)
	go func() { done <- m.upload(alice.signedIn, alice.path, "op-idem-04", "ref-04", false, slow) }()
	select {
	case <-inspecting:
	case <-time.After(time.Minute):
		t.Fatal("the first attempt never reached the inspection")
	}
	wantMediaError(t, m.upload(alice.signedIn, alice.path, "op-idem-04", "ref-04", false, slow), http.StatusConflict, "upload_in_progress", "")
	close(release)
	slowFirst := <-done
	m.faults.reset()
	wantUploaded(t, slowFirst, 10_000, 0)
	if again := m.upload(alice.signedIn, alice.path, "op-idem-04", "ref-04", false, slow); again.body != slowFirst.body {
		t.Fatalf("retry after the slow upload = %s, want %s", again.body, slowFirst.body)
	}

	// A rejected upload replays its rejection without a second reservation.
	m.faults.set(func(f *mediaFaults) {
		f.inspect = func(context.Context, int64) (media.Inspection, error) {
			return media.Inspection{}, media.Validate(media.Inspection{Kind: "svg"}, media.DefaultLimits())
		}
	})
	wantMediaError(t, m.upload(alice.signedIn, alice.path, "op-idem-05", "ref-05", false, slow), http.StatusUnprocessableEntity, "media_rejected", "unsupported_kind")
	m.faults.reset()
	wantMediaError(t, m.upload(alice.signedIn, alice.path, "op-idem-05", "ref-05", false, slow), http.StatusUnprocessableEntity, "media_rejected", "unsupported_kind")
	m.wantUsage(alice, 10_000, 0)
	m.wantBucket()

	// Just before the retention ends every operation still replays.
	m.clock.Set(start.Add(postgres.DefaultOperationRetention - time.Microsecond))
	if again := m.upload(alice.signedIn, alice.path, "op-idem-01", "ref-01", false, data); again.body != first.body {
		t.Fatalf("retry before the retention ends = %s, want %s", again.body, first.body)
	}
	if again := m.detach(alice.signedIn, alice.path, "op-idem-03", "ref-02"); again.body != detach.body {
		t.Fatalf("detach retry before the retention ends = %s, want %s", again.body, detach.body)
	}
	m.clock.Set(start.Add(postgres.DefaultOperationRetention))
	wantMediaError(t, m.upload(alice.signedIn, alice.path, "op-idem-01", "ref-01", false, data), http.StatusConflict, "reference_exists", "")
	m.wantUsage(alice, 10_000, 0)
	m.wantBucket()
	wantMediaError(t, m.detach(alice.signedIn, alice.path, "op-idem-03", "ref-02"), http.StatusNotFound, "not_found", "")
	if got := strictBody[attachJSON](t, m.attach(alice.signedIn, alice.path, "op-idem-02", up.AssetID, "ref-02"), http.StatusOK); got.Usage != usageFor(10_000, 0) {
		t.Fatalf("attach after the retention = %+v", got)
	}
	if n := m.count("SELECT count(*) FROM media_references WHERE tenant_id = $1", alice.tenantID); n != 3 {
		t.Fatalf("references = %d, want ref-01, ref-02, and ref-04", n)
	}
}

// S14, US-011 S16 (CTRL-RATE-01): the per-account upload limit answers 429
// with Retry-After before anything is reserved or read, whatever type the
// upload declares, recovers after its window, and leaves another streamer's
// account unaffected.
func TestMediaUploadLimitPerAccount(t *testing.T) {
	limits := httpapi.DefaultLimits
	limits.UploadsPerAccount = httpapi.Rate{Limit: 2, Window: time.Hour}
	m := newMediaEnv(t, limits)
	alice := m.streamer(m.op, "alice1", "alice@example.com")
	bob := m.streamer(m.op, "bob001", "bob@example.com")
	_, mod := m.moderatorOf(alice, "mod001", "mod@example.com")
	// Every upload inspects as a PNG, so undeclared and declared ones pass.
	m.faults.set(inspectAs(media.KindImage, "image/png"))

	wantUploaded(t, m.upload(alice.signedIn, alice.path, "op-rate-01", "ref-01", false, []byte("one")), 3, 0)
	wantUploaded(t, m.uploadAs(alice.signedIn, alice.path, "op-rate-02", "ref-02", "image/png", []byte("two")), 6, 0)
	limited := reasonCount(m.http.ingress, ingress.ReasonRateAccount)
	operations := m.count("SELECT count(*) FROM media_operations")
	for i, contentType := range []string{"application/octet-stream", "image/png", "image/svg+xml"} {
		refused := m.serveRaw(context.Background(), http.MethodPost, alice.path+"/media/uploads?"+uploadQuery("op-rate-03", "ref-03", false),
			contentType, unreadable{t}, 5, authOf(alice.signedIn)...)
		if got := refused.header.Get("Retry-After"); got != "3600" {
			t.Fatalf("%s: Retry-After = %q, want 3600", contentType, got)
		}
		wantMediaError(t, refused, http.StatusTooManyRequests, "rate_limited", "")
		if got := reasonCount(m.http.ingress, ingress.ReasonRateAccount); got != limited+uint64(i)+1 {
			t.Fatalf("rate_account count = %d, want %d", got, limited+uint64(i)+1)
		}
	}
	if got := m.count("SELECT count(*) FROM media_operations"); got != operations {
		t.Fatalf("media operations %d -> %d", operations, got)
	}
	m.wantUsage(alice, 6, 0)
	m.wantBucket()

	// Another streamer, and the moderator's own account, are unaffected.
	wantUploaded(t, m.uploadAs(bob.signedIn, bob.path, "op-rate-04", "ref-04", "image/png", []byte("bob")), 3, 0)
	wantUploaded(t, m.upload(mod, alice.path, "op-rate-05", "ref-05", false, []byte("mod")), 9, 0)

	m.limits.Set(m.limits.Now().Add(time.Hour))
	wantUploaded(t, m.uploadAs(alice.signedIn, alice.path, "op-rate-03", "ref-03", "image/png", []byte("three")), 14, 0)
}

// A server without media storage answers media_unavailable on the upload
// route before reserving, while the usage still answers.
func TestMediaUploadWithoutStorageIsUnavailable(t *testing.T) {
	m := newMediaEnv(t, httpapi.DefaultLimits)
	configure := m.configure
	m.configure = func(c *httpapi.Config) {
		configure(c)
		c.Storage = nil
	}
	m.serve(m.canvases)
	alice := m.streamer(m.op, "alice1", "alice@example.com")
	wantMediaError(t, m.upload(alice.signedIn, alice.path, "op-off-01", "ref-01", false, []byte("data")), http.StatusServiceUnavailable, "media_unavailable", "")
	if got := strictBody[usageJSON](t, m.getUsage(alice.signedIn, alice.path), http.StatusOK); got != usageFor(0, 0) {
		t.Fatalf("usage = %+v", got)
	}
	if n := m.count("SELECT count(*) FROM media_operations"); n != 0 {
		t.Fatalf("%d media operations retained", n)
	}
}

// S15 (CTRL-TRACE-01 primitive): the request's correlation ID reaches the
// retained media operations and the media usage outbox records, and the
// confirmation log carries the tenant, canvas, and usage.
func TestMediaCorrelationReachesStorageAndOutbox(t *testing.T) {
	m := newMediaEnv(t, httpapi.DefaultLimits)
	alice := m.streamer(m.op, "alice1", "alice@example.com")
	answer := m.upload(alice.signedIn, alice.path, "op-trace-01", "ref-01", false, []byte("traced"))
	up := wantUploaded(t, answer, 6, 0)
	id := answer.header.Get(httpx.CorrelationHeader)
	for _, stage := range []string{"op-trace-01/reserve", "op-trace-01/complete"} {
		var got string
		if err := m.pool.QueryRow(context.Background(), "SELECT correlation_id FROM media_operations WHERE tenant_id = $1 AND operation_id = $2",
			alice.tenantID, stage).Scan(&got); err != nil || got != id {
			t.Fatalf("%s correlation = %q, %v; want %q", stage, got, err, id)
		}
	}
	attach := m.attach(alice.signedIn, alice.path, "op-trace-02", up.AssetID, "ref-02")
	strictBody[attachJSON](t, attach, http.StatusOK)
	detach := m.detach(alice.signedIn, alice.path, "op-trace-03", "ref-02")
	strictBody[detachJSON](t, detach, http.StatusOK)
	for op, s := range map[string]sent{"op-trace-02": attach, "op-trace-03": detach} {
		var got string
		if err := m.pool.QueryRow(context.Background(), "SELECT correlation_id FROM media_operations WHERE tenant_id = $1 AND operation_id = $2",
			alice.tenantID, op).Scan(&got); err != nil || got != s.header.Get(httpx.CorrelationHeader) {
			t.Fatalf("%s correlation = %q, %v", op, got, err)
		}
	}

	pending, err := postgres.NewOutboxStore(m.pool).Pending(context.Background(), canvas.Key{TenantID: alice.tenantID, CanvasID: alice.canvasID}, postgres.MaxOutboxPage)
	if err != nil {
		t.Fatal(err)
	}
	var correlations []string
	for _, record := range pending {
		if record.Usage != nil {
			correlations = append(correlations, record.Event.CorrelationID)
		}
	}
	// Reserve and Complete change the usage; Attach and Detach of a second
	// reference do not.
	if !slices.Equal(correlations, []string{id, id}) {
		t.Fatalf("media usage record correlations = %v, want two of %s", correlations, id)
	}

	line := logLine(m.http.logs.String(), "media upload confirmed", id)
	if line == nil {
		t.Fatalf("no confirmation log for %s: %s", id, m.http.logs.String())
	}
	for field, want := range map[string]any{
		"tenant_id": alice.tenantID, "canvas_id": alice.canvasID, "asset_id": up.AssetID,
		"used_bytes": float64(6), "reserved_bytes": float64(0), "quota_bytes": float64(media.QuotaBytes),
	} {
		if line[field] != want {
			t.Fatalf("log %s = %v, want %v: %v", field, line[field], want, line)
		}
	}
	for _, leaked := range []string{"ref-01", "traced", "assets/"} {
		if strings.Contains(m.http.logs.String(), leaked) {
			t.Fatalf("logs contain %q", leaked)
		}
	}
}
