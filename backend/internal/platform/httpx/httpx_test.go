package httpx_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/enzoruiz/pepicanvas/backend/internal/platform/correlation"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpx"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/logging"
	"github.com/enzoruiz/pepicanvas/backend/internal/secret"
)

func decoyToken(t *testing.T) string {
	t.Helper()
	v, err := secret.Generate(secret.KindInvitationToken)
	if err != nil {
		t.Fatal(err)
	}
	return v.Reveal()
}

func assertPrivateHeaders(t *testing.T, h http.Header) {
	t.Helper()
	if got := h.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := h.Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q, want no-referrer", got)
	}
}

func TestPrivateSetsHeadersOnEveryResponsePath(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"implicit 200": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("private")) },
		"explicit 200": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) },
		"no body":      func(http.ResponseWriter, *http.Request) {},
		"override attempt": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Cache-Control", "public, max-age=3600")
			w.Header().Set("Referrer-Policy", "unsafe-url")
			_, _ = w.Write([]byte("private"))
		},
		"error": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Cache-Control", "max-age=60")
			http.Error(w, "nope", http.StatusForbidden)
		},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			httpx.Private(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/private", nil))
			assertPrivateHeaders(t, rec.Result().Header)
		})
	}
}

func TestCorrelateAssignsFreshIDAndIgnoresInboundHeader(t *testing.T) {
	inbound := decoyToken(t)
	var seen correlation.ID
	h := httpx.Correlate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := correlation.FromContext(r.Context())
		if !ok {
			t.Fatal("handler context carries no correlation ID")
		}
		seen = id
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(httpx.CorrelationHeader, inbound)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	got := rec.Result().Header.Get(httpx.CorrelationHeader)
	if got == inbound || got != seen.String() {
		t.Fatalf("response correlation = %q, context = %q, inbound = %q", got, seen, inbound)
	}
	if _, err := correlation.Parse(got); err != nil {
		t.Fatalf("response correlation %q is not canonical", got)
	}
}

func TestResponderErrorNeverReflectsSecrets(t *testing.T) {
	token := decoyToken(t)
	mediaURL := "https://bucket.s3.example.com/m.png?X-Amz-Signature=decoysig"
	var logs bytes.Buffer
	responder := httpx.Responder{Logger: slog.New(logging.NewHandler(slog.NewJSONHandler(&logs, nil)))}

	cases := []struct {
		code   httpx.ErrorCode
		status int
	}{
		{httpx.ErrBadRequest, http.StatusBadRequest},
		{httpx.ErrUnauthorized, http.StatusUnauthorized},
		{httpx.ErrForbidden, http.StatusForbidden},
		{httpx.ErrIdentityTaken, http.StatusConflict},
		{httpx.ErrNotFound, http.StatusNotFound},
		{httpx.ErrConflict, http.StatusConflict},
		{httpx.ErrRateLimited, http.StatusTooManyRequests},
		{httpx.ErrInternal, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		logs.Reset()
		h := httpx.Correlate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			err := fmt.Errorf("fetch %s for %s: %w", mediaURL, token, errors.New("boom"))
			responder.Error(w, r, tc.code, err)
		}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/invitations/"+token+"?code="+token, nil))
		res := rec.Result()

		if res.StatusCode != tc.status {
			t.Errorf("%s: status = %d, want %d", tc.code, res.StatusCode, tc.status)
		}
		if ct := res.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s: Content-Type = %q", tc.code, ct)
		}
		assertPrivateHeaders(t, res.Header)
		id := res.Header.Get(httpx.CorrelationHeader)
		want := fmt.Sprintf(`{"error":%q,"correlation_id":%q}`, tc.code, id)
		if body := strings.TrimSpace(rec.Body.String()); body != want {
			t.Errorf("%s: body = %s, want %s", tc.code, body, want)
		}
		for surface, text := range map[string]string{"body": rec.Body.String(), "headers": fmt.Sprint(res.Header), "logs": logs.String()} {
			if strings.Contains(text, token) || strings.Contains(text, "decoysig") {
				t.Errorf("%s: %s leaked a decoy: %s", tc.code, surface, text)
			}
		}
		var rec2 map[string]any
		if err := json.Unmarshal(logs.Bytes(), &rec2); err != nil {
			t.Fatalf("%s: log is not one JSON record: %q", tc.code, logs.String())
		}
		if rec2["correlation_id"] != id || rec2["error_code"] != string(tc.code) {
			t.Errorf("%s: log record = %v, want correlation %s", tc.code, rec2, id)
		}
	}
}

func TestResponderRejectsUnknownCodeAsInternal(t *testing.T) {
	var logs bytes.Buffer
	responder := httpx.Responder{Logger: slog.New(logging.NewHandler(slog.NewJSONHandler(&logs, nil)))}
	rec := httptest.NewRecorder()
	responder.Error(rec, httptest.NewRequest(http.MethodGet, "/", nil), httpx.ErrorCode("leak "+decoyToken(t)), errors.New("x"))
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), `"error":"internal"`) {
		t.Fatalf("unknown code produced %d %s", rec.Code, rec.Body.String())
	}
}

func TestRecoverAnswersGenericErrorWithoutPanicValue(t *testing.T) {
	token := decoyToken(t)
	var logs bytes.Buffer
	logger := slog.New(logging.NewHandler(slog.NewJSONHandler(&logs, nil)))
	h := httpx.Correlate(httpx.Recover(logger, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("invalid invitation " + token)
	})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), `"error":"internal"`) {
		t.Fatalf("panic produced %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String()+logs.String(), token) {
		t.Fatalf("panic value leaked: body=%s logs=%s", rec.Body.String(), logs.String())
	}
	if !strings.Contains(logs.String(), rec.Header().Get(httpx.CorrelationHeader)) {
		t.Fatalf("panic log lacks correlation: %s", logs.String())
	}
}

func TestRecoverRepanicsAbortHandler(t *testing.T) {
	h := httpx.Recover(slog.New(slog.DiscardHandler), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	defer func() {
		if recover() != http.ErrAbortHandler {
			t.Fatal("http.ErrAbortHandler must propagate")
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestPrivateSurvivesResponseControllerFlush(t *testing.T) {
	srv := httptest.NewServer(httpx.Private(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Header().Del("Referrer-Policy")
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush: %v", err)
		}
		_, _ = w.Write([]byte("private"))
	})))
	defer srv.Close()
	res, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	assertPrivateHeaders(t, res.Header)
}

func TestPrivateWriterSupportsFlusher(t *testing.T) {
	rec := httptest.NewRecorder()
	httpx.Private(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("private writer does not implement http.Flusher")
		}
		w.Header().Set("Cache-Control", "public")
		f.Flush()
	})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	assertPrivateHeaders(t, rec.Result().Header)
	if !rec.Flushed {
		t.Fatal("flush did not reach the underlying writer")
	}
}
