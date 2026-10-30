package httpapi_test

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/enzoruiz/pepicanvas/backend/internal/media"
)

// readyImageMedia answers one ready PNG asset for every reference and records
// the lookup in the trace.
type readyImageMedia struct {
	*actorMedia
	size int64
}

func (s readyImageMedia) ReadyReference(context.Context, media.CanvasKey, string) (media.Asset, error) {
	s.trace.add("reference")
	return media.Asset{ID: "asset-img", State: media.StateReady, Kind: media.KindImage, DeclaredBytes: s.size, StorageKey: "assets/internal-key", References: 1}, nil
}

// tracedImageStorage serves the same bytes for every key and records each
// open in the trace.
type tracedImageStorage struct {
	unusedStorage
	trace *trace
	data  []byte
}

func (s tracedImageStorage) Open(context.Context, string) (io.ReadCloser, error) {
	s.trace.add("open")
	return io.NopCloser(bytes.NewReader(s.data)), nil
}

// US-011 S10 with US-006 S8: the content route reauthorizes the membership
// after reading the asset and before the answer, so a moderator removed in
// between gets the route's forbidden with no bytes, type, or ETag, both for a
// full read and for a revalidation; the owner still reads the image.
func TestMediaContentReauthorizesBeforeDelivery(t *testing.T) {
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewGray(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	data := encoded.Bytes()
	r := newRevocationEnv(t)
	r.store = readyImageMedia{actorMedia: r.media, size: int64(len(data))}
	r.storage = tracedImageStorage{unusedStorage: unusedStorage{t}, trace: r.trace, data: data}
	path := revocationPath + "/media/references/ref-1/content"
	for name, c := range map[string]struct {
		header map[string]string
		status int
		events []string
	}{
		"read":         {status: http.StatusOK, events: []string{"membership", "reference", "open", "membership"}},
		"revalidation": {header: map[string]string{"If-None-Match": `"asset-img"`}, status: http.StatusNotModified, events: []string{"membership", "reference", "membership"}},
	} {
		t.Run(name, func(t *testing.T) {
			options := []option{withSession(r.owner.token)}
			for key, value := range c.header {
				options = append(options, func(req *http.Request) { req.Header.Set(key, value) })
			}
			r.serve(-1, "")
			res := serveContent(r, path, options...)
			if res.StatusCode != c.status {
				t.Fatalf("owner: status %d, want %d", res.StatusCode, c.status)
			}
			if got := r.trace.take(); !slices.Equal(got, c.events) {
				t.Fatalf("owner read = %v, want %v", got, c.events)
			}

			// The route check passes; the membership is gone at delivery.
			r.serve(1, "")
			options[0] = withSession(r.moderator.token)
			// The forbidden answer is an ordinary no-store error.
			res = r.do(http.MethodGet, path, nil, options...)
			body := r.bodies[len(r.bodies)-1]
			expectError(t, res, http.StatusForbidden, "forbidden")
			if strings.Contains(body, "\x89PNG") || res.Header.Get("ETag") != "" || strings.HasPrefix(res.Header.Get("Content-Type"), "image/") {
				t.Fatalf("forbidden answer carries the image: headers %v body %q", res.Header, body)
			}
			if got := r.trace.take(); !slices.Equal(got, c.events) {
				t.Fatalf("revoked read = %v, want %v", got, c.events)
			}
		})
	}
}

// serveContent sends a content request without the no-store assertion of
// env.do: an image answer revalidates with private, no-cache.
func serveContent(r *revocationEnv, path string, options ...option) *http.Response {
	r.t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = clientIP + ":40000"
	req.Header.Set("Origin", origin)
	for _, o := range options {
		o(req)
	}
	w := httptest.NewRecorder()
	r.handler.ServeHTTP(w, req)
	return w.Result()
}
