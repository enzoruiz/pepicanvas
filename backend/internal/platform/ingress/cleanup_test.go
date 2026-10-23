package ingress_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpx"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/ingress"
)

func TestRejectedConnectionsLeaveNoGoroutines(t *testing.T) {
	baseline := runtime.NumGoroutine()
	in, err := ingress.New(ingress.Config{AllowedOrigins: []string{origin}, Limits: generous})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	mux := http.NewServeMux()
	mux.Handle("POST /api/open", in.Route(ingress.Anonymous, handler))
	mux.Handle("GET /api/socket", in.Upgrade(handler))
	server := httptest.NewServer(httpx.Correlate(in.Protect(mux)))
	transport := &http.Transport{}
	client := &http.Client{Transport: transport}

	for i := range 50 {
		var r *http.Request
		if i%2 == 0 {
			r, _ = http.NewRequest(http.MethodGet, server.URL+"/api/socket", nil)
			r.Header.Set("Connection", "Upgrade")
			r.Header.Set("Upgrade", "websocket")
			r.Header.Set("Sec-WebSocket-Version", "13")
			r.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		} else {
			r, _ = http.NewRequest(http.MethodPost, server.URL+"/api/open", strings.NewReader("{}"))
		}
		r.Header.Set("Origin", "https://evil.test")
		res, err := client.Do(r)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("request %d: status = %d, want 403", i, res.StatusCode)
		}
	}
	transport.CloseIdleConnections()
	server.Close()
	if calls.Load() != 0 {
		t.Fatalf("handler ran %d times, want 0", calls.Load())
	}

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			t.Fatalf("goroutines = %d, baseline %d:\n%s", runtime.NumGoroutine(), baseline, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(10 * time.Millisecond)
	}
}
