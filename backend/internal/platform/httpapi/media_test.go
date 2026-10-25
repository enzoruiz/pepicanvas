package httpapi_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpapi"
)

// Without a media store every media route answers media_unavailable after
// the canvas check, and a caller without access still gets forbidden.
func TestMediaRoutesWithoutAMediaStoreAreUnavailable(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	op := e.operator("op-http-media-op01", "operator@example.com")
	alice := e.streamer(op, "alice1", "alice@example.com")
	bob := e.streamer(op, "bob001", "bob@example.com")
	upload := func(s streamer, path string) *http.Response {
		return e.do(http.MethodPost, path+"/media/uploads?operation_id=op-1&reference_id=ref-1", nil,
			withSession(s.token), withCSRF(s.csrf), withBody("application/octet-stream", "data"))
	}
	routes := map[string]func(s streamer, path string) *http.Response{
		"usage": func(s streamer, path string) *http.Response {
			return e.do(http.MethodGet, path+"/media/usage", nil, withSession(s.token))
		},
		"upload": upload,
		"attach": func(s streamer, path string) *http.Response {
			return e.do(http.MethodPost, path+"/media/references", map[string]string{"operation_id": "op-2", "asset_id": "a", "reference_id": "r"},
				withSession(s.token), withCSRF(s.csrf))
		},
		"detach": func(s streamer, path string) *http.Response {
			return e.do(http.MethodPost, path+"/media/references/r/detach", map[string]string{"operation_id": "op-3"},
				withSession(s.token), withCSRF(s.csrf))
		},
	}
	for name, route := range routes {
		t.Run(name, func(t *testing.T) {
			expectError(t, route(alice, alice.path), http.StatusServiceUnavailable, "media_unavailable")
			expectError(t, route(bob, alice.path), http.StatusForbidden, "forbidden")
			expectError(t, e.do(http.MethodGet, alice.path+"/media/usage", nil), http.StatusUnauthorized, "unauthorized")
		})
	}
}

func TestConfigurationRequiresTheUploadLimit(t *testing.T) {
	e := newEnv(t, httpapi.DefaultLimits)
	noLimit := e.config
	noLimit.Limits.UploadsPerAccount = httpapi.Rate{}
	noWindow := e.config
	noWindow.Limits.UploadsPerAccount = httpapi.Rate{Limit: 60}
	for name, config := range map[string]httpapi.Config{"no limit": noLimit, "no window": noWindow} {
		if _, err := httpapi.New(config); err == nil {
			t.Errorf("%s: configuration accepted", name)
		}
	}
	if httpapi.DefaultLimits.UploadsPerAccount != (httpapi.Rate{Limit: 60, Window: time.Hour}) {
		t.Fatalf("UploadsPerAccount = %+v, want 60 per hour", httpapi.DefaultLimits.UploadsPerAccount)
	}
}
