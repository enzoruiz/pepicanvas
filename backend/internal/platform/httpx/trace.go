package httpx

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/enzoruiz/pepicanvas/backend/internal/platform/telemetry"
)

// TraceparentHeader carries the request's root span back to the client.
const TraceparentHeader = "traceparent"

// unmatchedRoute names requests that no registered pattern served.
const unmatchedRoute = "unmatched"

// errPanic marks a span whose handler panicked; its value is never exported.
var (
	errPanic  = errors.New("handler panic")
	errServer = errors.New("server error status")
)

// Trace starts the http.request root span of each request and exports it
// through tracer; a nil tracer still starts spans without exporting them. It
// must run inside Correlate, so the trace ID is the request's correlation ID.
// Inbound traceparent headers are ignored: traces are server-issued
// (CTRL-TRACE-01). The span records the method, the registered ServeMux
// pattern or "unmatched", and the written status.
func Trace(tracer *telemetry.Tracer) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, span := telemetry.Start(telemetry.WithTracer(r.Context(), tracer), telemetry.SpanHTTPRequest,
				telemetry.Attr("method", r.Method))
			if sc, ok := telemetry.SpanContextFrom(ctx); ok {
				w.Header().Set(TraceparentHeader, sc.Traceparent())
			}
			sw := &statusWriter{ResponseWriter: w}
			// The mux records its pattern on the request it is given, which
			// the inner middleware pass down unchanged.
			req := r.WithContext(ctx)
			defer func() {
				v := recover()
				span.SetAttr("route", route(req.Pattern, sw))
				status := sw.status
				if status == 0 {
					status = http.StatusOK
					if v != nil {
						status = http.StatusInternalServerError
					}
				}
				span.SetAttr("status", strconv.Itoa(status))
				if v != nil {
					span.End(errPanic)
					panic(v)
				}
				if status >= http.StatusInternalServerError {
					span.End(errServer)
					return
				}
				span.End(nil)
			}()
			next.ServeHTTP(sw, req)
		})
	}
}

// route returns pattern, or unmatched when no pattern served the request.
// For CONNECT the mux's redirect handler reports its target path as the
// pattern, so a redirect to that path is treated as unmatched to keep request
// paths out of spans.
func route(pattern string, w *statusWriter) string {
	if pattern == "" {
		return unmatchedRoute
	}
	if w.status >= 300 && w.status < 400 {
		if location, err := url.Parse(w.Header().Get("Location")); err == nil && location.Path == pattern {
			return unmatchedRoute
		}
	}
	return pattern
}

// statusWriter records the first final written status.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 && status >= http.StatusOK {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
