package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/color/palette"
	"image/gif"
	"image/jpeg"
	"image/png"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/media"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpapi"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpx"
)

// pngOf, jpegOf, and gifOf encode real images, so the serving route can sniff
// their type; webpOf is the RIFF/WEBP signature a sniffer recognizes, which
// the fake inspector accepts like any other bytes.
func pngOf(t *testing.T, width, height int) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := png.Encode(&out, image.NewRGBA(image.Rect(0, 0, width, height))); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func jpegOf(t *testing.T, width, height int) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := jpeg.Encode(&out, image.NewRGBA(image.Rect(0, 0, width, height)), nil); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func gifOf(t *testing.T, width, height, frames int) []byte {
	t.Helper()
	animation := &gif.GIF{}
	for i := range frames {
		frame := image.NewPaletted(image.Rect(0, 0, width, height), palette.Plan9)
		frame.Set(i%width, 0, color.White)
		animation.Image = append(animation.Image, frame)
		animation.Delay = append(animation.Delay, 10)
	}
	var out bytes.Buffer
	if err := gif.EncodeAll(&out, animation); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func webpOf() []byte {
	return append([]byte("RIFF\x1a\x00\x00\x00WEBPVP8 "), bytes.Repeat([]byte{0x2a}, 18)...)
}

// inspectAs arms the fake inspector to report every upload as kind with the
// content-derived mediaType and a 64x48 size.
func inspectAs(kind media.Kind, mediaType string) func(*mediaFaults) {
	return func(f *mediaFaults) {
		f.inspect = func(_ context.Context, n int64) (media.Inspection, error) {
			inspection := media.Inspection{Kind: kind, MediaType: mediaType, InputBytes: n, StreamCount: 1}
			switch kind {
			case media.KindGIF:
				inspection.Duration = time.Second
				inspection.GIFFrameCount = 2
				inspection.GIFFrameDimensions = []media.Dimensions{{Width: 64, Height: 48}, {Width: 64, Height: 48}}
			case media.KindAudio:
				inspection.Duration = time.Second
			case media.KindVideo:
				inspection.Duration = time.Second
				inspection.Dimensions = media.Dimensions{Width: 64, Height: 48}
			default:
				inspection.Dimensions = media.Dimensions{Width: 64, Height: 48}
			}
			return inspection, nil
		}
	}
}

// uploadAs sends data as a non-replacing upload with contentType.
func (m *mediaEnv) uploadAs(s signedIn, path, op, ref, contentType string, data []byte, options ...option) sent {
	return m.serveRaw(context.Background(), http.MethodPost, path+"/media/uploads?"+uploadQuery(op, ref, false), contentType,
		bytes.NewReader(data), int64(len(data)), append(authOf(s), options...)...)
}

func contentPath(path, ref string) string {
	return path + "/media/references/" + ref + "/content"
}

// content reads a reference's bytes through the serving route.
func (m *mediaEnv) content(s signedIn, path, ref string, options ...option) sent {
	return m.serveRaw(context.Background(), http.MethodGet, contentPath(path, ref), "", nil, 0, append([]option{withSession(s.token)}, options...)...)
}

// wantContent checks a served reference: exactly data, its real type and
// length, the private revalidation policy, and the asset ID as the ETag.
func wantContent(t *testing.T, s sent, mediaType, assetID string, data []byte) {
	t.Helper()
	if s.status != http.StatusOK || s.body != string(data) {
		t.Fatalf("content = %d with %d bytes, want 200 with the %d bytes uploaded: %.200s", s.status, len(s.body), len(data), s.body)
	}
	for header, want := range map[string]string{
		"Content-Type":           mediaType,
		"Content-Length":         strconv.Itoa(len(data)),
		"Cache-Control":          "private, no-cache",
		"ETag":                   `"` + assetID + `"`,
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := s.header.Get(header); got != want {
			t.Fatalf("%s = %q, want %q", header, got, want)
		}
	}
}

type objectJSON struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Content string `json:"content"`
	X       int    `json:"x"`
	Y       int    `json:"y"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
}

// objects reads the canvas objects through the canvas route.
func (m *mediaEnv) objects(s streamer) []objectJSON {
	m.t.Helper()
	answer := m.serveRaw(context.Background(), http.MethodGet, s.path, "", nil, 0, withSession(s.token))
	if answer.status != http.StatusOK {
		m.t.Fatalf("GET %s = %d %s", s.path, answer.status, answer.body)
	}
	var body struct {
		Objects []objectJSON `json:"objects"`
	}
	if err := json.Unmarshal([]byte(answer.body), &body); err != nil {
		m.t.Fatal(err)
	}
	return body.Objects
}

// S1, S6, S8: each of the four image types may be declared; the upload
// creates the image object, visible through the canvas route under the asset
// ID with the reference as its content, and the reference serves the bytes
// with their real type. SVG or another image type is rejected as an
// unsupported kind, and any other type as a bad request, before anything is
// reserved or read. A declared type that differs from the content is a type
// mismatch that leaves nothing behind and repeats on retry.
func TestMediaUploadDeclaredImageTypes(t *testing.T) {
	m := newMediaEnv(t, httpapi.DefaultLimits)
	alice := m.streamer(m.op, "alice1", "alice@example.com")
	var used int64
	for i, tc := range []struct {
		contentType string
		kind        media.Kind
		data        []byte
	}{
		{"image/png", media.KindImage, pngOf(t, 64, 48)},
		{"image/jpeg", media.KindImage, jpegOf(t, 64, 48)},
		{"image/webp", media.KindImage, webpOf()},
		{"image/gif", media.KindGIF, gifOf(t, 64, 48, 2)},
	} {
		t.Run(tc.contentType, func(t *testing.T) {
			m.faults.set(inspectAs(tc.kind, tc.contentType))
			defer m.faults.reset()
			ref := fmt.Sprintf("ref-type-%d", i)
			used += int64(len(tc.data))
			got := strictBody[uploadJSON](t, m.uploadAs(alice.signedIn, alice.path, fmt.Sprintf("op-type-%d", i), ref, tc.contentType, tc.data), http.StatusCreated)
			if got.AssetID == "" || got.ObjectID != got.AssetID || got.Kind != string(tc.kind) || got.Usage != usageFor(used, 0) {
				t.Fatalf("upload = %+v, want a %s asset that is its own object, usage %+v", got, tc.kind, usageFor(used, 0))
			}
			var object *objectJSON
			for _, o := range m.objects(alice) {
				if o.ID == got.ObjectID {
					object = &o
				}
			}
			if object == nil || object.Kind != "image" || object.Content != ref || object.Width != 64 || object.Height != 48 {
				t.Fatalf("canvas object %s = %+v, want an image of %s at 64x48", got.ObjectID, object, ref)
			}
			wantContent(t, m.content(alice.signedIn, alice.path, ref), tc.contentType, got.AssetID, tc.data)
		})
	}
	m.wantBucket()
	objects := len(m.objects(alice))
	operations := m.count("SELECT count(*) FROM media_operations")

	refused := []struct {
		contentType string
		status      int
		code        string
		reason      string
	}{
		{"image/svg+xml", http.StatusUnprocessableEntity, "media_rejected", "unsupported_kind"},
		{"image/svg+xml; charset=utf-8", http.StatusUnprocessableEntity, "media_rejected", "unsupported_kind"},
		{"image/bmp", http.StatusUnprocessableEntity, "media_rejected", "unsupported_kind"},
		{"image/jpg", http.StatusUnprocessableEntity, "media_rejected", "unsupported_kind"},
		{"text/plain", http.StatusBadRequest, "bad_request", ""},
		{"video/mp4", http.StatusBadRequest, "bad_request", ""},
		{"image", http.StatusBadRequest, "bad_request", ""},
	}
	for _, tc := range refused {
		t.Run("refused "+tc.contentType, func(t *testing.T) {
			answer := m.serveRaw(context.Background(), http.MethodPost, alice.path+"/media/uploads?"+uploadQuery("op-type-refused", "ref-refused", false),
				tc.contentType, unreadable{t}, 100, authOf(alice.signedIn)...)
			wantMediaError(t, answer, tc.status, tc.code, tc.reason)
			m.wantUsage(alice, used, 0)
			m.wantBucket()
			if got := m.count("SELECT count(*) FROM media_operations"); got != operations {
				t.Fatalf("media operations %d -> %d", operations, got)
			}
		})
	}

	t.Run("declared type differs from the content", func(t *testing.T) {
		m.faults.set(inspectAs(media.KindImage, "image/jpeg"))
		defer m.faults.reset()
		data := pngOf(t, 32, 32)
		first := m.uploadAs(alice.signedIn, alice.path, "op-type-mismatch", "ref-mismatch", "image/png", data)
		wantMediaError(t, first, http.StatusUnprocessableEntity, "media_rejected", "type_mismatch")
		if strings.Contains(first.body, "jpeg") {
			t.Fatalf("the rejection names the inspected type: %s", first.body)
		}
		m.wantUsage(alice, used, 0)
		m.wantBucket()
		if got := len(m.objects(alice)); got != objects {
			t.Fatalf("canvas objects %d -> %d", objects, got)
		}
		wantMediaError(t, m.content(alice.signedIn, alice.path, "ref-mismatch"), http.StatusNotFound, "not_found", "")
		// The retry repeats the answer without a second reservation.
		m.faults.reset()
		wantMediaError(t, m.uploadAs(alice.signedIn, alice.path, "op-type-mismatch", "ref-mismatch", "image/png", data), http.StatusUnprocessableEntity, "media_rejected", "type_mismatch")
		m.wantUsage(alice, used, 0)
		m.wantBucket()
		if got := len(m.objects(alice)); got != objects {
			t.Fatalf("canvas objects after the retry %d -> %d", objects, got)
		}
	})

	t.Run("undeclared upload keeps the content kind", func(t *testing.T) {
		m.faults.set(inspectAs(media.KindGIF, "image/gif"))
		defer m.faults.reset()
		data := gifOf(t, 64, 48, 3)
		got := strictBody[uploadJSON](t, m.uploadAs(alice.signedIn, alice.path, "op-type-octet", "ref-octet", "application/octet-stream", data), http.StatusCreated)
		if got.Kind != "gif" || got.ObjectID != got.AssetID {
			t.Fatalf("undeclared upload = %+v", got)
		}
		wantContent(t, m.content(alice.signedIn, alice.path, "ref-octet"), "image/gif", got.AssetID, data)
	})
}

// S2: a declared image of exactly 10 000 000 bytes reaches the inspection and
// is accepted; one more byte is rejected at the route as too large, before
// anything is reserved or read. An undeclared upload is not held to the
// image limit at the route, because its kind is not known yet.
func TestMediaDeclaredImageByteLimit(t *testing.T) {
	m := newMediaEnv(t, httpapi.DefaultLimits)
	alice := m.streamer(m.op, "alice1", "alice@example.com")
	limit := media.DefaultLimits().MaxImageBytes
	if limit != 10_000_000 {
		t.Fatalf("MaxImageBytes = %d, want 10 000 000", limit)
	}

	m.faults.set(inspectAs(media.KindImage, "image/png"))
	wantUploaded(t, m.uploadAs(alice.signedIn, alice.path, "op-limit-01", "ref-01", "image/png", randomBytes(t, int(limit))), limit, 0)
	m.faults.reset()
	operations := m.count("SELECT count(*) FROM media_operations")
	for _, contentType := range []string{"image/png", "image/jpeg", "image/webp", "image/gif"} {
		t.Run(contentType, func(t *testing.T) {
			answer := m.serveRaw(context.Background(), http.MethodPost, alice.path+"/media/uploads?"+uploadQuery("op-limit-02", "ref-02", false),
				contentType, unreadable{t}, limit+1, authOf(alice.signedIn)...)
			wantMediaError(t, answer, http.StatusUnprocessableEntity, "media_rejected", "input_too_large")
			m.wantUsage(alice, limit, 0)
			m.wantBucket()
			if got := m.count("SELECT count(*) FROM media_operations"); got != operations {
				t.Fatalf("media operations %d -> %d", operations, got)
			}
		})
	}

	m.faults.set(inspectAs(media.KindVideo, "video/mp4"))
	defer m.faults.reset()
	got := strictBody[uploadJSON](t, m.uploadAs(alice.signedIn, alice.path, "op-limit-03", "ref-03", "application/octet-stream", randomBytes(t, int(limit)+1)), http.StatusCreated)
	if got.Kind != "video" || got.ObjectID != "" || got.Usage != usageFor(2*limit+1, 0) {
		t.Fatalf("undeclared upload over the image limit = %+v", got)
	}
	m.wantBucket()
}

// S10: the reference's bytes are served to the owner and a moderator of the
// canvas only. Another streamer and its moderator get the canvas route's
// forbidden on the path of the canvas, and a reference of one tenant is not
// found from the canvas of another, which serves only its own bytes under the
// same reference ID. Neither the answer nor the logs hold a storage key.
func TestMediaServesImagesToAuthorizedViewersOnly(t *testing.T) {
	m := newMediaEnv(t, httpapi.DefaultLimits)
	alice := m.streamer(m.op, "alice1", "alice@example.com")
	bob := m.streamer(m.op, "bob001", "bob@example.com")
	_, aliceMod := m.moderatorOf(alice, "alicem", "alicemod@example.com")
	_, bobMod := m.moderatorOf(bob, "bobmod", "bobmod@example.com")
	const ref = "ref-hero-5f3a"

	aliceData := pngOf(t, 40, 30)
	aliceUp := wantUploaded(t, m.upload(alice.signedIn, alice.path, "op-serve-01", ref, false, aliceData), int64(len(aliceData)), 0)
	onlyAlice := wantUploaded(t, m.upload(alice.signedIn, alice.path, "op-serve-02", "ref-only-alice", false, pngOf(t, 8, 8)), int64(len(aliceData)+len(pngOf(t, 8, 8))), 0)
	m.faults.set(inspectAs(media.KindGIF, "image/gif"))
	bobData := gifOf(t, 64, 48, 2)
	bobUp := strictBody[uploadJSON](t, m.uploadAs(bob.signedIn, bob.path, "op-serve-03", ref, "image/gif", bobData), http.StatusCreated)
	m.faults.reset()

	served := m.content(alice.signedIn, alice.path, ref)
	wantContent(t, served, "image/png", aliceUp.AssetID, aliceData)
	wantContent(t, m.content(aliceMod, alice.path, ref), "image/png", aliceUp.AssetID, aliceData)
	wantContent(t, m.content(bob.signedIn, bob.path, ref), "image/gif", bobUp.AssetID, bobData)
	wantContent(t, m.content(bobMod, bob.path, ref), "image/gif", bobUp.AssetID, bobData)

	t.Run("revalidation", func(t *testing.T) {
		etag := `"` + aliceUp.AssetID + `"`
		for _, match := range []string{etag, "W/" + etag, `"other", ` + etag, "*"} {
			answer := m.content(alice.signedIn, alice.path, ref, func(r *http.Request) { r.Header.Set("If-None-Match", match) })
			if answer.status != http.StatusNotModified || answer.body != "" || answer.header.Get("ETag") != etag ||
				answer.header.Get("Cache-Control") != "private, no-cache" {
				t.Fatalf("If-None-Match %s = %d %q %v, want 304 with the ETag", match, answer.status, answer.body, answer.header)
			}
		}
		other := m.content(alice.signedIn, alice.path, ref, func(r *http.Request) { r.Header.Set("If-None-Match", `"`+bobUp.AssetID+`"`) })
		wantContent(t, other, "image/png", aliceUp.AssetID, aliceData)
	})

	t.Run("HEAD", func(t *testing.T) {
		answer := m.serveRaw(context.Background(), http.MethodHead, contentPath(alice.path, ref), "", nil, 0, withSession(alice.token))
		if answer.status != http.StatusOK || answer.body != "" || answer.header.Get("Content-Length") != strconv.Itoa(len(aliceData)) ||
			answer.header.Get("Content-Type") != "image/png" || answer.header.Get("ETag") != `"`+aliceUp.AssetID+`"` {
			t.Fatalf("HEAD = %d %q %v", answer.status, answer.body, answer.header)
		}
	})

	t.Run("other tenant", func(t *testing.T) {
		for name, s := range map[string]signedIn{"other owner": bob.signedIn, "other moderator": bobMod} {
			t.Run(name, func(t *testing.T) {
				wantMediaError(t, m.content(s, alice.path, ref), http.StatusForbidden, "forbidden", "")
				wantMediaError(t, m.content(s, alice.path, "ref-only-alice"), http.StatusForbidden, "forbidden", "")
				wantMediaError(t, m.content(s, alice.path, "no-such-ref"), http.StatusForbidden, "forbidden", "")
				// The other tenant's own canvas does not reach alice's reference.
				wantMediaError(t, m.content(s, bob.path, "ref-only-alice"), http.StatusNotFound, "not_found", "")
			})
		}
		anonymous := m.serveRaw(context.Background(), http.MethodGet, contentPath(alice.path, "ref-only-alice"), "", nil, 0)
		wantMediaError(t, anonymous, http.StatusUnauthorized, "unauthorized", "")
	})

	t.Run("missing", func(t *testing.T) {
		wantMediaError(t, m.content(alice.signedIn, alice.path, "no-such-ref"), http.StatusNotFound, "not_found", "")
		wantMediaError(t, m.content(alice.signedIn, alice.path, "a%20b"), http.StatusNotFound, "not_found", "")
		wantMediaError(t, m.content(alice.signedIn, alice.path, strings.Repeat("r", media.MaxIdentifierBytes+1)), http.StatusNotFound, "not_found", "")
		missing := "/api/tenants/" + alice.tenantID + "/canvases/no-such-canvas"
		wantMediaError(t, m.content(alice.signedIn, missing, ref), http.StatusForbidden, "forbidden", "")
		otherTenant := "/api/tenants/" + bob.tenantID + "/canvases/" + alice.canvasID
		wantMediaError(t, m.content(alice.signedIn, otherTenant, ref), http.StatusForbidden, "forbidden", "")
	})

	line := logLine(m.http.logs.String(), "media content served", served.header.Get(httpx.CorrelationHeader))
	if line == nil || line["tenant_id"] != alice.tenantID || line["canvas_id"] != alice.canvasID || line["asset_id"] != aliceUp.AssetID {
		t.Fatalf("serving log = %v", line)
	}
	logs := m.http.logs.String()
	rows, err := m.pool.Query(context.Background(), "SELECT storage_key FROM media_assets")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(logs, key) {
			t.Fatalf("logs contain the storage key %s", key)
		}
		for name, values := range served.header {
			if strings.Contains(strings.Join(values, " "), key) {
				t.Fatalf("header %s holds the storage key", name)
			}
		}
	}
	for _, leaked := range []string{ref, "ref-only-alice", onlyAlice.AssetID + "/"} {
		if strings.Contains(logs, leaked) {
			t.Fatalf("logs contain %q", leaked)
		}
	}
}

// S7, S13: only a ready image or GIF is served. A rejected, failed, or
// still-running upload, a detached reference, and audio are not found; a
// replaced reference serves the replacement under its own ETag; stored bytes
// that are not one of the four image types are an internal error, never
// served under a guessed type; and a server without media storage is
// unavailable.
func TestMediaServesOnlyReadyImages(t *testing.T) {
	m := newMediaEnv(t, httpapi.DefaultLimits)
	alice := m.streamer(m.op, "alice1", "alice@example.com")
	notFound := func(t *testing.T, ref string) {
		t.Helper()
		wantMediaError(t, m.content(alice.signedIn, alice.path, ref), http.StatusNotFound, "not_found", "")
	}

	// Each content-caused rejection reaches the client with its own reason
	// and nothing of the inspector's error.
	for i, code := range []media.RejectionCode{media.RejectionDecodeFailed, media.RejectionResourcesExceeded, media.RejectionTypeMismatch,
		media.RejectionInvalidMetadata, media.RejectionDimensionsExceeded, media.RejectionUnsupportedKind} {
		t.Run("rejected "+string(code), func(t *testing.T) {
			m.faults.set(func(f *mediaFaults) {
				f.inspect = func(context.Context, int64) (media.Inspection, error) {
					return media.Inspection{}, fmt.Errorf("ffmpeg stderr /tmp/stage-42: %w", media.Reject(code))
				}
			})
			defer m.faults.reset()
			ref := fmt.Sprintf("ref-rejected-%d", i)
			answer := m.upload(alice.signedIn, alice.path, fmt.Sprintf("op-ready-01-%d", i), ref, false, pngOf(t, 4, 4))
			wantMediaError(t, answer, http.StatusUnprocessableEntity, "media_rejected", string(code))
			notFound(t, ref)
			if strings.Contains(m.http.logs.String(), "stage-42") {
				t.Fatal("the logs hold the inspector's output")
			}
		})
	}
	t.Run("storage failed", func(t *testing.T) {
		m.faults.set(func(f *mediaFaults) { f.failPut = errors.New("disk full") })
		defer m.faults.reset()
		wantMediaError(t, m.upload(alice.signedIn, alice.path, "op-ready-02", "ref-failed", false, pngOf(t, 4, 4)), http.StatusBadGateway, "upload_failed", "")
		notFound(t, "ref-failed")
	})
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		m.faults.set(func(f *mediaFaults) {
			f.inspect = func(context.Context, int64) (media.Inspection, error) {
				cancel()
				return media.Inspection{}, context.Canceled
			}
		})
		defer m.faults.reset()
		data := pngOf(t, 4, 4)
		answer := m.uploadBody(ctx, alice.signedIn, alice.path, uploadQuery("op-ready-03", "ref-cancelled", false), bytes.NewReader(data), int64(len(data)))
		wantMediaError(t, answer, http.StatusBadGateway, "upload_failed", "")
		notFound(t, "ref-cancelled")
	})
	m.wantUsage(alice, 0, 0)
	m.wantBucket()

	t.Run("still inspecting", func(t *testing.T) {
		inspecting, release := make(chan struct{}), make(chan struct{})
		m.faults.set(func(f *mediaFaults) {
			f.inspect = func(_ context.Context, n int64) (media.Inspection, error) {
				close(inspecting)
				<-release
				return media.Inspection{Kind: media.KindImage, MediaType: "image/png", InputBytes: n, StreamCount: 1, Dimensions: media.Dimensions{Width: 4, Height: 4}}, nil
			}
		})
		defer m.faults.reset()
		data := pngOf(t, 4, 4)
		done := make(chan sent, 1)
		go func() { done <- m.uploadAs(alice.signedIn, alice.path, "op-ready-04", "ref-slow", "image/png", data) }()
		select {
		case <-inspecting:
		case <-time.After(time.Minute):
			t.Fatal("the upload never reached the inspection")
		}
		notFound(t, "ref-slow")
		close(release)
		up := strictBody[uploadJSON](t, <-done, http.StatusCreated)
		wantContent(t, m.content(alice.signedIn, alice.path, "ref-slow"), "image/png", up.AssetID, data)
	})

	t.Run("detached and replaced", func(t *testing.T) {
		first, second := pngOf(t, 5, 5), jpegOf(t, 6, 6)
		before := m.usage(alice).UsedBytes
		up := wantUploaded(t, m.upload(alice.signedIn, alice.path, "op-ready-05", "ref-hero", false, first), before+int64(len(first)), 0)
		wantContent(t, m.content(alice.signedIn, alice.path, "ref-hero"), "image/png", up.AssetID, first)
		replaced := wantUploaded(t, m.upload(alice.signedIn, alice.path, "op-ready-06", "ref-hero", true, second), before+int64(len(second)), 0)
		if replaced.ObjectID != "" {
			t.Fatalf("a replacement created object %s", replaced.ObjectID)
		}
		wantContent(t, m.content(alice.signedIn, alice.path, "ref-hero"), "image/jpeg", replaced.AssetID, second)
		strictBody[detachJSON](t, m.detach(alice.signedIn, alice.path, "op-ready-07", "ref-hero"), http.StatusOK)
		notFound(t, "ref-hero")
	})

	t.Run("audio", func(t *testing.T) {
		m.faults.set(inspectAs(media.KindAudio, "audio/mpeg"))
		defer m.faults.reset()
		strictBody[uploadJSON](t, m.upload(alice.signedIn, alice.path, "op-ready-08", "ref-audio", false, []byte("ID3 audio bytes")), http.StatusCreated)
		notFound(t, "ref-audio")
	})

	t.Run("stored bytes are not an image", func(t *testing.T) {
		noise := []byte("<svg xmlns='http://www.w3.org/2000/svg'/>")
		before := m.usage(alice).UsedBytes
		wantUploaded(t, m.upload(alice.signedIn, alice.path, "op-ready-09", "ref-noise", false, noise), before+int64(len(noise)), 0)
		answer := m.content(alice.signedIn, alice.path, "ref-noise")
		wantMediaError(t, answer, http.StatusInternalServerError, "internal", "")
		if strings.Contains(answer.body, "svg") {
			t.Fatalf("the answer reflects the stored bytes: %s", answer.body)
		}
	})

	t.Run("media unavailable", func(t *testing.T) {
		configure := m.configure
		defer func() {
			m.configure = configure
			m.serve(m.canvases)
		}()
		m.configure = func(c *httpapi.Config) {
			configure(c)
			c.Storage = nil
		}
		m.serve(m.canvases)
		wantMediaError(t, m.content(alice.signedIn, alice.path, "ref-slow"), http.StatusServiceUnavailable, "media_unavailable", "")
	})
}
